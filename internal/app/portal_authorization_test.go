package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"work2api/internal/portal/portalauth"
	"work2api/internal/store"
	"work2api/internal/workbuddy/ratelimit"
)

func portalAuthorizeFixture(t *testing.T) (*Server, *store.User, int64, string) {
	t.Helper()
	s, u := portalFixture(t)
	for _, uid := range []string{"test-account", "second-account"} {
		if _, err := s.o.db.CreateContribution(u.ID, uid, "workbuddy", "codebuddy", "active"); err != nil {
			t.Fatal(err)
		}
	}
	key := "portal-authorization-key"
	id, err := s.o.db.CreateApp("key", s.o.hashKey(key), "portal", "", "", `["model-a","model-b"]`, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, u, id, key
}
func portalGrant(t *testing.T, s *Server, u *store.User, name, uid string, models []string) int64 {
	t.Helper()
	raw, _ := json.Marshal(models)
	id, err := s.o.db.CreateResourceGroup(name, "workbuddy", string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.o.db.AddGroupAccount(id, uid); err != nil {
		t.Fatal(err)
	}
	if err := s.o.db.GrantGroup(u.ID, id); err != nil {
		t.Fatal(err)
	}
	if err := s.o.db.SetGroupEnabled(id, true); err != nil {
		t.Fatal(err)
	}
	return id
}
func portalKeyPrincipal(t *testing.T, s *Server, key string) *Principal {
	t.Helper()
	p, e := s.o.checkAPIKey("Bearer "+key, "")
	if e != nil {
		t.Fatal(e.body)
	}
	return p
}
func TestPortalAuthorizationKeepsPerModelGroupScope(t *testing.T) {
	s, u, _, key := portalAuthorizeFixture(t)
	portalGrant(t, s, u, "group-a", "test-account", []string{"model-a"})
	portalGrant(t, s, u, "group-b", "second-account", []string{"model-b"})
	for model, uid := range map[string]string{"model-a": "test-account", "model-b": "second-account"} {
		p := portalKeyPrincipal(t, s, key)
		if err := s.prepareModel(p, map[string]any{"model": model}); err != nil {
			t.Fatal(err.body)
		}
		if len(p.AccountScope) != 1 || !p.AccountScope[uid] {
			t.Fatalf("%s widened to %+v", model, p.AccountScope)
		}
	}
}
func TestPortalCanonicalAliasDisabledDangerousAndEmptyKeyPolicies(t *testing.T) {
	s, u, id, key := portalAuthorizeFixture(t)
	portalGrant(t, s, u, "group", "test-account", []string{"friendly", "model-b", "other-provider", "automatic"})
	if err := s.o.db.SaveSettings(map[string]string{"model_aliases": "friendly=model-a\nother-provider=qoder/secret\nautomatic=auto", "portal_disabled_models": `["friendly"]`}); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"friendly", "model-a", "other-provider", "qoder/secret", "automatic", "auto"} {
		p := portalKeyPrincipal(t, s, key)
		if err := s.prepareModel(p, map[string]any{"model": model}); err == nil {
			t.Fatal("blocked model accepted", model)
		}
	}
	if err := s.o.db.SetAppModels(id, `[]`); err != nil {
		t.Fatal(err)
	}
	p := portalKeyPrincipal(t, s, key)
	if err := s.prepareModel(p, map[string]any{"model": "model-b"}); err == nil {
		t.Fatal("empty portal key became unrestricted")
	}
	if err := s.o.db.SetAppModels(id, `["friendly"]`); err != nil {
		t.Fatal(err)
	}
	if err := s.o.db.SaveSettings(map[string]string{"portal_disabled_models": "[]"}); err != nil {
		t.Fatal(err)
	}
	p = portalKeyPrincipal(t, s, key)
	payload := map[string]any{}
	if err := s.prepareModel(p, payload); err != nil {
		t.Fatal("unique canonical group/key intersection rejected", err.body)
	}
	if payload["model"] != "model-a" {
		t.Fatal(payload)
	}
}
func TestPortalQueuedRequestRefreshesAfterAdmission(t *testing.T) {
	s, u, _, key := portalAuthorizeFixture(t)
	group := portalGrant(t, s, u, "group", "test-account", []string{"model-a"})
	p := portalKeyPrincipal(t, s, key)
	payload := map[string]any{"model": "model-a"}
	if err := s.prepareModel(p, payload); err != nil {
		t.Fatal(err.body)
	}
	s.modelsAdmission.capacity = 1
	held, err := s.modelsAdmission.acquire(context.Background(), "held")
	if err != nil {
		t.Fatal(err)
	}
	defer held.release()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.Header.Set("Authorization", "Bearer "+key)
		r, release, ok := s.admitModel(w, r, p)
		defer release()
		if ok {
			s.refreshPortalRequest(w, r, p, payload)
		}
		done <- w
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.modelsAdmission.mu.Lock()
		queued := len(s.modelsAdmission.queue)
		s.modelsAdmission.mu.Unlock()
		if queued == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request never entered queue")
		}
		time.Sleep(time.Millisecond)
	}
	if err := s.o.db.RevokeGroup(u.ID, group); err != nil {
		t.Fatal(err)
	}
	held.release()
	select {
	case w := <-done:
		if w.Code != 403 {
			t.Fatal(w.Code, w.Body.String())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("queued request did not release")
	}
	s.modelsAdmission.mu.Lock()
	defer s.modelsAdmission.mu.Unlock()
	if s.modelsAdmission.active != 0 || len(s.modelsAdmission.queue) != 0 {
		t.Fatal("admission leaked")
	}
}
func TestPortalRunOnceRechecksAfterAccountThrottle(t *testing.T) {
	s, u, _, key := portalAuthorizeFixture(t)
	portalGrant(t, s, u, "group", "test-account", []string{"model-a"})
	p := portalKeyPrincipal(t, s, key)
	payload := map[string]any{"model": "model-a"}
	if err := s.prepareModel(p, payload); err != nil {
		t.Fatal(err.body)
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	r, ok := s.refreshPortalRequest(w, r, p, payload)
	if !ok {
		t.Fatal(w.Body.String())
	}
	lease, err := s.modelsAdmission.acquire(context.Background(), "throttle-test")
	if err != nil {
		t.Fatal(err)
	}
	defer lease.release()
	r = r.WithContext(context.WithValue(r.Context(), modelLeaseKey{}, lease))
	s.o.cfg.Ratelimit = true
	limit := ratelimit.New(150*time.Millisecond, 0)
	if !limit.Try() {
		t.Fatal("first grant")
	}
	s.o.limiters = map[string]*ratelimit.Limiter{"test-account": limit}
	done := make(chan error, 1)
	go func() {
		_, err := s.o.runOnce(r.Context(), s.o.pool.Get("test-account"), payload, func(string) error { return nil })
		done <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		s.modelsAdmission.mu.Lock()
		parked := s.modelsAdmission.active == 0 && len(s.modelsAdmission.queue) == 1
		s.modelsAdmission.mu.Unlock()
		if parked {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("request never parked for account throttle")
		}
		time.Sleep(time.Millisecond)
	}
	if err := s.o.db.SetUserStatus(u.ID, "disabled"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if _, ok := err.(*apiError); !ok {
			t.Fatal("revoked user reached upstream", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("throttled request stuck")
	}
	if s.o.upstreamClient.(*failingUpstream).calls != 0 {
		t.Fatal("upstream called after revoke")
	}
}
func adminLoginCookie(t *testing.T, s *Server, body string) *http.Cookie {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleAdminLogin(w, httptest.NewRequest("POST", "/admin/login", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != cookieName {
		t.Fatal(cookies)
	}
	return cookies[0]
}
func TestAdminPasswordRoleCookieIsolationRevocationAndRecovery(t *testing.T) {
	s, u := portalFixture(t)
	s.o.cfg.AdminToken = "private-recovery-token"
	adminID, err := portalauth.CreateUser(s.o.db, "administrator", "adminpassword", "admin")
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.handleAdminLogin(w, httptest.NewRequest("POST", "/admin/login", strings.NewReader(`{"username":"portal-user","password":"oldpassword"}`)))
	if w.Code != 401 {
		t.Fatal("ordinary user received admin session", w.Code, w.Body.String())
	}
	_, portalToken, err := portalauth.Login(s.o.db, u.Username, "oldpassword")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/admin/portal/users", nil)
	r.AddCookie(&http.Cookie{Name: portalauth.UserCookieName, Value: portalToken})
	if ok, _ := s.adminAuth(r); ok {
		t.Fatal("portal cookie authenticated admin")
	}
	cookie := adminLoginCookie(t, s, `{"username":"administrator","password":"adminpassword"}`)
	r = httptest.NewRequest("GET", "/portal/api/me", nil)
	r.AddCookie(cookie)
	if _, err := s.portalUser(r); err == nil {
		t.Fatal("admin cookie authenticated portal")
	}
	check := func(c *http.Cookie) bool {
		r := httptest.NewRequest("GET", "/admin/portal/users", nil)
		r.AddCookie(c)
		ok, _ := s.adminAuth(r)
		return ok
	}
	if !check(cookie) {
		t.Fatal("admin password login failed")
	}
	hash, err := portalauth.HashPassword("changedpassword")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.o.db.UpdateUserPassword(adminID, hash); err != nil {
		t.Fatal(err)
	}
	if check(cookie) {
		t.Fatal("password reset failed to revoke admin cookie")
	}
	cookie = adminLoginCookie(t, s, `{"username":"administrator","password":"changedpassword"}`)
	if err := s.o.db.SetUserStatus(adminID, "disabled"); err != nil {
		t.Fatal(err)
	}
	if check(cookie) {
		t.Fatal("disabled admin cookie remained valid")
	}
	recovery := adminLoginCookie(t, s, `{"token":"private-recovery-token"}`)
	if !check(recovery) {
		t.Fatal("token recovery stopped working")
	}
}

func TestPortalQuotaDisabledPreservesPayloadAndEnabledFailsClosed(t *testing.T) {
	s, u := portalFixture(t)
	p := &Principal{UserID: u.ID}
	payload := map[string]any{"model": "model-a", "messages": []any{}, "max_tokens": "invalid-scalar"}
	before, _ := json.Marshal(payload)
	w := httptest.NewRecorder()
	if !s.reservePortalBudget(w, p, payload) || p.quota != nil {
		t.Fatal("disabled quota altered request", w.Code, w.Body.String())
	}
	after, _ := json.Marshal(payload)
	if string(before) != string(after) {
		t.Fatal("disabled quota changed payload", payload)
	}
	s.o.cfg.PortalDailyRequests = 1
	w = httptest.NewRecorder()
	if s.reservePortalBudget(w, p, payload) || w.Code != 400 {
		t.Fatal("invalid output scalar accepted", w.Code, w.Body.String())
	}
	if err := s.o.db.Close(); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	if s.reservePortalBudget(w, p, map[string]any{"model": "model-a", "max_tokens": float64(16)}) || w.Code != 503 {
		t.Fatal("database quota error failed open", w.Code, w.Body.String())
	}
}
func TestPortalDispatchRejectsAliasTargetChangeAfterValidation(t *testing.T) {
	s, u, id, key := portalAuthorizeFixture(t)
	portalGrant(t, s, u, "group", "test-account", []string{"model-a", "model-b"})
	if err := s.o.db.SetAppModels(id, `["friendly","model-b"]`); err != nil {
		t.Fatal(err)
	}
	if err := s.o.db.SaveSettings(map[string]string{"model_aliases": "friendly=model-a"}); err != nil {
		t.Fatal(err)
	}
	p := portalKeyPrincipal(t, s, key)
	payload := map[string]any{"model": "friendly"}
	if err := s.prepareModel(p, payload); err != nil {
		t.Fatal(err.body)
	}
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	r, ok := s.refreshPortalRequest(w, r, p, payload)
	if !ok {
		t.Fatal(w.Body.String())
	}
	if err := s.o.db.SaveSettings(map[string]string{"model_aliases": "friendly=model-b"}); err != nil {
		t.Fatal(err)
	}
	_, err := s.o.runOnce(r.Context(), s.o.pool.Get("test-account"), map[string]any{"model": "model-a"}, func(string) error { return nil })
	if _, ok := err.(*apiError); !ok {
		t.Fatal("stale alias target dispatched", err)
	}
	if s.o.upstreamClient.(*failingUpstream).calls != 0 {
		t.Fatal("stale model reached upstream")
	}
}
