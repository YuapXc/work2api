// Package streamwatch provides watchdog protection for upstream SSE streams:
// an idle timeout (no bytes from upstream for N seconds → abort) and an
// overall duration cap. Without it, an upstream that accepts the connection
// and then hangs keeps the request goroutine and connection alive until the
// client disconnects.
//
// Mechanism: a watchdog goroutine holds the response body and closes it when
// a deadline fires. Closing the body unblocks any pending Read with an error,
// so no read-loop changes are needed at call sites. Every successful Read
// (and explicit Touch) resets the idle deadline.
package streamwatch

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults: idle must exceed the longest legit silent stretch of an upstream
// (thinking models emit reasoning deltas continuously, so long silence means
// the pipe is dead); total caps runaway streams.
const (
	DefaultIdle  = 180 * time.Second
	DefaultTotal = 30 * time.Minute
)

// Watch wraps a stream body with idle/total deadlines. Create with Watch,
// read the returned io.ReadCloser normally, and the underlying body is closed
// for you on first deadline breach or when the watcher's own Close is called.
type Watch struct {
	err       atomic.Pointer[BreachError]
	closeOnce sync.Once

	ctx        context.Context
	underlying ioCloser
	idle       time.Duration
	total      time.Duration

	touch chan struct{}
	done  chan struct{}
}

type ioCloser interface {
	io.ReadCloser
}

// BreachError distinguishes watchdog aborts from ordinary transport errors.
type BreachError struct {
	Kind string // "idle" / "total"
	Age  time.Duration
}

func (e *BreachError) Error() string {
	if e.Kind == "idle" {
		return fmt.Sprintf("upstream stream idle for %s (watchdog abort)", e.Age)
	}
	return fmt.Sprintf("upstream stream exceeded total duration %s (watchdog abort)", e.Age)
}

// NewWatch starts watchdog protection. ctx cancellation also closes the body.
// idle <= 0 → DefaultIdle; total <= 0 → DefaultTotal.
func NewWatch(body io.ReadCloser, ctx context.Context, idle, total time.Duration) *Watch {
	if idle <= 0 {
		idle = DefaultIdle
	}
	if total <= 0 {
		total = DefaultTotal
	}
	w := &Watch{
		ctx:        ctx,
		underlying: body,
		idle:       idle,
		total:      total,
		touch:      make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	go w.loop(ctx)
	return w
}

// Touch records stream activity, postponing the idle deadline.
func (w *Watch) Touch() {
	select {
	case w.touch <- struct{}{}:
	default:
	}
}

// Close stops the watchdog (does NOT close the underlying body — the owner's
// defer does that).
func (w *Watch) Close() {
	w.closeOnce.Do(func() { close(w.done) })
}

// Err safely publishes a watchdog failure to the reader goroutine.
func (w *Watch) Err() error {
	if err := w.err.Load(); err != nil {
		return err
	}
	return nil
}

func (w *Watch) loop(ctx context.Context) {
	idleTimer := time.NewTimer(w.idle)
	totalTimer := time.NewTimer(w.total)
	defer idleTimer.Stop()
	defer totalTimer.Stop()

	abort := func(kind string, age time.Duration) {
		w.err.Store(&BreachError{Kind: kind, Age: age})
		_ = w.underlying.Close() // unblocks the pending Read at the call site
	}
	for {
		select {
		case <-w.done:
			return
		case <-ctx.Done():
			_ = w.underlying.Close()
			return
		case <-idleTimer.C:
			abort("idle", w.idle)
			return
		case <-totalTimer.C:
			abort("total", w.total)
			return
		case <-w.touch:
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(w.idle)
		}
	}
}

// Reader wraps an io.Reader so every successful Read Touches the watchdog,
// resetting the idle deadline. Use it as the drop-in reader at the call site:
//
//	w := streamwatch.Watch(resp.Body, ctx, 0, 0)
//	defer w.Close()
//	reader := w.Reader(resp.Body)
type autoTouch struct {
	r io.Reader
	w *Watch
}

func (a autoTouch) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.w.Touch()
	}
	if err != nil && n == 0 {
		// Read error/EOF: keep the watchdog from firing on a dead stream
		a.w.Touch()
	}
	return n, err
}

// Reader returns an auto-touching view of any reader (pass resp.Body itself;
// the watchdog closes the underlying body on breach regardless).
func (w *Watch) Reader(r io.Reader) io.Reader { return autoTouch{r: LimitReader(w.ctx, r), w: w} }
