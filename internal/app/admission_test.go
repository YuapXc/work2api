package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
	a := newModelAdmission(&config.Config{})
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
	s := NewServer(&Orchestrator{cfg: &config.Config{}})
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

func TestRefreshSingleFlightAndPanicCleanup(t *testing.T) {
	s := NewServer(&Orchestrator{cfg: &config.Config{}})
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

func TestFourRealSSEHandlersLeaveManagementUsable(t *testing.T) {
	o, _ := newDNSFailoverOrch(t, &failingUpstream{})
	acc := o.pool.Accounts()[0]
	o.pool = pool.New(map[string]pool.Credential{acc.UID: catalogOffline{acc.Mgr}}, "")
	o.models = models.NewWithCatalogClient(o.pool, o.db, &http.Client{Transport: catalogTransport{}})
	o.models.Refresh() // Catalog transport returns fixture JSON without network access.

	h := &hangingUpstream{started: make(chan struct{}, 4)}
	o.upstreamClient = h
	s := NewServer(o)
	done := make(chan struct{}, 4)
	var cancels []context.CancelFunc
	for i := 0; i < 4; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		defer cancel()
		ctx = context.WithValue(ctx, principalContextKey{}, &Principal{AppID: 1, AppName: "test"})
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}],"stream":true}`)).WithContext(ctx)
		go func() {
			s.requestGuard(http.HandlerFunc(s.handleChat)).ServeHTTP(httptest.NewRecorder(), r)
			done <- struct{}{}
		}()
	}
	for i := 0; i < 4; i++ {
		select {
		case <-h.started:
		case <-time.After(2 * time.Second):
			t.Fatal("stream never reached upstream")
		}
	}
	w := httptest.NewRecorder()
	s.requestGuard(http.HandlerFunc(s.adminOverview)).ServeHTTP(w, httptest.NewRequest("GET", "/admin/overview", nil))
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"running":4`)) {
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
