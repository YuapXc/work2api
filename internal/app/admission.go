package app

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync"
	"time"

	"work2api/internal/config"
	"work2api/internal/streamwatch"
	"work2api/internal/workbuddy/pool"
)

type modelTicket struct {
	key            string
	limit          int
	ready, running bool
	since          time.Time
	shared         bool
	model          string
	// Reserve the account and execution slot together; busy accounts park in queue.
	chooseAccount func(map[string]bool) (*pool.Account, *apiError)
	account       *pool.Account
	accountHeld   bool
	selectionErr  *apiError
	bufferBytes   int64
	bufferHeld    bool
	// Release is idempotent across explicit error cleanup and deferred cleanup.
	released bool
}

// A single bounded queue rotates between authenticated applications. Parked
// account-throttle waits remain in this queue, rather than holding execution slots.
type modelAdmission struct {
	mu                                                       sync.Mutex
	capacity, queueSize, keyQueue                            int
	wait                                                     time.Duration
	limits                                                   map[string]int
	active                                                   int
	sharedActive, sharedCapacity                             int
	userQueue, sharedQueue                                   int
	byKey                                                    map[string]int
	queue                                                    []*modelTicket
	served                                                   map[string]uint64
	sequence                                                 uint64
	changed                                                  chan struct{}
	rejected                                                 map[string]int64
	accountLimit                                             int
	byAccount                                                map[string]int
	byModel                                                  map[string]int
	bufferBudget, sharedBufferBudget, buffers, sharedBuffers int64
	memoryHigh                                               uint64
	memoryUsage                                              func() uint64
	lastPressureGC                                           time.Time
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
	a := &modelAdmission{capacity: positiveOr(c.MaxConcurrentRequests, 13), queueSize: positiveOr(c.ModelQueueSize, 32), keyQueue: positiveOr(c.ModelKeyQueueSize, 16), wait: time.Duration(positiveOr(c.ModelQueueWaitSeconds, 60)) * time.Second, limits: map[string]int{}, byKey: map[string]int{}, served: map[string]uint64{}, changed: make(chan struct{}), rejected: map[string]int64{}}
	if c.ModelKeyLimits != "" {
		if err := json.Unmarshal([]byte(c.ModelKeyLimits), &a.limits); err != nil {
			log.Print("MODEL_KEY_CONCURRENCY_LIMITS 无效，使用全局并发限制")
			a.limits = map[string]int{}
		}
	}
	a.userQueue = positiveOr(c.PortalUserQueueSize, 6)
	a.sharedQueue = positiveOr(c.PortalSharedQueueSize, 28)
	if a.queueSize > 1 && a.sharedQueue >= a.queueSize {
		a.sharedQueue = a.queueSize - 1
	}
	a.sharedCapacity = positiveOr(c.PortalSharedConcurrency, 12)
	if a.capacity > 1 && a.sharedCapacity >= a.capacity {
		// 至少为非共享请求保留一个全局名额。
		a.sharedCapacity = a.capacity - 1
	}
	a.sharedCapacity = min(a.sharedCapacity, a.capacity)
	a.byModel = map[string]int{}
	a.accountLimit = positiveOr(c.WorkBuddyAccountConcurrency, 4)
	a.byAccount = map[string]int{}
	a.bufferBudget = c.ModelBufferBudgetBytes
	if a.bufferBudget <= 0 {
		a.bufferBudget = 224 << 20
	}
	responseLimit := c.MaxResponseBytes
	if responseLimit <= 0 {
		responseLimit = 8 << 20
	}
	// Reserve enough response budget for one private nonstream request.
	a.sharedBufferBudget = max(int64(0), a.bufferBudget-4*responseLimit)
	if c.ModelMemoryGuard {
		a.memoryHigh = uint64(c.ModelMemoryHighBytes)
		if c.ModelMemoryHighBytes <= 0 {
			a.memoryHigh = 256 << 20
		}
		a.memoryUsage = goMemoryUsage
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
			if !t.ready || a.byKey[t.key] >= t.limit || !a.sharedRoomLocked(t) || !a.bufferRoomLocked(t) {
				continue
			}
			if t.chooseAccount != nil && !t.accountHeld {
				busy := map[string]bool{}
				for uid, n := range a.byAccount {
					if n >= a.accountLimit {
						busy[uid] = true
					}
				}
				t.account, t.selectionErr = t.chooseAccount(busy)
				if t.account == nil && t.selectionErr == nil {
					continue
				}
			}
			if pick < 0 || (a.byKey[t.key] < a.byKey[a.queue[pick].key] || (a.byKey[t.key] == a.byKey[a.queue[pick].key] && a.served[t.key] < a.served[a.queue[pick].key])) {
				pick = i
			}
		}
		if pick < 0 {
			break
		}
		t := a.queue[pick]
		a.queue = append(a.queue[:pick], a.queue[pick+1:]...)
		t.running = true
		a.reserveBufferLocked(t)
		if t.account != nil && !t.accountHeld {
			a.byAccount[t.account.UID]++
			t.accountHeld = true
		}
		a.active++
		if t.shared {
			a.sharedActive++
			a.byModel[t.model]++
		}
		a.byKey[t.key]++
		a.sequence++
		a.served[t.key] = a.sequence
	}
	a.signalLocked()
}

func (a *modelAdmission) enqueueLocked(t *modelTicket) *apiError {
	code := "model_queue_full"
	count, sharedCount := 0, 0
	for _, q := range a.queue {
		if q.shared {
			sharedCount++
		}
		if q.key == t.key {
			count++
		}
	}
	queueLimit := a.keyQueue
	if t.shared && a.userQueue < queueLimit {
		queueLimit = a.userQueue
	}
	if t.shared && sharedCount >= a.sharedQueue {
		code = "shared_queue_full"
	} else if count >= queueLimit {
		code = "key_queue_full"
	} else if len(a.queue) < a.queueSize {
		a.queue = append(a.queue, t)
		return nil
	}
	a.rejected[code]++
	return localOverload(code)
}

func (a *modelAdmission) acquire(ctx context.Context, key string) (*modelLease, error) {
	return a.acquireWithLimit(ctx, key, 0, "")
}

// Shared execution always leaves private capacity available.
func (a *modelAdmission) sharedRoomLocked(t *modelTicket) bool {
	return !t.shared || a.sharedActive < a.sharedCapacity
}

// Portal keys of one user share a single concurrency limit.
func (a *modelAdmission) acquireWithLimit(ctx context.Context, key string, limitOverride int, model string) (*modelLease, error) {
	return a.acquireWithBudget(ctx, key, limitOverride, model, 0)
}

func (a *modelAdmission) acquireWithBudget(ctx context.Context, key string, limitOverride int, model string, bufferBytes int64) (*modelLease, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	capacity := a.capacity
	limit := capacity
	if n := a.limits[key]; n > 0 && n < limit {
		limit = n
	}
	if limitOverride > 0 && limitOverride < limit {
		limit = limitOverride
	}
	t := &modelTicket{key: key, limit: limit, ready: true, since: time.Now(), shared: strings.HasPrefix(key, "portal-user-"), model: model, bufferBytes: bufferBytes}
	if bufferBytes > a.bufferBudget || (t.shared && bufferBytes > a.sharedBufferBudget) {
		return nil, localOverload("response_buffer_budget")
	}
	l := &modelLease{a: a, t: t, remaining: a.wait}
	a.mu.Lock()
	// Existing runnable waiters always get first chance.
	a.dispatchLocked()
	if a.active < a.capacity && a.byKey[key] < limit && a.sharedRoomLocked(t) && a.bufferRoomLocked(t) {
		t.running = true
		a.reserveBufferLocked(t)
		a.active++
		if t.shared {
			a.sharedActive++
			a.byModel[t.model]++
		}
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
	l.releaseAccountLocked()
	a.releaseBufferLocked(t)
	if t.running {
		a.active--
		if t.shared {
			a.sharedActive--
			a.releaseModelLocked(t.model)
		}
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
	if l.t.released {
		l.a.mu.Unlock()
		return
	}
	l.t.released = true
	l.removeLocked()
	l.a.dispatchLocked()
	l.a.mu.Unlock()
}

func copyCounts(src map[string]int) map[string]int {
	dst := make(map[string]int, len(src))
	for k, n := range src {
		dst[k] = n
	}
	return dst
}

func (l *modelLease) releaseAccountLocked() {
	if l.t.accountHeld {
		uid := l.t.account.UID
		l.a.byAccount[uid]--
		if l.a.byAccount[uid] == 0 {
			delete(l.a.byAccount, uid)
		}
		l.t.accountHeld = false
	}
}

// bindAccount returns both permits to the fair queue, then atomically reserves
// a free authorized account and an execution slot. The same wait budget covers
// initial admission, account contention, start pacing and retry admission.
func (l *modelLease) bindAccount(ctx context.Context, choose func(map[string]bool) (*pool.Account, *apiError)) (*pool.Account, error) {
	a := l.a
	a.mu.Lock()
	if l.t.released || !l.t.running {
		a.mu.Unlock()
		return nil, context.Canceled
	}
	l.releaseAccountLocked()
	a.releaseBufferLocked(l.t)
	a.active--
	a.byKey[l.t.key]--
	if a.byKey[l.t.key] == 0 {
		delete(a.byKey, l.t.key)
	}
	if l.t.shared {
		a.sharedActive--
		a.releaseModelLocked(l.t.model)
	}
	l.t.running = false
	l.t.chooseAccount = choose
	l.t.account = nil
	l.t.selectionErr = nil
	// Let existing waiters run first. Do not reject a runnable request merely
	// because the wait queue is full: only actual waiters consume queue capacity.
	a.dispatchLocked()
	if a.active < a.capacity && a.byKey[l.t.key] < l.t.limit && a.sharedRoomLocked(l.t) && a.bufferRoomLocked(l.t) {
		busy := map[string]bool{}
		for uid, n := range a.byAccount {
			if n >= a.accountLimit {
				busy[uid] = true
			}
		}
		l.t.account, l.t.selectionErr = choose(busy)
		if l.t.selectionErr != nil {
			err := l.t.selectionErr
			a.mu.Unlock()
			return nil, err
		}
		if l.t.account != nil {
			l.t.accountHeld = true
			a.byAccount[l.t.account.UID]++
			l.t.running = true
			a.reserveBufferLocked(l.t)
			a.active++
			a.byKey[l.t.key]++
			if l.t.shared {
				a.sharedActive++
				a.byModel[l.t.model]++
			}
			a.sequence++
			a.served[l.t.key] = a.sequence
		}
	}
	if !l.t.running {
		if err := a.enqueueLocked(l.t); err != nil {
			a.mu.Unlock()
			return nil, err
		}
	}
	a.signalLocked()
	a.mu.Unlock()
	if err := l.await(ctx); err != nil {
		return nil, err
	}
	return l.t.account, nil
}

func (a *modelAdmission) releaseModelLocked(model string) {
	a.byModel[model]--
	if a.byModel[model] == 0 {
		delete(a.byModel, model)
	}
}

func (l *modelLease) await(ctx context.Context) error {
	start := time.Now()
	defer func() { streamwatch.QueueWait(ctx, time.Since(start), false) }()
	timer := time.NewTimer(l.remaining)
	defer timer.Stop()
	// Wake memory-pressure waiters even when no model request finishes.
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		l.a.mu.Lock()
		err := ctx.Err()
		if err == nil && l.t.running {
			l.a.mu.Unlock()
			l.remaining -= time.Since(start)
			if l.t.selectionErr != nil {
				err := l.t.selectionErr
				l.release()
				return err
			}
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
		case <-ticker.C:
			l.a.mu.Lock()
			// Dead objects can keep the guard closed before the ordinary heap
			// goal triggers GC. Only one waiter requests collection per interval,
			// and collection must not hold the scheduler lock.
			collect := l.a.memoryUsage != nil && l.a.memoryUsage() >= l.a.memoryHigh && time.Since(l.a.lastPressureGC) >= 5*time.Second
			if collect {
				l.a.lastPressureGC = time.Now()
			}
			l.a.mu.Unlock()
			if collect {
				runtime.GC()
			}
			l.a.mu.Lock()
			l.a.dispatchLocked()
			l.a.mu.Unlock()
		}
	}
}

// throttle uses the remaining queue budget and gives the model slot to other
// channels while WorkBuddy waits for an account's minimum interval.
func (l *modelLease) throttle(ctx context.Context, wait func(context.Context) error) error {
	start := time.Now()
	a := l.a
	a.mu.Lock()
	if l.t.released || !l.t.running {
		a.mu.Unlock()
		return context.Canceled
	}
	l.t.ready = false
	l.t.since = start
	if err := a.enqueueLocked(l.t); err != nil {
		l.t.ready = true
		a.mu.Unlock()
		return err
	}
	a.active--
	if l.t.shared {
		a.sharedActive--
		a.releaseModelLocked(l.t.model)
	}
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
	streamwatch.QueueWait(ctx, time.Since(start), true)
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
	sharedQueued := 0
	waitingReasons := map[string]int{}
	for _, t := range a.queue {
		if t.shared {
			sharedQueued++
		}
		reason := "fairness"
		switch {
		case !t.ready:
			reason = "upstream_pacing"
		case a.byKey[t.key] >= t.limit:
			reason = "key_or_user_limit"
		case !a.sharedRoomLocked(t):
			reason = "shared_limit"
		case a.memoryUsage != nil && a.memoryUsage() >= a.memoryHigh:
			reason = "memory_pressure"
		case !t.bufferHeld && (a.buffers+t.bufferBytes > a.bufferBudget || t.shared && a.sharedBuffers+t.bufferBytes > a.sharedBufferBudget):
			reason = "response_buffer_budget"
		case a.active >= a.capacity:
			reason = "execution_limit"
		case t.chooseAccount != nil && !t.accountHeld:
			reason = "account_busy"
		}
		waitingReasons[reason]++
	}
	byModel := make(map[string]int, len(a.byModel))
	for model, count := range a.byModel {
		byModel[model] = count
	}
	return map[string]any{"waiting_reasons": waitingReasons, "buffer_reserved_bytes": a.buffers, "buffer_budget_bytes": a.bufferBudget, "shared_buffer_reserved_bytes": a.sharedBuffers, "shared_buffer_budget_bytes": a.sharedBufferBudget, "memory_high_bytes": a.memoryHigh, "memory_pressure": a.memoryUsage != nil && a.memoryUsage() >= a.memoryHigh, "account_capacity": a.accountLimit, "account_running": copyCounts(a.byAccount), "portal_model_running": byModel, "shared_running": a.sharedActive, "shared_capacity": a.sharedCapacity, "shared_queued": sharedQueued, "shared_queue_capacity": a.sharedQueue, "user_queue_capacity": a.userQueue, "running": a.active, "queued": len(a.queue), "capacity": a.capacity, "queue_capacity": a.queueSize, "wait_limit_ms": a.wait.Milliseconds(), "oldest_wait_ms": oldest.Milliseconds(), "rejected": rejected}
}

func (s *Server) admitModel(w http.ResponseWriter, r *http.Request, p *Principal) (*http.Request, func(), bool) {
	key := strconv.FormatInt(p.AppID, 10)
	charge := int64(positiveOr(int(s.o.cfg.MaxResponseBytes), 8<<20)) * 4
	if stream, ok := pRequestStream(r); ok && stream {
		charge /= 2
	}
	if p.AppName == "model-test" && p.AppID == 0 {
		key = "admin-test"
	}
	// 门户 Key 的并发上限按用户计（PORTAL_USER_CONCURRENCY，默认 4）：同一
	// 用户多个 Key 共享一个并发槽，跨 Key 合并限流（HANDOFF §8）。
	if p.UserID > 0 {
		key = "portal-user-" + strconv.FormatInt(p.UserID, 10)
		l, err := s.modelsAdmission.acquireWithBudget(r.Context(), key, s.portalUserLimit(p.UserID), p.EffectiveModel, charge)
		if err != nil {
			st, body := errToHTTP(err)
			if st == 429 {
				w.Header().Set("Retry-After", "2")
			}
			writeJSON(w, st, body)
			return r, func() {}, false
		}
		return s.finishAdmit(w, r, l)
	}
	l, err := s.modelsAdmission.acquireWithBudget(r.Context(), key, 0, p.EffectiveModel, charge)
	if err != nil {
		st, body := errToHTTP(err)
		if st == 429 {
			w.Header().Set("Retry-After", "2")
		}
		writeJSON(w, st, body)
		return r, func() {}, false
	}
	return s.finishAdmit(w, r, l)
}

// finishAdmit is the tail shared by both admission paths (headers + context).
func (s *Server) finishAdmit(w http.ResponseWriter, r *http.Request, l *modelLease) (*http.Request, func(), bool) {
	w.Header().Set("X-Queue-Wait-Ms", strconv.FormatInt(time.Since(l.t.since).Milliseconds(), 10))
	ctx := streamwatch.WithResponseLimit(r.Context(), int64(positiveOr(int(s.o.cfg.MaxResponseBytes), 8<<20)))
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	return r.WithContext(context.WithValue(ctx, modelLeaseKey{}, l)), func() { cancel(); l.release(); _ = http.NewResponseController(w).SetWriteDeadline(time.Time{}) }, true
}

// Go memory in use, excluding released and reusable heap pages. Counting
// reusable pages as pressure can indefinitely park an idle service after a burst.
// Response reservations and the OS limit still protect future memory growth.
func goMemoryUsage() uint64 {
	samples := []metrics.Sample{{Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"}, {Name: "/memory/classes/heap/free:bytes"}, {Name: "/memory/classes/heap/unused:bytes"}}
	metrics.Read(samples)
	if samples[0].Value.Kind() != metrics.KindUint64 {
		return 0
	}
	used := samples[0].Value.Uint64()
	for _, sample := range samples[1:] {
		if sample.Value.Kind() == metrics.KindUint64 {
			used -= min(used, sample.Value.Uint64())
		}
	}
	return used
}
func (a *modelAdmission) bufferRoomLocked(t *modelTicket) bool {
	if a.memoryUsage != nil && a.memoryUsage() >= a.memoryHigh {
		return false
	}
	if t.bufferHeld {
		return true
	}
	return a.buffers+t.bufferBytes <= a.bufferBudget && (!t.shared || a.sharedBuffers+t.bufferBytes <= a.sharedBufferBudget)
}
func (a *modelAdmission) reserveBufferLocked(t *modelTicket) {
	if t.bufferHeld {
		return
	}
	t.bufferHeld = true
	a.buffers += t.bufferBytes
	if t.shared {
		a.sharedBuffers += t.bufferBytes
	}
}
func (a *modelAdmission) releaseBufferLocked(t *modelTicket) {
	if !t.bufferHeld {
		return
	}
	t.bufferHeld = false
	a.buffers -= t.bufferBytes
	if t.shared {
		a.sharedBuffers -= t.bufferBytes
	}
}

type requestStreamKey struct{}

func pRequestStream(r *http.Request) (bool, bool) {
	v, ok := r.Context().Value(requestStreamKey{}).(bool)
	return v, ok
}
