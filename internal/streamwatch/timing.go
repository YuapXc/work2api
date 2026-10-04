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
		t.mu.Unlock()
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
}

func (t *Timing) Snapshot() TimingSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TimingSnapshot{Start: t.Start, Queue: t.Queue, Account: t.Account, FirstByte: t.FirstByte, BytesSeen: t.BytesSeen, Attempts: t.Attempts, Upstream429: t.Upstream429, Outcome: t.Outcome, RequestID: t.RequestID, FirstAttempt: t.FirstAttempt, WaitAtAttempt: t.WaitAtAttempt}
}
