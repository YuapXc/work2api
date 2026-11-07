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
	"errors"
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

	lastCredit              float64
	lastUsageMaintenance    time.Time
	usageMaintenancePending bool

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
	ctx, cancel := context.WithTimeout(s.context(), time.Second)
	defer cancel()
	err := s.o.db.MaintainUsage(ctx, s.o.cfg.UsageRetentionDays, s.o.cfg.UsageMaxRows, s.o.cfg.UsageContentRetentionDays, s.o.cfg.UsageContentBudgetBytes)
	s.usageMaintenancePending = err != nil
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		log.Printf("使用记录维护异常: %v", err)
	}
	s.lastUsageMaintenance = time.Now()
	if s.lastCleanupDate != today() && ctx.Err() == nil {
		if _, err := s.o.db.CleanupOldDailyQuota(); err != nil {
			log.Printf("日额度记录清理异常: %v", err)
		} else {
			s.lastCleanupDate = today()
		}
	}
}

// run is the once-per-minute scheduling loop. Each branch is idempotent per day
// and uses catch-up semantics (>= hour, not == hour) so a service that starts
// after a window still performs that day's task.
func (s *Scheduler) run() {
	maintenanceDone := make(chan struct{})
	go s.runUsageMaintenance(maintenanceDone)
	var mediaDone chan struct{}
	defer func() {
		if s.cancel != nil {
			s.cancel()
		}
		<-maintenanceDone
		if mediaDone != nil {
			<-mediaDone
		}
		close(s.done)
	}()
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

	// AA benchmarks: warm once at startup so the Models page has data without
	// waiting for the daily window (skipped cheaply when no key is configured).
	if s.o.bench.Configured() {
		_ = s.o.bench.Refresh(s.context())
	}

	leaveWarm()
	mediaDone = make(chan struct{})
	go s.runMediaTracking(mediaDone)
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
	s.o.checkAccountsTick(ctx, settings)
	t := today()
	if now.Hour() == parseHour(s.setting("model_refresh_hour", "6"), 6) && s.lastModelRefreshDate != t {
		s.o.refreshRuntimeModels(ctx)
		s.lastModelRefreshDate = t
	}
	// --- daily AA benchmark refresh (only when configured) ---
	if s.o.bench.Configured() && now.Hour() == parseHour(s.setting("aa_refresh_hour", "7"), 7) && s.lastBenchDate != t {
		_ = s.o.bench.Refresh(ctx)
		s.lastBenchDate = t
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

func (s *Scheduler) StopAndWait(ctx context.Context) error {
	s.Stop()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Log maintenance is serialized independently of upstream warming/refreshing.
// It participates in snapshot coordination, and shutdown joins both workers.
func (s *Scheduler) runUsageMaintenance(done chan struct{}) {
	defer close(done)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-s.context().Done():
			return
		default:
		}
		if s.usageMaintenancePending || s.lastUsageMaintenance.IsZero() || time.Since(s.lastUsageMaintenance) >= time.Duration(positiveOr(s.o.cfg.UsageCheckIntervalMin, 30))*time.Minute {
			leave := statebackup.Enter()
			s.cleanupUsage()
			leave()
		}
		select {
		case <-s.stop:
			return
		case <-s.context().Done():
			return
		case <-ticker.C:
		}
	}
}
