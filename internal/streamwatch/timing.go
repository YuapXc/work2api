package streamwatch

import (
	"context"
	"sync"
	"time"
)

type timingKey struct{}

// Timing carries bounded, content-free observations across provider boundaries.
type Timing struct {
	mu                          sync.Mutex
	Start                       time.Time
	Queue, Account              time.Duration
	FirstByte                   time.Duration
	BytesSeen                   bool
	Attempts, Upstream429       int
	Outcome                     string
	RequestID                   string
	FirstAttempt, WaitAtAttempt time.Duration
	AttemptStages               []AttemptStage
	lastAttempt                 time.Time
}

// AttemptStage contains no URL, account identifier, headers or request content.
type AttemptStage struct {
	Number       int   `json:"number"`
	HeaderWaitMS int64 `json:"header_wait_ms"`
	HTTPStatus   int   `json:"http_status"`
	Finished     bool  `json:"headers_finished"`
}

func WithTiming(ctx context.Context) (context.Context, *Timing) {
	t := &Timing{Start: time.Now()}
	return context.WithValue(ctx, timingKey{}, t), t
}
func TimingFrom(ctx context.Context) *Timing { t, _ := ctx.Value(timingKey{}).(*Timing); return t }
func QueueWait(ctx context.Context, d time.Duration, account bool) {
	if t := TimingFrom(ctx); t != nil {
		t.mu.Lock()
		if account {
			t.Account += d
		} else {
			t.Queue += d
		}
		t.mu.Unlock()
	}
}
func StartAttempt(ctx context.Context) {
	if t := TimingFrom(ctx); t != nil {
		t.mu.Lock()
		if t.Attempts == 0 {
			t.FirstAttempt = time.Since(t.Start)
			t.WaitAtAttempt = t.Queue + t.Account
		}
		t.Attempts++
		t.lastAttempt = time.Now()
		if len(t.AttemptStages) < 16 {
			t.AttemptStages = append(t.AttemptStages, AttemptStage{Number: t.Attempts})
		}
		t.mu.Unlock()
	}
}

// AttemptHeaders records only connection/header wait, not body generation time.
func AttemptHeaders(ctx context.Context, status int) {
	if t := TimingFrom(ctx); t != nil {
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.Attempts == 0 || t.Attempts > len(t.AttemptStages) {
			return
		}
		stage := &t.AttemptStages[t.Attempts-1]
		if stage.Finished {
			return
		}
		stage.HeaderWaitMS = time.Since(t.lastAttempt).Milliseconds()
		stage.HTTPStatus = status
		stage.Finished = true
	}
}
func AttemptResult(ctx context.Context, status int) {
	if status != 429 {
		return
	}
	if t := TimingFrom(ctx); t != nil {
		t.mu.Lock()
		t.Upstream429++
		t.mu.Unlock()
	}
}
func Outcome(ctx context.Context, status string) {
	if t := TimingFrom(ctx); t != nil {
		t.mu.Lock()
		t.Outcome = status
		t.mu.Unlock()
	}
}
func (t *Timing) FirstWrite() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.BytesSeen {
		t.BytesSeen = true
		t.FirstByte = time.Since(t.Start)
	}
}

type TimingSnapshot struct {
	Start                       time.Time
	Queue, Account, FirstByte   time.Duration
	BytesSeen                   bool
	Attempts, Upstream429       int
	Outcome                     string
	RequestID                   string
	FirstAttempt, WaitAtAttempt time.Duration
	AttemptStages               []AttemptStage
}

func (t *Timing) Snapshot() TimingSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TimingSnapshot{Start: t.Start, Queue: t.Queue, Account: t.Account, FirstByte: t.FirstByte, BytesSeen: t.BytesSeen, Attempts: t.Attempts, Upstream429: t.Upstream429, Outcome: t.Outcome, RequestID: t.RequestID, FirstAttempt: t.FirstAttempt, WaitAtAttempt: t.WaitAtAttempt, AttemptStages: append([]AttemptStage(nil), t.AttemptStages...)}
}
