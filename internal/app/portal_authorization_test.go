package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"work2api/internal/portal/portalauth"
	"work2api/internal/store"
	"work2api/internal/workbuddy/models"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/ratelimit"
)

func TestManagedAdminHTTPPasswordGateAndRoleEpoch(t *testing.T) {
	s, user := portalFixture(t)
	s.o.cfg.AdminToken = "identity-recovery"
	hash, err := portalauth.HashPassword("ownerpassword")
	if err != nil {
		t.Fatal(err)
	}
	ownerID, err := s.o.db.CreateInitialAdmin("owner", hash)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	s.mountIdentity(mux)
	mux.HandleFunc("GET /admin/protected", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	handler := s.adminGuard(mux)
	request := func(method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	ownerCookie := adminLoginCookie(t, s, `{"username":"owner","password":"ownerpassword"}`)
	if err := s.o.attachPortalScope(&Principal{UserID: ownerID}); err == nil {
		t.Fatal("owner identity granted portal calling eligibility")
	}
	w := request("POST", "/admin/portal/users", `{"username":"New-Admin","password":"temporarypass","role":"admin"}`, ownerCookie)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	staff, _ := s.o.db.GetUserByUsername("new-admin")
	if staff == nil || !staff.MustChangePassword {
		t.Fatal("missing normalized temporary admin")
	}
	cookie := adminLoginCookie(t, s, `{"username":"NEW-ADMIN","password":"temporarypass"}`)
	if w := request("GET", "/admin/protected", "", cookie); w.Code != 403 {
		t.Fatal("temporary password accessed management", w.Code)
	}
	if w := request("GET", "/admin/identity", "", cookie); w.Code != 200 || !strings.Contains(w.Body.String(), `"must_change_password":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("POST", "/admin/identity/password", `{"old_password":"temporarypass","new_password":"temporarypass"}`, cookie); w.Code != 400 {
		t.Fatal("same temporary password accepted")
	}
	if w := request("POST", "/admin/identity/password", `{"old_password":"temporarypass","new_password":"personalpassword"}`, cookie); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("GET", "/admin/protected", "", cookie); w.Code != 401 {
		t.Fatal("old admin cookie survived password change")
	}
	cookie = adminLoginCookie(t, s, `{"username":"new-admin","password":"personalpassword"}`)
	if w := request("GET", "/admin/protected", "", cookie); w.Code != 200 {
		t.Fatal("completed admin unable to manage")
	}
	if w := request("POST", "/admin/portal/users", `{"username":"illegal-admin","password":"temporarypass","role":"admin"}`, cookie); w.Code == 200 {
		t.Fatal("ordinary administrator appointed administrator")
	}
	if w := request("POST", "/admin/portal/users/"+fmt.Sprint(ownerID)+"/role", `{"role":"user"}`, cookie); w.Code == 200 {
		t.Fatal("ordinary administrator demoted owner")
	}
	// Owner demotes and promotes without changing the password. Old cookies must not revive.
	path := "/admin/portal/users/" + fmt.Sprint(staff.ID) + "/role"
	for _, role := range []string{"user", "admin"} {
		if w := request("POST", path, `{"role":"`+role+`"}`, ownerCookie); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := request("GET", "/admin/protected", "", cookie); w.Code != 401 {
		t.Fatal("old role epoch revived")
	}
	ownerActor := store.IdentityActor{ID: ownerID, Method: "password"}
	if err := s.o.db.ManageIdentity(ownerActor, user.ID, "password", hash); err != nil {
		t.Fatal(err)
	}
	_, token, err := portalauth.Login(s.o.db, user.Username, "ownerpassword")
	if err != nil {
		t.Fatal(err)
	}
	portalHandler := s.portalGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	for path, want := range map[string]int{"/portal/api/me": 200, "/portal/api/keys": 403} {
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: portalauth.UserCookieName, Value: token})
		w := httptest.NewRecorder()
		portalHandler.ServeHTTP(w, r)
		if w.Code != want {
			t.Fatal(path, w.Code)
		}
	}
	if err := s.o.attachPortalScope(&Principal{UserID: user.ID}); err == nil {
		t.Fatal("temporary credentials bypassed API Key protection")
	}
	cookie = adminLoginCookie(t, s, `{"username":"new-admin","password":"personalpassword"}`)
	s.sessions.mu.Lock()
	entry := s.sessions.sessions[cookie.Value]
	entry.verifiedAt = time.Now().Add(-6 * time.Minute)
	s.sessions.sessions[cookie.Value] = entry
	s.sessions.mu.Unlock()
	create := `{"username":"managed-user","password":"temporarypass","role":"user"}`
	if w := request("POST", "/admin/portal/users", create, cookie); w.Code != 403 {
		t.Fatal("expired step-up accepted")
	}
	if w := request("POST", "/admin/identity/reauth", `{"password":"wrong"}`, cookie); w.Code != 401 {
		t.Fatal("wrong reauthentication proof accepted")
	}
	if w := request("POST", "/admin/identity/reauth", `{"password":"personalpassword"}`, cookie); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for i := 0; i < loginWindowMax+1; i++ {
		if w := request("POST", "/admin/identity/reauth", `{"password":"personalpassword"}`, cookie); w.Code != 200 {
			t.Fatal("successful step-up verification incorrectly locked out operator", i, w.Code)
		}
	}
	if w := request("POST", "/admin/portal/users", create, cookie); w.Code != 200 {
		t.Fatal("ordinary admin could not create ordinary user", w.Code, w.Body.String())
	}
}

func TestAdminSessionCapsArePerIdentityAndBoundedGlobally(t *testing.T) {
	sessions := newAdminSessionManager()
	protected := sessions.createForUser(1, "hash")
	for i := 0; i < maxSessionsPerAdmin+3; i++ {
		sessions.createForUser(2, "other")
	}
	if !sessions.validate(protected) {
		t.Fatal("another identity evicted active session below global bound")
	}
	count := 0
	for _, entry := range sessions.sessions {
		if entry.userID == 2 {
			count++
		}
	}
	if count != maxSessionsPerAdmin {
		t.Fatal("per-identity cap", count)
	}
	for i := 3; i < maxSessions+20; i++ {
		sessions.createForUser(int64(i), "hash")
	}
	if len(sessions.sessions) != maxSessions {
		t.Fatal("unbounded admin sessions", len(sessions.sessions))
	}
}

func TestNewContributionCatalogVerificationAndScopeAffinity(t *testing.T) {
	s, owner := portalFixture(t)
	acc := s.o.pool.Accounts()[0]
	s.o.pool = pool.New(map[string]pool.Credential{acc.UID: catalogOffline{acc.Mgr}}, "")
	s.o.models = models.NewWithCatalogClient(s.o.pool, s.o.db, &http.Client{Transport: catalogTransport{}})
	if _, err := s.o.db.CreateContribution(owner.ID, acc.UID, "workbuddy", "codebuddy", "active"); err != nil {
		t.Fatal(err)
	}
	if len(s.portalAvailableModels(owner.ID)) != 0 {
		t.Fatal("unverified account got models")
	}
	if !s.o.models.RefreshAccount(context.Background(), acc.UID) {
		t.Fatal("account catalog not verified")
	}
	if !s.portalAvailableModels(owner.ID)["test-model"] {
		t.Fatal("verified model not available immediately")
	}
	key := principalSessionKey(&Principal{UserID: owner.ID}, "test-model", map[string]any{"prompt_cache_key": "chat"})
	scope := map[string]bool{acc.UID: true}
	if _, err := s.o.pickInScope("test-model", key, nil, scope); err != nil {
		t.Fatal(err)
	}
	if s.o.sessions.lookup(key) != acc.UID {
		t.Fatal("scoped account not bound")
	}
	if _, err := s.o.pickInScope("test-model", key, nil, map[string]bool{}); err == nil {
		t.Fatal("stale affinity bypassed scope revocation")
	}
	w := httptest.NewRecorder()
	r := portalRequest(owner, "/portal/api/contributions/999/models/refresh", `{}`)
	r.SetPathValue("id", "999")
	s.portalRefreshContributionModels(w, r)
	if w.Code != 404 {
		t.Fatal("unknown ownership accepted", w.Code)
	}
}

func TestPersonalAccountAccessSurvivesWithdrawalWithoutGroup(t *testing.T) {
	s, owner := portalFixture(t)
	acc := s.o.pool.Accounts()[0]
	s.o.pool = pool.New(map[string]pool.Credential{acc.UID: catalogOffline{acc.Mgr}}, "")
	s.o.models = models.NewWithCatalogClient(s.o.pool, s.o.db, &http.Client{Transport: catalogTransport{}})
	s.o.models.Refresh()
	id, err := s.o.db.CreateContribution(owner.ID, acc.UID, "workbuddy", "codebuddy", "active")
	if err != nil {
		t.Fatal(err)
	}
	key := "personal-key"
	appID, err := s.o.db.CreateApp("personal", s.o.hashKey(key), "", "", "", `["test-model"]`, owner.ID)
	if err != nil {
		t.Fatal(err)
	}
	p := portalKeyPrincipal(t, s, key)
	if err := s.prepareModel(p, map[string]any{"model": "test-model"}); err != nil {
		t.Fatal(err.body)
	}
	if !p.AccountScope[acc.UID] || len(p.AccountScope) != 1 || !p.OwnedModels["test-model"] || p.SharedModels["test-model"] {
		t.Fatal(p)
	}
	otherID, err := portalauth.CreateUser(s.o.db, "other-user", "otherpassword", "user")
	if err != nil {
		t.Fatal(err)
	}
	other, _ := s.o.db.GetUser(otherID)
	if _, err := s.o.db.CreateContribution(other.ID, "other-account", "workbuddy", "codebuddy", "active"); err != nil {
		t.Fatal(err)
	}
	group := portalGrant(t, s, other, "shared", acc.UID, []string{"test-model"})
	otherKey := "other-key"
	if _, err := s.o.db.CreateApp("other", s.o.hashKey(otherKey), "", "", "", `["test-model"]`, other.ID); err != nil {
		t.Fatal(err)
	}
	otherPrincipal := portalKeyPrincipal(t, s, otherKey)
	payload := map[string]any{"model": "test-model"}
	if err := s.prepareModel(otherPrincipal, payload); err != nil {
		t.Fatal(err.body)
	}
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+otherKey)
	req, ok := s.refreshPortalRequest(httptest.NewRecorder(), req, otherPrincipal, payload)
	if !ok {
		t.Fatal("shared request should initially be authorized")
	}
	w := httptest.NewRecorder()
	withdraw := portalRequest(owner, "/portal/api/contributions/revoke", `{}`)
	withdraw.SetPathValue("id", itoa(int(id)))
	s.portalRevokeContribution(w, withdraw)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if ids, err := s.o.db.GroupAccountUIDs(group); err != nil || len(ids) != 0 {
		t.Fatal(ids, err)
	}
	check := req.Context().Value(portalDispatchCheckKey{}).(func(string, string) *apiError)
	if err := check(acc.UID, "test-model"); err == nil {
		t.Fatal("queued shared request survived withdrawal")
	}
	p = portalKeyPrincipal(t, s, key)
	if err := s.prepareModel(p, map[string]any{"model": "test-model"}); err != nil || !p.AccountScope[acc.UID] {
		t.Fatal("owner lost personal access", err)
	}
	if _, err := s.o.pickAccountExcludingIn("test-model", "", nil, nil); err == nil {
		t.Fatal("private-only account entered unrestricted pool")
	}
	if _, err := s.o.pickAccountExcludingIn("test-model", "", nil, p.AccountScope); err != nil {
		t.Fatal("owner could not select own account", err)
	}
	restore := portalRequest(owner, "/portal/api/contributions/share", `{"accepted":true}`)
	restore.SetPathValue("id", itoa(int(id)))
	otherRestore := portalRequest(other, "/portal/api/contributions/share", `{"accepted":true}`)
	otherRestore.SetPathValue("id", itoa(int(id)))
	w = httptest.NewRecorder()
	s.portalShareContribution(w, otherRestore)
	if w.Code != 404 {
		t.Fatal("nonowner restored sharing", w.Code)
	}
	w = httptest.NewRecorder()
	s.portalShareContribution(w, restore)
	if w.Code != 200 {
		t.Fatal("owner could not restore sharing", w.Code, w.Body.String())
	}
	// Restore only joins the default pool; custom memberships stay removed.
	if ids, err := s.o.db.GroupAccountUIDs(group); err != nil || len(ids) != 0 {
		t.Fatal("restore silently rejoined custom pool", ids, err)
	}
	w = httptest.NewRecorder()
	s.portalModels(w, portalRequest(owner, "/portal/api/models", `{}`))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"own_account":true`) || strings.Contains(w.Body.String(), acc.UID) || strings.Contains(w.Body.String(), "account_uids") {
		t.Fatal("model view did not respect privacy or ownership", w.Code, w.Body.String())
	}
	if err := s.o.db.SetAppModels(appID, `[]`); err != nil {
		t.Fatal(err)
	}
	if err := s.prepareModel(portalKeyPrincipal(t, s, key), map[string]any{"model": "test-model"}); err == nil {
		t.Fatal("personal rights bypassed key whitelist")
	}
	if err := s.o.db.SetAppModels(appID, `["test-model"]`); err != nil {
		t.Fatal(err)
	}
	if err := s.o.db.SaveSettings(map[string]string{"portal_disabled_models": `["test-model"]`}); err != nil {
		t.Fatal(err)
	}
	if err := s.prepareModel(portalKeyPrincipal(t, s, key), map[string]any{"model": "test-model"}); err == nil {
		t.Fatal("personal rights bypassed global model disable")
	}
}

func TestPlatformAccountSharingRechecksPrivateDispatch(t *testing.T) {
	s, u := portalFixture(t)
	acc := s.o.pool.Accounts()[0]
	s.o.pool = pool.New(map[string]pool.Credential{acc.UID: catalogOffline{acc.Mgr}}, "")
	s.o.models = models.NewWithCatalogClient(s.o.pool, s.o.db, &http.Client{Transport: catalogTransport{}})
	s.o.models.Refresh()
	if _, err := s.o.db.UpsertAccount(map[string]any{"auth": map[string]any{"access_token": "test-token"}, "account": map[string]any{"uid": acc.UID, "nickname": "platform"}}); err != nil {
		t.Fatal(err)
	}
	g := portalGrant(t, s, u, "platform-pool", acc.UID, []string{"test-model"})
	if _, err := s.o.db.CreateContribution(u.ID, "user-own-account", "workbuddy", "workbuddy", "active"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.o.db.CreateApp("private", s.o.hashKey("private-platform-key"), "", "", "", "", 0); err != nil {
		t.Fatal(err)
	}
	p := portalKeyPrincipal(t, s, "private-platform-key")
	body := map[string]any{"model": "test-model"}
	req := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer private-platform-key")
	req, ok := s.refreshPortalRequest(httptest.NewRecorder(), req, p, body)
	if !ok {
		t.Fatal("private fixture not authorized")
	}
	if err := s.o.db.SetPlatformAccountSharing(acc.UID, "shared", []int64{g}); err != nil {
		t.Fatal(err)
	}
	check := req.Context().Value(portalDispatchCheckKey{}).(func(string, string) *apiError)
	if err := check(acc.UID, "test-model"); err == nil {
		t.Fatal("private dispatch survived shared-only conversion")
	}
	userPrincipal := &Principal{UserID: u.ID, AllowedModels: []string{"test-model"}}
	if err := s.o.attachPortalScope(userPrincipal); err != nil {
		t.Fatal(err.body)
	}
	if err := s.prepareModel(userPrincipal, map[string]any{"model": "test-model"}); err != nil || !userPrincipal.AccountScope[acc.UID] {
		t.Fatal("shared user could not access supplied platform account", err)
	}
	if err := s.o.db.SetPlatformAccountSharing(acc.UID, "private", nil); err != nil {
		t.Fatal(err)
	}
	userPrincipal = &Principal{UserID: u.ID, AllowedModels: []string{"test-model"}}
	if err := s.o.attachPortalScope(userPrincipal); err != nil {
		t.Fatal(err.body)
	}
	if err := s.prepareModel(userPrincipal, map[string]any{"model": "test-model"}); err == nil {
		t.Fatal("private conversion left shared access")
	}
	if _, err := s.o.pickAccountExcludingIn("test-model", "", nil, nil); err != nil {
		t.Fatal("private conversion did not restore platform access", err)
	}
}

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
