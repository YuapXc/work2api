package app

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"work2api/internal/config"
	"work2api/internal/streamwatch"
)

type modelTicket struct {
	key            string
	limit          int
	ready, running bool
	since          time.Time
}

// A single bounded queue rotates between authenticated applications. Parked
// account-throttle waits remain in this queue, rather than holding execution slots.
type modelAdmission struct {
	mu                            sync.Mutex
	capacity, queueSize, keyQueue int
	wait                          time.Duration
	limits                        map[string]int
	active                        int
	byKey                         map[string]int
	queue                         []*modelTicket
	served                        map[string]uint64
	sequence                      uint64
	changed                       chan struct{}
	rejected                      map[string]int64
}

type modelLease struct {
	a         *modelAdmission
	t         *modelTicket
	remaining time.Duration
}

type modelLeaseKey struct{}

func positiveOr(v, fallback int) int {
	if v > 0 {
		return v
	}
	return fallback
}

func newModelAdmission(c *config.Config) *modelAdmission {
	a := &modelAdmission{capacity: positiveOr(c.MaxConcurrentRequests, 4), queueSize: positiveOr(c.ModelQueueSize, 8), keyQueue: positiveOr(c.ModelKeyQueueSize, 8), wait: time.Duration(positiveOr(c.ModelQueueWaitSeconds, 20)) * time.Second, limits: map[string]int{}, byKey: map[string]int{}, served: map[string]uint64{}, changed: make(chan struct{}), rejected: map[string]int64{}}
	if c.ModelKeyLimits != "" {
		if err := json.Unmarshal([]byte(c.ModelKeyLimits), &a.limits); err != nil {
			log.Print("MODEL_KEY_CONCURRENCY_LIMITS 无效，使用全局并发限制")
			a.limits = map[string]int{}
		}
	}
	return a
}

func localOverload(code string) *apiError {
	e := errBody(429, "本地容量暂时不足，请稍后重试", "local_overload")
	e.body["error"].(map[string]any)["code"] = code
	return e
}

func (a *modelAdmission) signalLocked() { close(a.changed); a.changed = make(chan struct{}) }

func (a *modelAdmission) dispatchLocked() {
	for a.active < a.capacity {
		pick := -1
		for i, t := range a.queue {
			if !t.ready || a.byKey[t.key] >= t.limit {
				continue
			}
			if pick < 0 || a.served[t.key] < a.served[a.queue[pick].key] {
				pick = i
			}
		}
		if pick < 0 {
			break
		}
		t := a.queue[pick]
		a.queue = append(a.queue[:pick], a.queue[pick+1:]...)
		t.running = true
		a.active++
		a.byKey[t.key]++
		a.sequence++
		a.served[t.key] = a.sequence
	}
	a.signalLocked()
}

func (a *modelAdmission) enqueueLocked(t *modelTicket) *apiError {
	code := "model_queue_full"
	count := 0
	for _, q := range a.queue {
		if q.key == t.key {
			count++
		}
	}
	if count >= a.keyQueue {
		code = "key_queue_full"
	} else if len(a.queue) < a.queueSize {
		a.queue = append(a.queue, t)
		return nil
	}
	a.rejected[code]++
	return localOverload(code)
}

func (a *modelAdmission) acquire(ctx context.Context, key string) (*modelLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := a.capacity
	if n := a.limits[key]; n > 0 && n < limit {
		limit = n
	}
	t := &modelTicket{key: key, limit: limit, ready: true, since: time.Now()}
	l := &modelLease{a: a, t: t, remaining: a.wait}
	a.mu.Lock()
	// Existing runnable waiters always get first chance.
	a.dispatchLocked()
	if a.active < a.capacity && a.byKey[key] < limit {
		t.running = true
		a.active++
		a.byKey[key]++
		a.sequence++
		a.served[key] = a.sequence
	} else if err := a.enqueueLocked(t); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	a.mu.Unlock()
	if err := l.await(ctx); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *modelLease) removeLocked() {
	a, t := l.a, l.t
	if t.running {
		a.active--
		a.byKey[t.key]--
		if a.byKey[t.key] == 0 {
			delete(a.byKey, t.key)
		}
		t.running = false
	} else {
		for i, q := range a.queue {
			if q == t {
				a.queue = append(a.queue[:i], a.queue[i+1:]...)
				break
			}
		}
	}
	if a.byKey[t.key] == 0 {
		queued := false
		for _, q := range a.queue {
			if q.key == t.key {
				queued = true
				break
			}
		}
		if !queued {
			delete(a.served, t.key)
		}
	}
}

func (l *modelLease) release() {
	l.a.mu.Lock()
	l.removeLocked()
	l.a.dispatchLocked()
	l.a.mu.Unlock()
}

func (l *modelLease) await(ctx context.Context) error {
	start := time.Now()
	timer := time.NewTimer(l.remaining)
	defer timer.Stop()
	for {
		l.a.mu.Lock()
		err := ctx.Err()
		if err == nil && l.t.running {
			l.a.mu.Unlock()
			l.remaining -= time.Since(start)
			return nil
		}
		if err != nil || time.Since(start) >= l.remaining {
			if err == nil {
				l.a.rejected["model_queue_timeout"]++
				err = localOverload("model_queue_timeout")
			}
			l.removeLocked()
			l.a.dispatchLocked()
			l.a.mu.Unlock()
			return err
		}
		changed := l.a.changed
		l.a.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-timer.C:
		case <-changed:
		}
	}
}

// throttle uses the remaining queue budget and gives the model slot to other
// channels while WorkBuddy waits for an account's minimum interval.
func (l *modelLease) throttle(ctx context.Context, wait func(context.Context) error) error {
	start := time.Now()
	a := l.a
	a.mu.Lock()
	l.t.ready = false
	l.t.since = start
	if err := a.enqueueLocked(l.t); err != nil {
		l.t.ready = true
		a.mu.Unlock()
		return err
	}
	a.active--
	a.byKey[l.t.key]--
	if a.byKey[l.t.key] == 0 {
		delete(a.byKey, l.t.key)
	}
	l.t.running = false
	a.dispatchLocked()
	a.mu.Unlock()
	waitCtx, cancel := context.WithTimeout(ctx, l.remaining)
	defer cancel()
	err := wait(waitCtx)
	cancel()
	l.remaining -= time.Since(start)
	if err != nil {
		l.release()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == context.DeadlineExceeded {
			a.mu.Lock()
			a.rejected["model_queue_timeout"]++
			a.mu.Unlock()
			return localOverload("model_queue_timeout")
		}
		return err
	}
	if l.remaining <= 0 {
		l.release()
		a.mu.Lock()
		a.rejected["model_queue_timeout"]++
		a.mu.Unlock()
		return localOverload("model_queue_timeout")
	}
	a.mu.Lock()
	l.t.ready = true
	a.dispatchLocked()
	a.mu.Unlock()
	return l.await(ctx)
}

func (a *modelAdmission) snapshot() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	oldest := time.Duration(0)
	for _, t := range a.queue {
		if age := time.Since(t.since); age > oldest {
			oldest = age
		}
	}
	rejected := map[string]int64{}
	for k, v := range a.rejected {
		rejected[k] = v
	}
	return map[string]any{"running": a.active, "queued": len(a.queue), "capacity": a.capacity, "queue_capacity": a.queueSize, "wait_limit_ms": a.wait.Milliseconds(), "oldest_wait_ms": oldest.Milliseconds(), "rejected": rejected}
}

func (s *Server) admitModel(w http.ResponseWriter, r *http.Request, p *Principal) (*http.Request, func(), bool) {
	key := strconv.FormatInt(p.AppID, 10)
	if p.AppName == "model-test" && p.AppID == 0 {
		key = "admin-test"
	}
	l, err := s.modelsAdmission.acquire(r.Context(), key)
	if err != nil {
		st, body := errToHTTP(err)
		if st == 429 {
			w.Header().Set("Retry-After", "2")
		}
		writeJSON(w, st, body)
		return r, func() {}, false
	}
	w.Header().Set("X-Queue-Wait-Ms", strconv.FormatInt(time.Since(l.t.since).Milliseconds(), 10))
	ctx := streamwatch.WithResponseLimit(r.Context(), int64(positiveOr(int(s.o.cfg.MaxResponseBytes), 8<<20)))
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	return r.WithContext(context.WithValue(ctx, modelLeaseKey{}, l)), func() { cancel(); l.release(); _ = http.NewResponseController(w).SetWriteDeadline(time.Time{}) }, true
}
