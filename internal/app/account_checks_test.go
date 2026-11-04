package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"work2api/internal/core/provider"
)

type accountCheckerProbe struct {
	registryProbe
	mu       sync.Mutex
	revision string
	fn       func(context.Context) provider.AccountCheck
	calls    atomic.Int32
	wrong    atomic.Int32
}

func (p *accountCheckerProbe) Ready() bool { return true }
func (p *accountCheckerProbe) Models(context.Context) []provider.CatalogModel {
	return []provider.CatalogModel{{ID: "check-probe/m", Extra: map[string]any{"account_uids": []string{"target"}}}}
}
func (p *accountCheckerProbe) CheckAccountIDs() []string { return []string{"target"} }
func (p *accountCheckerProbe) CheckAccountIdentity(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if id != "target" {
		return ""
	}
	return p.revision
}
func (p *accountCheckerProbe) CheckAccount(ctx context.Context, id string, force bool) provider.AccountCheck {
	p.calls.Add(1)
	if p.fn != nil {
		return p.fn(ctx)
	}
	return provider.Checked("valid", "只读授权", "ok")
}
func (p *accountCheckerProbe) Serve(ctx context.Context, req provider.ServeRequest) (provider.UsageReport, error) {
	if provider.CheckDispatch(ctx, provider.AccountRef{Provider: p.Name(), LocalID: "other"}, "check-probe/m") == nil {
		p.wrong.Add(1)
	}
	if err := provider.CheckDispatch(ctx, provider.AccountRef{Provider: p.Name(), LocalID: "target"}, "check-probe/m"); err != nil {
		return provider.UsageReport{}, err
	}
	if !provider.CurrentInvocation(ctx).Diagnostic {
		return provider.UsageReport{}, context.Canceled
	}
	req.Writer.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"content":"OK"}}]}`))
	return provider.UsageReport{Status: "ok", AccountUID: "target"}, nil
}
func newAccountCheckerProbe() *accountCheckerProbe {
	return &accountCheckerProbe{registryProbe: registryProbe{name: "check-probe", prefix: "check-probe/"}, revision: "first"}
}

func TestAccountIdentityPreviewRequiresAdmin(t *testing.T) {
	s, _ := portalFixture(t)
	s.o.cfg.AdminToken = "identity-admin-secret"
	uid := s.o.wb.Pool.Accounts()[0].UID
	h := s.Handler()
	request := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://127.0.0.1/admin/providers/workbuddy/accounts/"+uid+"/identity", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request(""); w.Code != 401 {
		t.Fatal("unauthenticated identity access", w.Code)
	}
	w := request(s.o.cfg.AdminToken)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), `"local_only":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestAccountCheckCoalescingReuseAndCancellation(t *testing.T) {
	p := newAccountCheckerProbe()
	var b accountCheckBook
	entered, release := make(chan struct{}), make(chan struct{})
	p.fn = func(ctx context.Context) provider.AccountCheck {
		close(entered)
		<-release
		return provider.Checked("valid", "只读授权", "ok")
	}
	done := make(chan provider.AccountCheck, 1)
	go func() { done <- b.run(context.Background(), p, p, "target", true, 30*time.Minute) }()
	<-entered
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result := b.run(ctx, p, p, "target", true, 30*time.Minute); result.Status != "unknown" {
		t.Fatal(result)
	}
	close(release)
	if result := <-done; result.Status != "valid" {
		t.Fatal(result)
	}
	b.run(context.Background(), p, p, "target", false, 30*time.Minute)
	if p.calls.Load() != 1 || len(b.slots) != 0 {
		t.Fatal("probe duplicated or slot leaked", p.calls.Load())
	}
}
func TestAccountCheckRejectsOldCredentialResults(t *testing.T) {
	p := newAccountCheckerProbe()
	var b accountCheckBook
	entered, release := make(chan struct{}), make(chan struct{})
	p.fn = func(context.Context) provider.AccountCheck {
		close(entered)
		<-release
		return provider.Checked("valid", "只读授权", "ok")
	}
	done := make(chan provider.AccountCheck, 1)
	go func() { done <- b.run(context.Background(), p, p, "target", true, 30*time.Minute) }()
	<-entered
	p.mu.Lock()
	p.revision = "second"
	p.mu.Unlock()
	close(release)
	if result := <-done; result.Status != "unknown" {
		t.Fatal("stale authorization reported healthy", result)
	}
	if check, _ := b.snapshot(p, p, "target"); check != nil {
		t.Fatal("stale result published")
	}
}
func TestAccountCheckTimeoutBacksOffAndPreservesTest(t *testing.T) {
	p := newAccountCheckerProbe()
	var b accountCheckBook
	b.saveTest(p, p, "target", checkIdentity(p, p, "target"), provider.Checked("valid", "指定模型", "ok"))
	p.fn = func(ctx context.Context) provider.AccountCheck {
		<-ctx.Done()
		return provider.Checked("warning", "只读授权", "timeout")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	b.run(ctx, p, p, "target", true, 30*time.Minute)
	b.run(context.Background(), p, p, "target", false, 30*time.Minute)
	check, test := b.snapshot(p, p, "target")
	if check == nil || check.Status != "warning" || test == nil || p.calls.Load() != 1 || len(b.slots) != 0 {
		t.Fatal("timeout retried, lost separate test, or slot leak", check, test, p.calls.Load())
	}
}
func TestAccountTestPinsIdentityAndValidatesInput(t *testing.T) {
	o, _ := newDNSFailoverOrch(t, &failingUpstream{})
	p := newAccountCheckerProbe()
	if err := o.runtimes.Register(p); err != nil {
		t.Fatal(err)
	}
	s := NewServer(o)
	testMu.Lock()
	oldDone := testLastDone
	testLastDone = time.Time{}
	testMu.Unlock()
	defer func() { testMu.Lock(); testLastDone = oldDone; testMu.Unlock() }()
	request := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/providers/check-probe/accounts/target/test", strings.NewReader(body))
		req.SetPathValue("name", p.Name())
		req.SetPathValue("id", "target")
		w := httptest.NewRecorder()
		s.adminAccountTest(w, req)
		return w
	}
	for _, body := range []string{`{"model":"workbuddy/m"}`, `{"model":"check-probe/m","unexpected":true}`, `{} {}`} {
		if w := request(body); w.Code != 400 {
			t.Fatal("invalid diagnostic request accepted", w.Code, w.Body.String())
		}
	}
	w := request(`{"model":"check-probe/m"}`)
	var result provider.AccountCheck
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || w.Code != 200 || result.Status != "valid" || p.wrong.Load() != 0 {
		t.Fatal("test escaped target or failed", w.Code, w.Body.String(), err)
	}
	if _, test := o.accountChecks.snapshot(p, p, "target"); test == nil {
		t.Fatal("test result missing")
	}
}
func TestAccountCheckScheduleCanBeDisabled(t *testing.T) {
	p := newAccountCheckerProbe()
	o := &Orchestrator{runtimes: provider.NewRegistry()}
	if err := o.runtimes.Register(p); err != nil {
		t.Fatal(err)
	}
	o.checkAccountsTick(context.Background(), map[string]string{"account_check_enabled": "0"})
	if p.calls.Load() != 0 {
		t.Fatal("disabled scheduler made upstream request")
	}
	o.checkAccountsTick(context.Background(), map[string]string{})
	o.checkAccountsTick(context.Background(), map[string]string{})
	if p.calls.Load() != 1 {
		t.Fatal("scheduled check ignored cached freshness", p.calls.Load())
	}
}

func TestCheckSnapshotReadNeverStartsUpstreamProbe(t *testing.T) {
	p := newAccountCheckerProbe()
	o := &Orchestrator{runtimes: provider.NewRegistry()}
	if err := o.runtimes.Register(p); err != nil {
		t.Fatal(err)
	}
	s := &Server{o: o}
	w := httptest.NewRecorder()
	s.adminAccountCheckSnapshots(w, httptest.NewRequest("GET", "/admin/account-checks", nil))
	if w.Code != 200 || p.calls.Load() != 0 {
		t.Fatal("opening account page probed upstream")
	}
}
func TestCheckSettingsRejectInvalidIntervals(t *testing.T) {
	o, _ := newDNSFailoverOrch(t, &failingUpstream{})
	s := NewServer(o)
	for _, body := range []string{`{"account_check_interval_min":"0"}`, `{"account_check_interval_min":"1441"}`, `{"account_check_enabled":"yes"}`} {
		w := httptest.NewRecorder()
		s.adminSaveSettings(w, httptest.NewRequest("POST", "/admin/settings", strings.NewReader(body)))
		if w.Code != 400 {
			t.Fatal("invalid check configuration accepted", body, w.Code)
		}
	}
}

func TestCheckIntervalChangeAppliesToExistingEvidence(t *testing.T) {
	p := newAccountCheckerProbe()
	var b accountCheckBook
	p.fn = func(ctx context.Context) provider.AccountCheck {
		r := provider.Checked("valid", "只读授权", "ok")
		if p.calls.Load() == 1 {
			r.CheckedAt = time.Now().Add(-10 * time.Minute).Unix()
		} else if provider.AccountCheckMaxAge(ctx) != 5*time.Minute {
			t.Error("channel did not receive selected freshness interval")
		}
		return r
	}
	b.run(context.Background(), p, p, "target", false, 30*time.Minute)
	b.run(context.Background(), p, p, "target", false, 5*time.Minute)
	if p.calls.Load() != 2 {
		t.Fatal("shortened interval did not recheck existing evidence")
	}
}
