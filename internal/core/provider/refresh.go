package provider

import (
	"context"
	"errors"
	"sync"
)

var ErrPartialRefresh = errors.New("部分目录来源刷新失败，保留可用快照")

type catalogFlight struct {
	key  string
	done chan struct{}
	err  error
}

// RefreshGate merges matching refreshes. Changed account identity waits for the
// previous task, then runs its own discovery instead of sharing stale results.
type RefreshGate struct {
	mu     sync.Mutex
	flight *catalogFlight
}

func (g *RefreshGate) Do(ctx context.Context, key string, refresh func(context.Context) error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		g.mu.Lock()
		if flight := g.flight; flight != nil {
			g.mu.Unlock()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-flight.done:
				if flight.key == key {
					return flight.err
				}
				continue
			}
		}
		flight := &catalogFlight{key: key, done: make(chan struct{})}
		g.flight = flight
		g.mu.Unlock()
		return func() (err error) {
			finished := false
			defer func() {
				g.mu.Lock()
				flight.err = err
				if !finished {
					flight.err = errors.New("目录刷新未完成")
				}
				g.flight = nil
				close(flight.done)
				g.mu.Unlock()
			}()
			err = refresh(ctx)
			finished = true
			return err
		}()
	}
}
