package runtime

import (
	"context"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"work2api/internal/statebackup"
	"work2api/internal/workbuddy/credentials"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/siterouting"
)

const (
	checkinMaxRetry       = 3
	creditRecheckInterval = time.Minute
	creditRecheckTimeout  = 20 * time.Second
	creditRecheckBacklog  = 32
)

type creditRecheck struct {
	account   *pool.Account
	manager   *credentials.Manager
	requested float64
	revision  uint64
}

type creditRecheckQueue struct {
	mu      sync.Mutex
	pending []creditRecheck
	recent  map[string]creditRecheck
	cancel  context.CancelFunc
	done    chan struct{}
	closed  bool
}

func (r *Runtime) queueCreditRecheck(acc *pool.Account, mgr *credentials.Manager, revision uint64) {
	if revision == 0 || mgr == nil || r.db == nil || r.Manager(acc.UID) != mgr {
		return
	}
	now := nowSec()
	q := &r.creditRechecks
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed || len(q.pending) >= creditRecheckBacklog {
		return
	}
	for uid, last := range q.recent {
		if now-last.requested >= creditRecheckInterval.Seconds() {
			delete(q.recent, uid)
		}
	}
	if last := q.recent[acc.UID]; last.manager == mgr {
		return
	}
	if q.recent == nil {
		q.recent = make(map[string]creditRecheck)
	}
	job := creditRecheck{account: acc, manager: mgr, requested: now, revision: revision}
	q.recent[acc.UID] = job
	q.pending = append(q.pending, job)
	if q.done == nil {
		ctx, cancel := context.WithCancel(context.Background())
		q.cancel, q.done = cancel, make(chan struct{})
		go r.runCreditRechecks(ctx)
	}
}

func (r *Runtime) runCreditRechecks(ctx context.Context) {
	q := &r.creditRechecks
	for {
		q.mu.Lock()
		if q.closed || len(q.pending) == 0 {
			q.cancel()
			close(q.done)
			q.cancel, q.done = nil, nil
			q.mu.Unlock()
			return
		}
		job := q.pending[0]
		q.pending[0] = creditRecheck{}
		q.pending = q.pending[1:]
		q.mu.Unlock()

		if nowSec()-job.requested >= creditRecheckInterval.Seconds() {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, creditRecheckTimeout)
		leave := statebackup.Enter()
		r.recheckCredits(probeCtx, job)
		leave()
		cancel()
	}
}

func (r *Runtime) recheckCredits(ctx context.Context, job creditRecheck) {
	uid := job.account.UID
	if ctx.Err() != nil || r.Manager(uid) != job.manager {
		return
	}
	if ran := r.refreshCreditsForManager(ctx, r.db, job.account, job.manager, job.revision); ran || ctx.Err() != nil {
		return
	}
	// A manual or periodic read that began before the rejection cannot settle it.
	r.refreshCreditsForManager(ctx, r.db, job.account, job.manager, job.revision)
}

func (q *creditRecheckQueue) close() {
	q.mu.Lock()
	q.closed, q.pending, q.recent = true, nil, nil
	done := q.done
	if q.cancel != nil {
		q.cancel()
	}
	q.mu.Unlock()
	if done != nil {
		<-done
	}
}

type maintenanceState struct {
	r                 *Runtime
	ctx               context.Context
	lastCredit        float64
	lastKeepaliveDate string
	checkinJobs       map[string]checkinJob
}
type checkinJob struct {
	date, identity string
	attempts       int
	retryAt        time.Time
	complete       bool
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
	var skip map[string]bool
	t := today()
	var failed []string
	for _, acc := range s.r.Pool.Accounts() {
		mgr := s.r.Manager(acc.UID)
		if mgr == nil {
			continue
		}
		identity, _ := mgr.MaintenanceIdentity()
		if s.checkinJobs == nil {
			s.checkinJobs = map[string]checkinJob{}
		}
		job := s.checkinJobs[acc.UID]
		if job.date != t || job.identity != identity {
			job = checkinJob{date: t, identity: identity}
		}
		if job.complete || job.attempts >= checkinMaxRetry || time.Now().Before(job.retryAt) {
			continue
		}
		// 国际站 WorkBuddy 无签到活动，跳过（避免每日两次注定失败的请求与日志噪音）
		if acc.Provider == "workbuddy" && siterouting.ProfileSite(acc.Profile) == "international" {
			continue
		}
		state := s.r.billingGuard().State(acc.UID, identity)
		if state.NeedsLogin || state.Kind != "" && time.Now().Unix() < state.RetryAt {
			continue
		}
		if skip == nil {
			skip = s.checkedInToday()
		}
		if skip[acc.UID] {
			job.complete = true
			s.checkinJobs[acc.UID] = job
			continue
		}
		res, err := s.r.checkinAccount(ctx, acc)
		if err != nil {
			if maintenancePaused(err) {
				continue
			}
			log.Printf("签到失败 %s: %v", acc.UID, err)
			job.attempts++
			job.retryAt = time.Now().Add(2 * time.Hour)
			s.checkinJobs[acc.UID] = job
			failed = append(failed, acc.UID)
			continue
		}
		log.Printf("签到 %s: ok=%v already=%v %s", acc.UID, res.OK, res.Already, res.Message)
		if res.OK || res.Already {
			// record only on success/already, so failures retry；
			// 同时追加历史（签到日历数据源），already 也记（幂等）。
			_ = s.r.db.RecordCheckin(acc.UID, t, "auto")
		} else {
			// A deterministic business rejection is not a transport retry.
		}
		job.complete = true
		s.checkinJobs[acc.UID] = job
	}
	// checkin can change balances (rewards); refresh + auto-thaw afterwards.
	return failed
}

// Keepalive never changes the administrator's dispatch switch. Authentication
// failures enter the same maintenance recovery state as billing and check-in.
func (s *maintenanceState) doKeepalive() {
	if s.setting("keepalive_enabled", "1") != "1" {
		return
	}
	for _, acc := range s.r.Pool.Accounts() {
		mgr := s.r.Manager(acc.UID)
		if mgr == nil {
			continue
		}
		if s.r.prepareBillingMaintenance(s.context(), acc) != nil {
			continue
		}
		if err := mgr.KeepaliveErrorContext(s.context()); err == nil {
			continue
		} else {
			s.r.observeBilling(acc.UID, mgr, err)
			log.Printf("token 保活未完成，已按授权状态暂停后续维护")
		}
		if s.context().Err() != nil {
			return
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
	// Per-account state isolates authentication pauses from transient task retries.
	// Revisit pending credentials on the existing minute tick; completed accounts
	// do no upstream work, and reauthorization can catch up in the same day.
	if now.Hour() >= minInt(hours) {
		s.doCheckin()
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
