package app

// Scheduler is the background daemon ported from workbuddy_one/scheduler.py.
// Single-user scope: daily checkin (with catch-up + retry), periodic credit
// refresh, daily token keepalive (fail-threshold disable), daily model refresh
// and cache-TTL warming, daily usage-log cleanup, and daily AA benchmark
// refresh. All timing is read from the DB settings on every tick, so changes
// take effect without a restart.
//
// Deferred (per project decisions): credit webhook alerts,
// and attachment archiving.

import (
	"context"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"work2api/internal/core/provider"
	"work2api/internal/statebackup"
)

// Scheduler owns the periodic background tasks over an Orchestrator.
type Scheduler struct {
	ctx    context.Context
	cancel context.CancelFunc
	o      *Orchestrator
	stop   chan struct{}
	done   chan struct{}

	lastCredit   float64
	lastRowCheck float64

	lastModelRefreshDate string
	lastBenchDate        string
	lastCleanupDate      string
}

// NewScheduler builds a scheduler bound to the orchestrator.
func NewScheduler(o *Orchestrator) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())
	return &Scheduler{
		ctx: ctx, cancel: cancel,
		o:    o,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
}

// Start launches the background loop. Warming (credits + models) happens as the
// loop's first action so startup never blocks and Stop() always completes.
func (s *Scheduler) Start() { go s.run() }

// Stop signals the loop to exit and waits (bounded) for it to finish, so a hung
// upstream call during a tick can never wedge process shutdown.
func (s *Scheduler) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
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

func (s *Scheduler) context() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
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

// cleanupUsage applies common retention and row limits.
func (s *Scheduler) cleanupUsage() {
	n, err := s.o.db.CleanupUsage(s.o.cfg.UsageRetentionDays)
	if err != nil {
		log.Printf("使用记录清理异常: %v", err)
	} else if n > 0 {
		log.Printf("使用记录清理完成（保留 %d 天，删除 %d 条）", s.o.cfg.UsageRetentionDays, n)
	}
	s.checkUsageRows()
	if n, err := s.o.db.CleanupOldDailyQuota(); err != nil {
		log.Printf("日额度记录清理异常: %v", err)
	} else if n > 0 {
		log.Printf("日额度记录清理完成（删除 %d 条）", n)
	}
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
	leaveWarm := statebackup.Enter()
	for _, rt := range s.o.runtimes.Runtimes() {
		if warm, ok := rt.(provider.WarmMaintainer); ok {
			warm.Warm(s.context())
		}
	}
	s.o.refreshRuntimeModels(s.context())
	// 同步 lastCredit：否则首个 tick 的周期判据（now-lastCredit >= interval）
	// 因零值必然命中，刚预热完又对全部账号白刷一遍额度（Workbuddy2API #34 同款）。
	s.lastCredit = float64(time.Now().Unix())
	// lastRowCheck 同形：零值会让首个 tick 立即对 usage_logs 跑行数检查。
	s.lastRowCheck = s.lastCredit
	// AA benchmarks: warm once at startup so the Models page has data without
	// waiting for the daily window (skipped cheaply when no key is configured).
	if s.o.bench.Configured() {
		s.o.bench.Refresh()
	}

	leaveWarm()
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		default:
		}
		s.tick()
		s.o.scheduledBackup(s.ctx)
		select {
		case <-s.stop:
			return
		case <-ticker.C:
		}
	}
}

func (s *Scheduler) tick() {
	leave := statebackup.Enter()
	defer leave()
	now := time.Now()
	settings, _ := s.o.db.GetSettings()
	ctx := s.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	for _, rt := range s.o.runtimes.Runtimes() {
		if maintainer, ok := rt.(provider.BackgroundMaintainer); ok {
			maintainer.Maintain(ctx, settings, now)
		}
	}
	t := today()
	if now.Hour() == 4 && s.lastCleanupDate != t {
		s.cleanupUsage()
		s.lastCleanupDate = t
	}
	if now.Hour() == parseHour(s.setting("model_refresh_hour", "6"), 6) && s.lastModelRefreshDate != t {
		s.o.refreshRuntimeModels(ctx)
		s.lastModelRefreshDate = t
	}
	// --- daily AA benchmark refresh (only when configured) ---
	if s.o.bench.Configured() && now.Hour() == parseHour(s.setting("aa_refresh_hour", "7"), 7) && s.lastBenchDate != t {
		s.o.bench.Refresh()
		s.lastBenchDate = t
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

func (s *Scheduler) setting(key, def string) string {
	settings, err := s.o.db.GetSettings()
	if err != nil {
		return def
	}
	if v := settings[key]; v != "" {
		return v
	}
	return def
}
