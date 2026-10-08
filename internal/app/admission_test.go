package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"work2api/internal/config"
	"work2api/internal/streamwatch"
	"work2api/internal/workbuddy/models"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/upstream"
)

func awaitQueue(t *testing.T, a *modelAdmission, n int) {
	t.Helper()
	until := time.Now().Add(time.Second)
	for time.Now().Before(until) {
		if a.snapshot()["queued"].(int) == n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("queue never reached %d: %v", n, a.snapshot())
}

func TestModelAdmissionBoundedAndCancelable(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 4, ModelQueueSize: 8, ModelKeyQueueSize: 8})
	var active []*modelLease
	for i := 0; i < 4; i++ {
		l, err := a.acquire(context.Background(), "1")
		if err != nil {
			t.Fatal(err)
		}
		active = append(active, l)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			l, err := a.acquire(ctx, "1")
			if l != nil {
				l.release()
			}
			results <- err
		}()
	}
	awaitQueue(t, a, 8)
	if _, err := a.acquire(context.Background(), "2"); err == nil {
		t.Fatal("13th request admitted")
	}
	cancel()
	for i := 0; i < 8; i++ {
		if err := <-results; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel lost: %v", err)
		}
	}
	for _, l := range active {
		l.release()
	}
	if s := a.snapshot(); s["running"] != 0 || s["queued"] != 0 {
		t.Fatal(s)
	}
}

func TestAdmissionFairKeysAndLimits(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 1, ModelKeyLimits: `{"1":1}`})
	first, _ := a.acquire(context.Background(), "1")
	got := make(chan *modelLease, 2)
	go func() { l, _ := a.acquire(context.Background(), "1"); got <- l }()
	awaitQueue(t, a, 1)
	go func() { l, _ := a.acquire(context.Background(), "2"); got <- l }()
	awaitQueue(t, a, 2)
	first.release()
	second := <-got
	if second.t.key != "2" {
		t.Fatal("busy key starved a different key")
	}
	second.release()
	(<-got).release()
	a = newModelAdmission(&config.Config{MaxConcurrentRequests: 4, ModelKeyLimits: `{"1":1}`})
	one, _ := a.acquire(context.Background(), "1")
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := a.acquire(ctx, "1"); result <- err }()
	awaitQueue(t, a, 1)
	other, _ := a.acquire(context.Background(), "2")
	if a.snapshot()["running"] != 2 {
		t.Fatal("per-key limit ignored or blocked other key")
	}
	cancel()
	<-result
	one.release()
	other.release()
}

func TestThrottleReturnsSlotAndSharesWaitBudget(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 1})
	a.wait = 100 * time.Millisecond
	l, _ := a.acquire(context.Background(), "1")
	parked := make(chan struct{})
	finish := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- l.throttle(context.Background(), func(ctx context.Context) error {
			close(parked)
			select {
			case <-finish:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	<-parked
	other, err := a.acquire(context.Background(), "2")
	if err != nil {
		t.Fatal(err)
	}
	close(finish)
	if err := <-result; err == nil {
		t.Fatal("account wait + second admission escaped shared deadline")
	}
	l.release()
	other.release()
	if s := a.snapshot(); s["running"] != 0 || s["queued"] != 0 {
		t.Fatal(s)
	}
}

func TestManagementIndependentOfFullModelPool(t *testing.T) {
	s := NewServer(&Orchestrator{cfg: &config.Config{MaxConcurrentRequests: 4}})
	var leases []*modelLease
	for i := 0; i < 4; i++ {
		l, _ := s.modelsAdmission.acquire(context.Background(), "1")
		leases = append(leases, l)
		defer l.release()
	}
	h := s.requestGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	for _, path := range []string{"/admin/overview", "/health", "/assets/app.js"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 {
			t.Fatalf("management blocked by model traffic: %s: %d", path, w.Code)
		}
	}
}

func TestBodyBudgetChargesCapacityAndRecovers(t *testing.T) {
	b := &bodyBudget{limit: 2 << 20}
	data, reserved, err := b.read(strings.NewReader(strings.Repeat("x", 1<<20)), 1<<20)
	if err != nil || len(data) != 1<<20 || b.usage() != reserved {
		t.Fatal("body budget mismatch", err)
	}
	_, second, err := b.read(strings.NewReader(strings.Repeat("x", 2<<20)), 2<<20)
	if err == nil {
		t.Fatal("body budget overrun")
	}
	b.release(second)
	b.release(reserved)
	if b.usage() != 0 {
		t.Fatal("body budget leaked")
	}
	_, unknown, err := b.read(bytes.NewBuffer(make([]byte, 1<<20)), -1)
	if err != nil {
		t.Fatal(err)
	}
	b.release(unknown)
}

func TestBodyReaderQueueCancellationAndRecovery(t *testing.T) {
	s := NewServer(&Orchestrator{cfg: &config.Config{BodyReadConcurrency: 1, ModelQueueSize: 1}})
	s.bodySlots <- struct{}{}
	h := s.requestGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	request := func(ctx context.Context) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/test", strings.NewReader(`{}`)).WithContext(ctx))
		done <- w.Code
	}
	waitQueued := func() {
		t.Helper()
		deadline := time.Now().Add(time.Second)
		for len(s.bodyWaiting) == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if len(s.bodyWaiting) != 1 {
			t.Fatal("body reader did not queue")
		}
	}
	go request(ctx)
	waitQueued()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/test", strings.NewReader(`{}`)))
	if w.Code != 429 || !strings.Contains(w.Body.String(), "body_read_capacity") {
		t.Fatal("body waiting queue was not bounded", w.Code, w.Body.String())
	}
	cancel()
	<-done
	if len(s.bodyWaiting) != 0 || len(s.bodySlots) != 1 || s.bodies.usage() != 0 {
		t.Fatal("cancelled body waiter leaked capacity")
	}
	go request(context.Background())
	waitQueued()
	<-s.bodySlots
	if code := <-done; code != 204 || len(s.bodyWaiting) != 0 || len(s.bodySlots) != 0 || s.bodies.usage() != 0 {
		t.Fatal("body queue did not recover", code)
	}
}

func TestRefreshSingleFlightAndPanicCleanup(t *testing.T) {
	s := NewServer(&Orchestrator{cfg: &config.Config{MaxConcurrentRequests: 4}})
	var calls atomic.Int32
	started, finish := make(chan struct{}), make(chan struct{})
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(started)
		<-finish
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	result := make(chan *httptest.ResponseRecorder, 2)
	go func() {
		w := httptest.NewRecorder()
		s.serveRefresh(w, httptest.NewRequest("POST", "/admin/models", nil), next)
		result <- w
	}()
	<-started
	go func() {
		w := httptest.NewRecorder()
		s.serveRefresh(w, httptest.NewRequest("POST", "/admin/models/refresh", nil), next)
		result <- w
	}()
	// Wait for the follower to take a snapshot while the leader remains blocked.
	until := time.Now().Add(time.Second)
	for {
		s.refreshMu.Lock()
		n := s.refreshes["/admin/models/refresh"].waiters
		s.refreshMu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(until) {
			t.Fatal("follower did not join refresh")
		}
		time.Sleep(time.Millisecond)
	}
	close(finish)
	for i := 0; i < 2; i++ {
		w := <-result
		body, _ := io.ReadAll(w.Result().Body)
		if w.Code != 200 || !bytes.Contains(body, []byte(`"ok":true`)) {
			t.Fatal(w.Code, string(body))
		}
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate refresh")
	}
	func() {
		defer func() { _ = recover() }()
		s.serveRefresh(httptest.NewRecorder(), httptest.NewRequest("POST", "/admin/models/refresh", nil), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("test") }))
	}()
	if len(s.heavySlots) != 0 || len(s.refreshes) != 0 {
		t.Fatal("panic leaked admission")
	}
}

func TestModelPolicyAndInternalTestCannotBypassAdmission(t *testing.T) {
	client := &failingUpstream{err: errors.New("must not dispatch")}
	o, _ := newDNSFailoverOrch(t, client)
	s := NewServer(o)
	var leases []*modelLease
	for i := 0; i < 4; i++ {
		l, _ := s.modelsAdmission.acquire(context.Background(), "1")
		leases = append(leases, l)
		defer l.release()
	}
	s.modelsAdmission.wait = 5 * time.Millisecond
	// Unauthorized models must be rejected without spending any queue budget.
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"blocked","messages":[]}`))
	r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, &Principal{AppID: 1, AllowedModels: []string{"allowed"}}))
	w := httptest.NewRecorder()
	s.handleChat(w, r)
	if w.Code != 403 || s.modelsAdmission.snapshot()["queued"] != 0 {
		t.Fatal("policy checked after admission", w.Code)
	}
	// Public authentication runs before reading bodies/admission.
	w = httptest.NewRecorder()
	s.requestGuard(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("unauthenticated request reached handler") })).ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`)))
	if w.Code != 401 || s.bodies.usage() != 0 {
		t.Fatal("unauthenticated request used resources")
	}
	testMu.Lock()
	testRunning = false
	testLastDone = time.Time{}
	testMu.Unlock()
	t.Cleanup(func() { testMu.Lock(); testRunning = false; testLastDone = time.Time{}; testMu.Unlock() })
	w = httptest.NewRecorder()
	s.adminModelTest(w, httptest.NewRequest("POST", "/admin/models/test", strings.NewReader(`{"model":"test-model"}`)))
	if w.Code != 429 || w.Header().Get("Retry-After") == "" || client.calls != 0 {
		t.Fatal("internal test bypassed model pool", w.Code, client.calls)
	}
}

type hangingUpstream struct{ started chan struct{} }

func (h *hangingUpstream) StreamUpstream(ctx context.Context, _ map[string]string, _ map[string]any, _ string, yield upstream.LineFunc) error {
	if err := yield(`data: {"choices":[{"delta":{"content":"hello"},"index":0}]}`); err != nil {
		return err
	}
	h.started <- struct{}{}
	<-ctx.Done()
	return ctx.Err()
}

func TestRealSSEHandlersRespectAccountAndGlobalLimits(t *testing.T) {
	for _, accountLimit := range []int{2, 4} {
		t.Run(fmt.Sprint(accountLimit), func(t *testing.T) {
			o, _ := newDNSFailoverOrch(t, &failingUpstream{})
			o.cfg.WorkBuddyAccountConcurrency = accountLimit
			acc := o.pool.Accounts()[0]
			o.pool = pool.New(map[string]pool.Credential{acc.UID: catalogOffline{acc.Mgr}}, "")
			o.models = models.NewWithCatalogClient(o.pool, o.db, &http.Client{Transport: catalogTransport{}})
			o.models.Refresh() // Catalog transport returns fixture JSON without network access.

			h := &hangingUpstream{started: make(chan struct{}, 4)}
			o.upstreamClient = h
			s := NewServer(o)
			done := make(chan struct{}, 4)
			if _, err := o.db.CreateApp("test", o.hashKey("test-private-key"), "", "", "", "", 0); err != nil {
				t.Fatal(err)
			}
			var cancels []context.CancelFunc
			for i := 0; i < 4; i++ {
				ctx, cancel := context.WithCancel(context.Background())
				cancels = append(cancels, cancel)
				defer cancel()
				r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}],"stream":true}`)).WithContext(ctx)
				r.Header.Set("Authorization", "Bearer test-private-key")
				go func() {
					s.requestGuard(http.HandlerFunc(s.handleChat)).ServeHTTP(httptest.NewRecorder(), r)
					done <- struct{}{}
				}()
			}
			for i := 0; i < accountLimit; i++ {
				select {
				case <-h.started:
				case <-time.After(2 * time.Second):
					t.Fatal("stream never reached upstream")
				}
			}
			awaitQueue(t, s.modelsAdmission, 4-accountLimit)
			w := httptest.NewRecorder()
			s.requestGuard(http.HandlerFunc(s.adminOverview)).ServeHTTP(w, httptest.NewRequest("GET", "/admin/overview", nil))
			if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(fmt.Sprintf(`"running":%d`, accountLimit))) {
				t.Fatal("management unavailable", w.Code, w.Body.String())
			}
			for _, cancel := range cancels {
				cancel()
			}
			for i := 0; i < 4; i++ {
				<-done
			}
			if s.modelsAdmission.snapshot()["running"] != 0 || s.bodies.usage() != 0 {
				t.Fatal("disconnected streams leaked admission/body budget")
			}

		})
	}
}

func TestThirdKeyCannotStarve(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 1})
	first, _ := a.acquire(context.Background(), "1")
	got := make(chan *modelLease, 9)
	for _, key := range []string{"1", "1", "2", "2", "3", "3"} {
		go func(key string) {
			l, err := a.acquire(context.Background(), key)
			if err != nil {
				t.Error(err)
				return
			}
			got <- l
		}(key)
	}
	awaitQueue(t, a, 6)
	first.release()
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		l := <-got
		seen[l.t.key] = true
		if i == 2 && len(seen) != 3 {
			t.Error("three-key round robin starved a key", seen)
		}
		l.release()
	}
}

type catalogOffline struct{ pool.Credential }

func (catalogOffline) CatalogHeaders() (map[string]string, error) {
	return map[string]string{}, nil
}

type catalogTransport struct{}

func (catalogTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":{"models":[{"id":"test-model","name":"Test"}],"agents":[{"name":"cli","models":["test-model"]}]}}`)), Request: r}, nil
}

func TestLocalResponseLimitDoesNotPunishAccount(t *testing.T) {
	o, acc := newDNSFailoverOrch(t, &failingUpstream{emit: true})
	ctx := streamwatch.WithResponseLimit(context.Background(), 1)
	_, err := o.openUpstream(ctx, acc, map[string]any{}, "test-model", "", func(string) error { return nil }, nil)
	if !errors.Is(err, streamwatch.ErrResponseTooLarge) || o.pool.Get(acc.UID).FailureCount != 0 {
		t.Fatal("local output limit punished upstream account", err)
	}
}

func TestDownstreamWriteFailureCancelsWithoutAccountPenalty(t *testing.T) {
	o, acc := newDNSFailoverOrch(t, &failingUpstream{emit: true})
	send, ok := sseWriter(brokenWriter{header: make(http.Header)})
	if !ok {
		t.Fatal("flusher lost")
	}
	_, err := o.openUpstream(context.Background(), acc, map[string]any{}, "test-model", "", send, nil)
	if !errors.Is(err, context.Canceled) || o.pool.Get(acc.UID).FailureCount != 0 {
		t.Fatal("downstream failure punished account", err)
	}
}

type brokenWriter struct{ header http.Header }

func (w brokenWriter) Header() http.Header     { return w.header }
func (brokenWriter) WriteHeader(int)           {}
func (brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func (brokenWriter) Flush()                    {}

func TestJSONAllocationAmplificationBounded(t *testing.T) {
	if jsonWithinComplexity([]byte(`{"messages":[{},{},{},{},{}]}`), 8, 128) {
		t.Fatal("object amplification allowed")
	}
	if jsonWithinComplexity([]byte(`[[[[[]]]]]`), 100, 4) {
		t.Fatal("deep JSON allowed")
	}
	if !jsonWithinComplexity([]byte(`{"text":"{[\\\"braces\\\"]}:,,,","image":"long base64"}`), 10, 4) {
		t.Fatal("string punctuation counted as structure")
	}
}

func TestEveryModelProtocolUsesAdmission(t *testing.T) {
	o, _ := newDNSFailoverOrch(t, &failingUpstream{})
	s := NewServer(o)
	s.modelsAdmission.wait = 5 * time.Millisecond
	for i := 0; i < 4; i++ {
		l, _ := s.modelsAdmission.acquire(context.Background(), "1")
		defer l.release()
	}
	for _, tc := range []struct {
		path    string
		handler http.HandlerFunc
	}{{"/v1/chat/completions", s.handleChat}, {"/v1/messages", s.handleMessages}, {"/v1/responses", s.handleResponses}} {
		r := httptest.NewRequest("POST", tc.path, strings.NewReader(`{"model":"test-model","messages":[],"input":"hi"}`))
		r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, &Principal{AppID: 2}))
		w := httptest.NewRecorder()
		s.requestGuard(tc.handler).ServeHTTP(w, r)
		if w.Code != 429 || !bytes.Contains(w.Body.Bytes(), []byte(`"model_queue_timeout"`)) || w.Header().Get("Content-Type") != "application/json" {
			t.Fatal(tc.path, "bypassed admission or sent premature SSE", w.Code, w.Body.String())
		}
	}
}

func TestRuntimeDownstreamFailurePropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &cancelWriter{ResponseWriter: brokenWriter{header: make(http.Header)}, cancel: cancel}
	_, err := w.Write([]byte("test"))
	if err == nil || ctx.Err() != context.Canceled {
		t.Fatal("upstream context not canceled after write failure")
	}
}

// One UID is a single upstream resource even across different users, models,
// and private/shared pool membership. Busy resources must not hold server slots.
func TestAccountAdmissionUIDIsolationAlternativesAndCancellation(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 4, WorkBuddyAccountConcurrency: 2})
	first := &pool.Account{UID: "one"}
	second := &pool.Account{UID: "two"}
	choose := func(accounts ...*pool.Account) func(map[string]bool) (*pool.Account, *apiError) {
		return func(busy map[string]bool) (*pool.Account, *apiError) {
			for _, acc := range accounts {
				if !busy[acc.UID] {
					return acc, nil
				}
			}
			return nil, nil
		}
	}
	bind := func(key, model string, selector func(map[string]bool) (*pool.Account, *apiError)) *modelLease {
		l, err := a.acquireWithLimit(context.Background(), key, 2, model)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(l.release)
		if _, err = l.bindAccount(context.Background(), selector); err != nil {
			t.Fatal(err)
		}
		return l
	}
	one := bind("portal-user-1", "model-a", choose(first))
	private := bind("private", "model-b", choose(first))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blocked, _ := a.acquireWithLimit(ctx, "portal-user-2", 2, "model-c")
	result := make(chan error, 1)
	go func() { _, err := blocked.bindAccount(ctx, choose(first)); result <- err }()
	awaitQueue(t, a, 1)
	if a.snapshot()["running"] != 2 {
		t.Fatal("busy UID consumed execution slot", a.snapshot())
	}
	alternative := bind("portal-user-3", "model-a", choose(first, second))
	if alternative.t.account.UID != "two" {
		t.Fatal("idle alternative not used")
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	blocked.release()
	one.release()
	private.release()
	alternative.release()
	if s := a.snapshot(); s["running"] != 0 || s["queued"] != 0 || len(s["account_running"].(map[string]int)) != 0 {
		t.Fatal("permit leak", s)
	}
}

func TestAccountBindingDoesNotRejectRunnableWhenQueueFull(t *testing.T) {
	a := newModelAdmission(&config.Config{ModelQueueSize: 1})
	l, _ := a.acquire(context.Background(), "private")
	defer l.release()
	a.mu.Lock()
	a.enqueueLocked(&modelTicket{key: "parked", ready: false})
	a.mu.Unlock()
	if _, err := l.bindAccount(context.Background(), func(map[string]bool) (*pool.Account, *apiError) { return &pool.Account{UID: "free"}, nil }); err != nil {
		t.Fatal("free capacity rejected", err)
	}
}

func TestFairAdmissionPrefersUserWithoutExecution(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 4, PortalSharedConcurrency: 3})
	var leases []*modelLease
	for _, key := range []string{"portal-user-a", "portal-user-a", "portal-user-b", "private"} {
		l, err := a.acquireWithLimit(context.Background(), key, 3, "m")
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, l)
		defer l.release()
	}
	got := make(chan *modelLease, 2)
	go func() { l, _ := a.acquireWithLimit(context.Background(), "portal-user-a", 3, "m"); got <- l }()
	awaitQueue(t, a, 1)
	go func() { l, _ := a.acquireWithLimit(context.Background(), "portal-user-c", 3, "m"); got <- l }()
	awaitQueue(t, a, 2)
	leases[2].release()
	next := <-got
	if next.t.key != "portal-user-c" {
		t.Error("new user lost to another child agent", next.t.key)
	}
	next.release()
	(<-got).release()
}

func TestAccountSelectorUsesSameSiteAccountsAndLiveAuthorizedReadiness(t *testing.T) {
	o, original := newDNSFailoverOrch(t, &failingUpstream{})
	o.pool = pool.New(map[string]pool.Credential{"one": catalogOffline{original.Mgr}, "two": catalogOffline{original.Mgr}}, "")
	o.models = models.NewWithCatalogClient(o.pool, o.db, &http.Client{Transport: catalogTransport{}})
	o.models.Refresh()
	zero := 0.0
	o.pool.SetCredits("one", &zero, nil, nil, nil)
	exhausted, e := o.accountSelector("test-model", "one", nil, map[string]bool{"one": true, "two": true})
	if e != nil {
		t.Fatal(e)
	}
	if acc, e := exhausted(nil); e != nil || acc == nil || acc.UID != "two" {
		t.Fatal("preferred cooldown grace revived exhausted account", acc, e)
	}
	o.pool.SetCredits("one", nil, nil, nil, nil)
	selector, err := o.accountSelector("test-model", "one", nil, map[string]bool{"one": true, "two": true})
	if err != nil {
		t.Fatal(err)
	}
	if acc, err := selector(map[string]bool{"one": true}); err != nil || acc != nil {
		t.Fatal("busy preferred account lost affinity", acc, err)
	}
	other, err := o.accountSelector("test-model", "two", nil, map[string]bool{"one": true, "two": true})
	if err != nil {
		t.Fatal(err)
	}
	if acc, err := other(map[string]bool{"one": true}); err != nil || acc == nil || acc.UID != "two" {
		t.Fatal("same-site account wrongly coalesced", acc, err)
	}
	restricted, err := o.accountSelector("test-model", "one", nil, map[string]bool{"one": true})
	if err != nil {
		t.Fatal(err)
	}
	if acc, err := restricted(map[string]bool{"one": true}); acc != nil || err != nil {
		t.Fatal("scope escaped instead of waiting", acc, err)
	}
	o.modelCooldowns["two|test-model"] = cdEntry{until: nowSec() + 60}
	o.pool.OnFailure("one", 60)
	if acc, err := selector(nil); acc != nil || err == nil {
		t.Fatal("cooling accounts admitted", acc, err)
	}
}

func TestAccountPermitRetryAndThrottleFailureRecover(t *testing.T) {
	a := newModelAdmission(&config.Config{})
	l, _ := a.acquire(context.Background(), "private")
	defer l.release()
	for _, uid := range []string{"failed", "replacement"} {
		if _, err := l.bindAccount(context.Background(), func(map[string]bool) (*pool.Account, *apiError) { return &pool.Account{UID: uid}, nil }); err != nil {
			t.Fatal(err)
		}
		counts := a.snapshot()["account_running"].(map[string]int)
		if len(counts) != 1 || counts[uid] != 1 {
			t.Fatal("retry retained old permit", counts)
		}
	}
	if err := l.throttle(context.Background(), func(context.Context) error { return context.DeadlineExceeded }); err == nil {
		t.Fatal("lost wait failure")
	}
	l.release()
	if s := a.snapshot(); s["running"] != 0 || s["queued"] != 0 || len(s["account_running"].(map[string]int)) != 0 {
		t.Fatal("failed throttle leaked permit", s)
	}
}

func TestFiveSlotProfileSupportsFourUserAgentsAndPrivateReserve(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 5, PortalSharedConcurrency: 4})
	var leases []*modelLease
	defer func() {
		for _, l := range leases {
			l.release()
		}
	}()
	for i := 0; i < 4; i++ {
		l, err := a.acquireWithLimit(context.Background(), "portal-user-1", 4, "m")
		if err != nil {
			t.Fatal(err)
		}
		leases = append(leases, l)
		if _, err = l.bindAccount(context.Background(), func(map[string]bool) (*pool.Account, *apiError) { return &pool.Account{UID: "contributed"}, nil }); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		l, err := a.acquireWithLimit(ctx, "portal-user-2", 4, "m")
		if l != nil {
			l.release()
		}
		result <- err
	}()
	awaitQueue(t, a, 1)
	private, err := a.acquire(context.Background(), "private")
	if err != nil {
		t.Fatal(err)
	}
	leases = append(leases, private)
	if a.snapshot()["running"] != 5 {
		t.Fatal("private reserve unavailable", a.snapshot())
	}
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type capacityUpstream struct {
	started chan struct{}
	finish  chan struct{}
}

func (c *capacityUpstream) StreamUpstream(ctx context.Context, _ map[string]string, _ map[string]any, _ string, yield upstream.LineFunc) error {
	// Almost 8 MiB of wire response: retain nonstream aggregates concurrently.
	line := `data: {"choices":[{"delta":{"content":"` + strings.Repeat("x", 128<<10) + `"}}]}`
	for i := 0; i < 63; i++ {
		if err := yield(line); err != nil {
			return err
		}
	}
	c.started <- struct{}{}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.finish:
		return yield(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`)
	}
}
func TestFiveSlotCapacityNonstreamNearResponseLimit(t *testing.T) {
	o, acc := newDNSFailoverOrch(t, &failingUpstream{})
	o.cfg.MaxConcurrentRequests = 5
	o.cfg.PortalSharedConcurrency = 4
	// Single fixture UID deliberately permits 5 to exercise the global memory
	// ceiling. Production 4+1 spans at least two distinct accounts.
	o.cfg.WorkBuddyAccountConcurrency = 5
	o.cfg.UsageContentMaxBytes = 32768
	o.pool = pool.New(map[string]pool.Credential{acc.UID: catalogOffline{acc.Mgr}}, "")
	o.models = models.NewWithCatalogClient(o.pool, o.db, &http.Client{Transport: catalogTransport{}})
	o.models.Refresh()
	upstream := &capacityUpstream{started: make(chan struct{}, 5), finish: make(chan struct{})}
	o.upstreamClient = upstream
	s := NewServer(o)
	if _, err := o.db.CreateApp("capacity", o.hashKey("capacity-key"), "", "", "", "", 0); err != nil {
		t.Fatal(err)
	}
	var finish sync.Once
	defer finish.Do(func() { close(upstream.finish) })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result := make(chan int, 5)
	input := `{"model":"auto","messages":[{"role":"user","content":"` + strings.Repeat("a", 2<<20) + `"}],"stream":false}`
	for i := 0; i < 5; i++ {
		go func() {
			r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(input)).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer capacity-key")
			w := httptest.NewRecorder()
			s.requestGuard(http.HandlerFunc(s.handleChat)).ServeHTTP(w, r)
			result <- w.Code
		}()
		// Stagger body reads so five large uploads do not intentionally exceed
		// the shared 32 MiB transient-allocation budget. All aggregates overlap.
		select {
		case <-upstream.started:
		case code := <-result:
			t.Fatalf("request exited before five aggregates: %d", code)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if s.modelsAdmission.snapshot()["running"] != 5 {
		t.Fatal("five requests did not execute")
	}
	finish.Do(func() { close(upstream.finish) })
	for i := 0; i < 5; i++ {
		if code := <-result; code != 200 {
			t.Fatal(code)
		}
	}
	if s.modelsAdmission.snapshot()["running"] != 0 || s.bodies.usage() != 0 {
		t.Fatal("capacity not recovered")
	}
}

func TestAdaptiveBudgetAdmitsThreeUsersWithoutFixedFourSlotBottleneck(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 13, PortalSharedConcurrency: 12})
	var leases []*modelLease
	defer func() {
		for _, l := range leases {
			l.release()
		}
	}()
	for u := 0; u < 3; u++ {
		for child := 0; child < 4; child++ {
			l, err := a.acquireWithBudget(context.Background(), fmt.Sprintf("portal-user-%d", u), 4, "m", 16<<20)
			if err != nil {
				t.Fatal(err)
			}
			leases = append(leases, l)
			uid := fmt.Sprint(u)
			if _, err = l.bindAccount(context.Background(), func(map[string]bool) (*pool.Account, *apiError) { return &pool.Account{UID: uid}, nil }); err != nil {
				t.Fatal(err)
			}
		}
	}
	private, err := a.acquireWithBudget(context.Background(), "private", 0, "m", 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	leases = append(leases, private)
	if a.snapshot()["running"] != 13 || a.buffers != 224<<20 {
		t.Fatal("resource budget failed to expand", a.snapshot())
	}
	for _, l := range leases {
		l.release()
	}
	if a.buffers != 0 || a.sharedBuffers != 0 {
		t.Fatal("buffer reservation leak")
	}
}
func TestAdaptiveMemoryPressureRecoversWithoutCompletion(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 13, PortalSharedConcurrency: 12, ModelMemoryGuard: true})
	var pressure atomic.Uint64
	pressure.Store(a.memoryHigh)
	a.memoryUsage = func() uint64 { return pressure.Load() }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	got := make(chan *modelLease, 1)
	go func() {
		l, err := a.acquireWithBudget(ctx, "private", 0, "m", 16<<20)
		if err != nil {
			t.Error(err)
		}
		got <- l
	}()
	awaitQueue(t, a, 1)
	if a.snapshot()["running"] != 0 {
		t.Fatal("pressure ignored")
	}
	deadline := time.Now().Add(time.Second)
	var collected time.Time
	for time.Now().Before(deadline) {
		a.mu.Lock()
		collected = a.lastPressureGC
		a.mu.Unlock()
		if !collected.IsZero() {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if collected.IsZero() {
		t.Fatal("pressure never requested dead-object collection")
	}
	time.Sleep(300 * time.Millisecond)
	a.mu.Lock()
	repeated := !a.lastPressureGC.Equal(collected)
	a.mu.Unlock()
	if repeated {
		t.Fatal("pressure waiters triggered repeated collection")
	}
	pressure.Store(0)
	l := <-got
	if l == nil {
		t.Fatal("pressure queue never recovered")
	}
	l.release()
	if a.buffers != 0 || len(a.queue) != 0 {
		t.Fatal("pressure recovery leaked reservations")
	}
}

type discardResponse struct {
	header http.Header
	code   int
}

func (d *discardResponse) Header() http.Header  { return d.header }
func (d *discardResponse) WriteHeader(code int) { d.code = code }
func (d *discardResponse) Write(b []byte) (int, error) {
	if d.code == 0 {
		d.code = 200
	}
	return len(b), nil
}
func (d *discardResponse) Flush() {}
func TestAdaptiveProtocolCapacity(t *testing.T) {
	for _, route := range []string{"chat", "responses", "anthropic"} {
		for _, n := range []int{9, 13} {
			t.Run(fmt.Sprintf("%s/%d", route, n), func(t *testing.T) {
				o, acc := newDNSFailoverOrch(t, &failingUpstream{})
				o.cfg.MaxConcurrentRequests = 13
				o.cfg.PortalSharedConcurrency = 12
				o.cfg.ModelMemoryGuard = true
				// Single fake credential removes account routing as a variable in the
				// server-capacity test; real multitenant UID limits are tested separately.
				o.cfg.WorkBuddyAccountConcurrency = 13
				o.cfg.UsageContentMaxBytes = 32768
				o.pool = pool.New(map[string]pool.Credential{acc.UID: catalogOffline{acc.Mgr}}, "")
				o.models = models.NewWithCatalogClient(o.pool, o.db, &http.Client{Transport: catalogTransport{}})
				o.models.Refresh()
				c := &capacityUpstream{started: make(chan struct{}, n), finish: make(chan struct{})}
				o.upstreamClient = c
				s := NewServer(o)
				if _, err := o.db.CreateApp("capacity", o.hashKey("capacity-key"), "", "", "", "", 0); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				var finish sync.Once
				defer finish.Do(func() { close(c.finish) })
				result := make(chan int, n)
				payload := `{"model":"auto","messages":[{"role":"user","content":"` + strings.Repeat("a", 128<<10) + `"}],"input":"hello","stream":true,"max_tokens":16384}`
				handler := s.handleChat
				path := "/v1/chat/completions"
				if route == "responses" {
					handler = s.handleResponses
					path = "/v1/responses"
				}
				if route == "anthropic" {
					handler = s.handleMessages
					path = "/v1/messages"
				}
				for i := 0; i < n; i++ {
					go func() {
						r := httptest.NewRequest("POST", path, strings.NewReader(payload)).WithContext(ctx)
						r.Header.Set("Authorization", "Bearer capacity-key")
						w := &discardResponse{header: make(http.Header)}
						s.requestGuard(http.HandlerFunc(handler)).ServeHTTP(w, r)
						result <- w.code
					}()
					select {
					case <-c.started:
					case code := <-result:
						t.Fatalf("early exit %d", code)
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
				if s.modelsAdmission.snapshot()["running"] != n {
					t.Fatal("capacity stalled", s.modelsAdmission.snapshot())
				}
				finish.Do(func() { close(c.finish) })
				for i := 0; i < n; i++ {
					if code := <-result; code != 200 {
						t.Fatal(code)
					}
				}
				if s.modelsAdmission.buffers != 0 || s.bodies.usage() != 0 {
					t.Fatal("capacity leak")
				}
			})
		}
	}
}

func TestPublicBodySaturationPreservesProtectedIntake(t *testing.T) {
	s := NewServer(&Orchestrator{cfg: &config.Config{BodyReadConcurrency: 1}})
	for i := 0; i < cap(s.publicBodySlots); i++ {
		s.publicBodySlots <- struct{}{}
	}
	for i := 0; i < cap(s.publicBodyWaiting); i++ {
		s.publicBodyWaiting <- struct{}{}
	}
	if !s.publicBodies.reserve(s.publicBodies.limit) {
		t.Fatal("failed to saturate public budget")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /portal/api/auth/login", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("POST /protected", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	h := s.requestGuard(mux)
	public := httptest.NewRecorder()
	h.ServeHTTP(public, httptest.NewRequest("POST", "/portal/api/auth/login", strings.NewReader(`{}`)))
	if public.Code != 429 {
		t.Fatal("public intake was not bounded", public.Code)
	}
	protected := httptest.NewRecorder()
	h.ServeHTTP(protected, httptest.NewRequest("POST", "/protected", strings.NewReader(`{}`)))
	if protected.Code != 204 || s.bodies.usage() != 0 || len(s.bodySlots) != 0 {
		t.Fatal("public traffic exhausted protected intake", protected.Code)
	}
	for _, tc := range []struct {
		method, path string
		code         int
	}{{"POST", "/missing", 404}, {"POST", "//protected", 307}, {"POST", "/./protected", 307}, {"POST", "//portal/api/auth/login", 307}, {"POST", "/health", 405}, {"GET", "/health", 204}, {"HEAD", "/health", 204}} {
		body := &unreadAdmissionBody{t: t}
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Body = body
		r.ContentLength = -1
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("%s %s: %d", tc.method, tc.path, w.Code)
		}
		if tc.code == 405 && w.Header().Get("Allow") == "" {
			t.Fatal("method allowance lost")
		}
	}
}

type unreadAdmissionBody struct{ t *testing.T }

func (b *unreadAdmissionBody) Read([]byte) (int, error) {
	b.t.Error("unused body was read")
	return 0, io.EOF
}
func (*unreadAdmissionBody) Close() error { return nil }

type refreshCatalogTransport struct{ stage, calls int }

func (tr *refreshCatalogTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	tr.calls++
	// 双路目录（/v3/config + 插件目录）共享同一传输：stage 1 只让先拉的
	// /v3/config 失败一次，插件目录仍成功——这才构成「单路失败」语义；
	// 否则两路同失败会触发整路失败退避，不再保留旧映射。
	if tr.stage == 1 && tr.calls == 1 {
		return nil, errors.New("catalog temporarily unavailable")
	}
	if tr.stage == 1 && r.URL.Path != "/v3/config" {
		// 插件目录成功且不含 test-model：v3 失败回落旧快照 → Fresh 保留
		return &http.Response{StatusCode: 200, Header: make(http.Header),
			Body:    io.NopCloser(strings.NewReader(`{"data":{"models":[],"agents":[{"name":"cli","models":[]}]}}`)),
			Request: r}, nil
	}
	body := `{"data":{"models":[{"id":"test-model","name":"Fresh"}],"agents":[{"name":"cli","models":["test-model"]}]}}`
	if tr.stage == 0 {
		body = strings.ReplaceAll(body, "Fresh", "Old")
	}
	if tr.stage == 2 {
		body = `{"data":{"models":[],"agents":[{"name":"cli","models":[]}]}}`
	}
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}
func TestCatalogPartialFailurePreservesOnlyConfirmedMappings(t *testing.T) {
	o, original := newDNSFailoverOrch(t, &failingUpstream{})
	o.pool = pool.New(map[string]pool.Credential{"one": catalogOffline{original.Mgr}, "two": catalogOffline{original.Mgr}}, "")
	tr := &refreshCatalogTransport{}
	o.models = models.NewWithCatalogClient(o.pool, o.db, &http.Client{Transport: tr})
	o.models.Refresh()
	tr.stage, tr.calls = 1, 0
	o.models.Refresh()
	var found map[string]any
	for _, model := range o.models.ListCached() {
		if model["id"] == "test-model" {
			found = model
		}
	}
	if found == nil || found["name"] != "Fresh" || len(found["account_uids"].([]string)) != 2 {
		t.Fatal("partial refresh lost verified account mapping or fresh metadata", found)
	}
	tr.stage, tr.calls = 2, 0
	o.models.Refresh()
	// 双路语义下的「模型被撤销」判定：空目录是合法响应、覆盖成功快照，但单路
	// 失败会让另一来源的快照继续供应该模型——只有两路同时给空目录才算撤销。
	// 逐个账号清快照再刷新，等价于「两路从此都返回空」。
	o.models.ClearCatalogSnapshots()
	o.models.Refresh()
	for _, model := range o.models.ListCached() {
		if model["id"] == "test-model" {
			t.Fatal("valid empty catalog retained revoked model", model)
		}
	}
}

func TestAdmissionSnapshotExplainsWaitWithoutSelectingAccounts(t *testing.T) {
	a := newModelAdmission(&config.Config{MaxConcurrentRequests: 4})
	a.queue = []*modelTicket{{key: "paced", limit: 4}, {key: "limited", limit: 1, ready: true}, {key: "buffer", limit: 4, ready: true, bufferBytes: a.bufferBudget + 1}, {key: "account", limit: 4, ready: true, chooseAccount: func(map[string]bool) (*pool.Account, *apiError) {
		t.Fatal("snapshot invoked account selection")
		return nil, nil
	}}}
	a.byKey["limited"] = 1
	reasons := a.snapshot()["waiting_reasons"].(map[string]int)
	if reasons["upstream_pacing"] != 1 || reasons["key_or_user_limit"] != 1 || reasons["response_buffer_budget"] != 1 || reasons["account_busy"] != 1 {
		t.Fatal(reasons)
	}
	a.active = a.capacity
	reasons = a.snapshot()["waiting_reasons"].(map[string]int)
	if reasons["execution_limit"] != 1 || reasons["account_busy"] != 0 {
		t.Fatal(reasons)
	}
}
