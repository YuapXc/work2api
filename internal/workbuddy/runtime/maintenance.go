package runtime

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

const (
	checkinRetryInterval = 2 * 3600.0
	checkinMaxRetry      = 3
	keepaliveFailLimit   = 3
)

type maintenanceState struct {
	r                                                    *Runtime
	ctx                                                  context.Context
	lastCredit                                           float64
	lastCheckinDate, checkinCountDate, lastKeepaliveDate string
	checkinFailCount                                     int
	checkinRetryAt                                       float64
	checkinAttemptedToday                                bool
	keepaliveFails                                       map[string]int
}

func (s *maintenanceState) context() context.Context { return s.ctx }
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
func (s *maintenanceState) setting(key, def string) string {
	settings, err := s.r.db.GetSettings()
	if err != nil {
		return def
	}
	if v, ok := settings[key]; ok && v != "" {
		return v
	}
	return def
}
func (s *maintenanceState) checkedInToday() map[string]bool {
	out := map[string]bool{}
	dates, err := s.r.db.CheckinDates()
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
func (s *maintenanceState) refreshCredits() {
	ctx := s.context()
	for _, acc := range s.r.Pool.Accounts() { // snapshot: admin may add/remove concurrently
		s.r.RefreshCreditsFor(ctx, s.r.db, acc)
	}
}

// doCheckin runs daily checkin for accounts not already checked in today.
// Returns the UIDs that genuinely failed (excludes "already checked in").
func (s *maintenanceState) doCheckin() []string {
	ctx := s.context()
	skip := s.checkedInToday()
	t := today()
	var failed []string
	for _, acc := range s.r.Pool.Accounts() {
		if skip[acc.UID] {
			continue
		}
		mgr := s.r.Manager(acc.UID)
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
			// record only on success/already, so failures retry；
			// 同时追加历史（签到日历数据源），already 也记（幂等）。
			_ = s.r.db.RecordCheckin(acc.UID, t, "auto")
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
func (s *maintenanceState) doKeepalive() {
	if s.setting("keepalive_enabled", "1") != "1" {
		return
	}
	for _, acc := range s.r.Pool.Accounts() {
		if !acc.Enabled {
			continue
		}
		mgr := s.r.Manager(acc.UID)
		if mgr == nil {
			continue
		}
		if mgr.KeepaliveContext(s.context()) {
			delete(s.keepaliveFails, acc.UID)
			continue
		}
		if s.context().Err() != nil {
			return
		}
		s.keepaliveFails[acc.UID]++
		if s.keepaliveFails[acc.UID] >= keepaliveFailLimit {
			reason := "保活连续失败（session 可能失效），请重新扫码登录"
			log.Printf("token 保活 %s: 连续 %d 次失败，自动禁用", acc.UID, s.keepaliveFails[acc.UID])
			s.r.Pool.SetEnabled(acc.UID, false, reason)
			_ = s.r.db.SetAccountState(acc.UID, map[string]any{"enabled": 0, "disabled_reason": reason})
			s.keepaliveFails[acc.UID] = 0 // reset; wait for user re-login
		} else {
			log.Printf("token 保活 %s: 失败 %d/%d（暂不禁用）", acc.UID, s.keepaliveFails[acc.UID], keepaliveFailLimit)
		}
	}
}

// cleanupUsage removes usage_logs older than the configured retention, then
// enforces the row-count cap (if any). Runs daily at 04:00.
func (r *Runtime) Warm(ctx context.Context) {
	r.MaintenanceMu.Lock()
	defer r.MaintenanceMu.Unlock()
	s := r.maintenance
	s.ctx = ctx
	s.refreshCredits()
	s.lastCredit = float64(time.Now().Unix())
}
func (r *Runtime) Maintain(ctx context.Context, settings map[string]string, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	r.MaintenanceMu.Lock()
	defer r.MaintenanceMu.Unlock()
	s := r.maintenance
	s.ctx = ctx
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

	r.Catalog.ListContext(ctx)
	// --- periodic credit refresh ---
	interval := s.r.cfg.CreditRefreshMin
	if v, err := strconv.Atoi(s.setting("credit_refresh_min", strconv.Itoa(interval))); err == nil && v >= 1 {
		interval = v
	}
	if float64(now.Unix())-s.lastCredit >= float64(interval)*60 {
		s.refreshCredits()
		s.lastCredit = float64(now.Unix())
	}

}
