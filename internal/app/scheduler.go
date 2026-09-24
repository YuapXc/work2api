package app

// Scheduler is the background daemon ported from workbuddy_one/scheduler.py.
// Single-user scope: daily checkin (with catch-up + retry), periodic credit
// refresh, daily token keepalive (fail-threshold disable), daily model refresh
// and cache-TTL warming, and daily usage-log cleanup. All timing is read from
// the DB settings on every tick, so changes take effect without a restart.
//
// Deferred (per project decisions): AA benchmarks, weekly full backup, credit
// webhook alerts, attachment archiving, and the usage row-count cap.

import (
	"context"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"work2api/internal/workbuddy/billing"
	"work2api/internal/workbuddy/siterouting"
)

// checkin retry cadence: retry every 2h, at most 3 attempts before giving up
// for the day (avoids losing a day of credits to one network blip, while not
// hammering upstream).
const (
	checkinRetryInterval = 2 * 3600.0
	checkinMaxRetry      = 3
	keepaliveFailLimit   = 3
)

// Scheduler owns the periodic background tasks over an Orchestrator.
type Scheduler struct {
	o    *Orchestrator
	stop chan struct{}
	done chan struct{}

	lastCredit   float64
	lastRowCheck float64

	// checkin state (see scheduler.py _run for the invariants these preserve)
	lastCheckinDate       string
	checkinCountDate      string
	checkinFailCount      int
	checkinRetryAt        float64
	checkinAttemptedToday bool

	lastKeepaliveDate    string
	lastModelRefreshDate string
	lastCleanupDate      string
	keepaliveFails       map[string]int
}

// NewScheduler builds a scheduler bound to the orchestrator.
func NewScheduler(o *Orchestrator) *Scheduler {
	return &Scheduler{
		o:              o,
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
		keepaliveFails: map[string]int{},
	}
}

// Start launches the background loop. Warming (credits + models) happens as the
// loop's first action so startup never blocks and Stop() always completes.
func (s *Scheduler) Start() { go s.run() }

// Stop signals the loop to exit and waits (bounded) for it to finish, so a hung
// upstream call during a tick can never wedge process shutdown.
func (s *Scheduler) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
	}
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
	}
}

func (s *Scheduler) setting(key, def string) string {
	settings, err := s.o.db.GetSettings()
	if err != nil {
		return def
	}
	if v, ok := settings[key]; ok && v != "" {
		return v
	}
	return def
}

func parseHours(raw string) []int {
	seen := map[int]bool{}
	var out []int
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return []int{9, 21}
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return []int{9, 21}
	}
	sort.Ints(out)
	return out
}

func parseHour(raw string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 0 || n > 23 {
		return def
	}
	return n
}

func minInt(xs []int) int {
	m := xs[0]
	for _, x := range xs[1:] {
		if x < m {
			m = x
		}
	}
	return m
}

// today returns the local YYYY-MM-DD date string.
func today() string { return time.Now().Format("2006-01-02") }

// checkedInToday returns the set of account UIDs already checked in today (by
// local DB record).
func (s *Scheduler) checkedInToday() map[string]bool {
	out := map[string]bool{}
	dates, err := s.o.db.CheckinDates()
	if err != nil {
		return out
	}
	t := today()
	for uid, d := range dates {
		if d == t {
			out[uid] = true
		}
	}
	return out
}

// refreshCredits fetches credits for every account and persists them; accounts
// with remaining credit get their cooldown cleared (auto-thaw).
func (s *Scheduler) refreshCredits() {
	ctx := context.Background()
	for _, acc := range s.o.pool.Accounts() { // snapshot: admin may add/remove concurrently
		s.o.refreshCreditsFor(ctx, acc)
	}
}

// doCheckin runs daily checkin for accounts not already checked in today.
// Returns the UIDs that genuinely failed (excludes "already checked in").
func (s *Scheduler) doCheckin() []string {
	ctx := context.Background()
	skip := s.checkedInToday()
	t := today()
	var failed []string
	for _, acc := range s.o.pool.Accounts() {
		if skip[acc.UID] {
			continue
		}
		mgr := s.o.managers[acc.UID]
		if mgr == nil {
			continue
		}
		// 国际站 WorkBuddy 无签到活动，跳过（避免每日两次注定失败的请求与日志噪音）
		if acc.Provider == "workbuddy" && siterouting.ProfileSite(acc.Profile) == "international" {
			continue
		}
		res, err := billing.DailyCheckin(ctx, mgr)
		if err != nil {
			log.Printf("签到失败 %s: %v", acc.UID, err)
			failed = append(failed, acc.UID)
			continue
		}
		log.Printf("签到 %s: ok=%v already=%v %s", acc.UID, res.OK, res.Already, res.Message)
		if res.OK || res.Already {
			_ = s.o.db.SetCheckinDate(acc.UID, t) // record only on success/already, so failures retry
		} else {
			failed = append(failed, acc.UID)
		}
	}
	// checkin can change balances (rewards); refresh + auto-thaw afterwards.
	s.refreshCredits()
	return failed
}

// doKeepalive force-refreshes each enabled account's token. A single failure
// only increments a counter; only sustained failure (session likely dead)
// disables the account, so a network blip never silently benches an account.
func (s *Scheduler) doKeepalive() {
	if s.setting("keepalive_enabled", "1") != "1" {
		return
	}
	for _, acc := range s.o.pool.Accounts() {
		if !acc.Enabled {
			continue
		}
		mgr := s.o.managers[acc.UID]
		if mgr == nil {
			continue
		}
		if mgr.Keepalive() {
			delete(s.keepaliveFails, acc.UID)
			continue
		}
		s.keepaliveFails[acc.UID]++
		if s.keepaliveFails[acc.UID] >= keepaliveFailLimit {
			reason := "保活连续失败（session 可能失效），请重新扫码登录"
			log.Printf("token 保活 %s: 连续 %d 次失败，自动禁用", acc.UID, s.keepaliveFails[acc.UID])
			s.o.pool.SetEnabled(acc.UID, false, reason)
			_ = s.o.db.SetAccountState(acc.UID, map[string]any{"enabled": 0, "disabled_reason": reason})
			s.keepaliveFails[acc.UID] = 0 // reset; wait for user re-login
		} else {
			log.Printf("token 保活 %s: 失败 %d/%d（暂不禁用）", acc.UID, s.keepaliveFails[acc.UID], keepaliveFailLimit)
		}
	}
}

// cleanupUsage removes usage_logs older than the configured retention, then
// enforces the row-count cap (if any). Runs daily at 04:00.
func (s *Scheduler) cleanupUsage() {
	n, err := s.o.db.CleanupUsage(s.o.cfg.UsageRetentionDays)
	if err != nil {
		log.Printf("使用记录清理异常: %v", err)
	} else if n > 0 {
		log.Printf("使用记录清理完成（保留 %d 天，删除 %d 条）", s.o.cfg.UsageRetentionDays, n)
	}
	s.checkUsageRows()
}

// run is the once-per-minute scheduling loop. Each branch is idempotent per day
// and uses catch-up semantics (>= hour, not == hour) so a service that starts
// after a window still performs that day's task.
func (s *Scheduler) run() {
	defer close(s.done)
	// Warm credits + model catalog once so the WebUI/API has data immediately.
	// Bail early if shutdown races startup.
	select {
	case <-s.stop:
		return
	default:
	}
	s.refreshCredits()
	s.o.models.Refresh()

	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		s.tick()
		select {
		case <-s.stop:
			return
		case <-ticker.C:
		}
	}
}

func (s *Scheduler) tick() {
	now := time.Now()
	t := today()
	hours := parseHours(s.setting("checkin_hours", "9,21"))

	// --- daily checkin (catch-up + bounded retry) ---
	if now.Hour() >= minInt(hours) {
		if s.checkinCountDate != t { // cross-day reset of failure/retry state
			s.checkinCountDate = t
			s.checkinFailCount = 0
			s.checkinRetryAt = 0
			s.checkinAttemptedToday = false
		}
		if !s.checkinAttemptedToday {
			// still cooling down from a prior failed attempt?
			if s.checkinRetryAt == 0 || now.Unix() >= int64(s.checkinRetryAt) {
				failed := s.doCheckin()
				s.checkinAttemptedToday = true
				if len(failed) > 0 {
					s.checkinFailCount++
					if s.checkinFailCount >= checkinMaxRetry {
						log.Printf("签到连续失败 %d 次，放弃当天重试（失败账号 %v）", s.checkinFailCount, failed)
						s.lastCheckinDate = t
						s.checkinRetryAt = 0
					} else {
						s.checkinRetryAt = float64(now.Unix()) + checkinRetryInterval
						log.Printf("签到有失败账号，%d 小时后重试（第 %d/%d 次）", int(checkinRetryInterval)/3600, s.checkinFailCount, checkinMaxRetry)
					}
				} else {
					s.lastCheckinDate = t
					s.checkinFailCount = 0
					s.checkinRetryAt = 0
				}
			}
		} else if s.checkinRetryAt != 0 && now.Unix() >= int64(s.checkinRetryAt) {
			// retry time reached: clear the attempted flag so next tick retries
			s.checkinAttemptedToday = false
			s.checkinRetryAt = 0
		}
	}

	// --- daily token keepalive ---
	if now.Hour() == parseHour(s.setting("keepalive_hour", "22"), 22) && s.lastKeepaliveDate != t {
		s.doKeepalive()
		s.lastKeepaliveDate = t
	}

	// --- daily usage cleanup (04:00) ---
	if now.Hour() == 4 && s.lastCleanupDate != t {
		s.cleanupUsage()
		s.lastCleanupDate = t
	}

	// --- daily model refresh ---
	if now.Hour() == parseHour(s.setting("model_refresh_hour", "6"), 6) && s.lastModelRefreshDate != t {
		s.o.models.Refresh()
		s.lastModelRefreshDate = t
	}
	// TTL-expiry refresh happens off the request path: List() refreshes if stale.
	s.o.models.List()

	// --- periodic credit refresh ---
	interval := s.o.cfg.CreditRefreshMin
	if v, err := strconv.Atoi(s.setting("credit_refresh_min", strconv.Itoa(interval))); err == nil && v >= 1 {
		interval = v
	}
	if float64(now.Unix())-s.lastCredit >= float64(interval)*60 {
		s.refreshCredits()
		s.lastCredit = float64(now.Unix())
	}

	// --- periodic usage row-count guard ---
	// 天数保留一天判一次够用，但行数上限可能被一天内的高频调用冲破，所以按
	// UsageCheckIntervalMin 定期检查。检查很便宜：先读 MIN/MAX(id) 预判，真超才删。
	if s.o.cfg.UsageMaxRows > 0 {
		checkEvery := s.o.cfg.UsageCheckIntervalMin
		if checkEvery < 1 {
			checkEvery = 30
		}
		if float64(now.Unix())-s.lastRowCheck >= float64(checkEvery)*60 {
			s.checkUsageRows()
			s.lastRowCheck = float64(now.Unix())
		}
	}
}

// checkUsageRows trims usage_logs to UsageMaxRows when exceeded. Uses the cheap
// MIN/MAX(id) span as a "maybe over" precheck to avoid a COUNT(*)/DELETE on the
// common (under-cap) path.
func (s *Scheduler) checkUsageRows() {
	max := s.o.cfg.UsageMaxRows
	if max <= 0 {
		return
	}
	lo, hi, err := s.o.db.UsageRowSpan()
	if err != nil || hi <= 0 || (hi-lo+1) <= int64(max) {
		return
	}
	n, err := s.o.db.CleanupUsageRows(max)
	if err != nil {
		log.Printf("使用记录行数检查异常: %v", err)
		return
	}
	if n > 0 {
		log.Printf("使用记录超出上限 %d 行，已截断最旧 %d 条", max, n)
	}
}
