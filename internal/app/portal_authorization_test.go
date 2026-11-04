package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"work2api/internal/workbuddy/credentials"
	wbruntime "work2api/internal/workbuddy/runtime"

	"work2api/internal/core/provider"
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
	ownerPrincipal := &Principal{UserID: ownerID}
	if err := s.o.attachPortalScope(ownerPrincipal); err != nil || len(ownerPrincipal.PortalModels) != 0 {
		t.Fatal("owner received unavailable models", err)
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
	acc := s.o.wb.Pool.Accounts()[0]
	s.o.wb.Pool = pool.New(map[string]pool.Credential{acc.UID: catalogOffline{acc.Mgr}}, "")
	s.o.wb.Catalog = models.NewWithCatalogClient(s.o.wb.Pool, s.o.db, &http.Client{Transport: catalogTransport{}})
	if _, err := s.o.db.CreateContribution(owner.ID, acc.UID, "workbuddy", "codebuddy", "active"); err != nil {
		t.Fatal(err)
	}
	if len(s.portalAvailableModels(owner.ID)) != 0 {
		t.Fatal("unverified account got models")
	}
	if !s.o.wb.Catalog.RefreshAccount(context.Background(), acc.UID) {
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
	if s.o.sessions.Lookup(key) != acc.UID {
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
	acc := s.o.wb.Pool.Accounts()[0]
	s.o.wb.Pool = pool.New(map[string]pool.Credential{acc.UID: catalogOffline{acc.Mgr}}, "")
	s.o.wb.Catalog = models.NewWithCatalogClient(s.o.wb.Pool, s.o.db, &http.Client{Transport: catalogTransport{}})
	s.o.wb.Catalog.Refresh()
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
		t.Fatal(err.Body)
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
		t.Fatal(err.Body)
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
	acc := s.o.wb.Pool.Accounts()[0]
	s.o.wb.Pool = pool.New(map[string]pool.Credential{acc.UID: catalogOffline{acc.Mgr}}, "")
	s.o.wb.Catalog = models.NewWithCatalogClient(s.o.wb.Pool, s.o.db, &http.Client{Transport: catalogTransport{}})
	s.o.wb.Catalog.Refresh()
	if _, err := wbruntime.PersistAccount(s.o.db, map[string]any{"auth": map[string]any{"access_token": "test-token"}, "account": map[string]any{"uid": acc.UID, "nickname": "platform"}}); err != nil {
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
	if err := check(acc.UID, "test-model"); err != nil {
		t.Fatal("legacy sharing mode changed unified pool", err)
	}
	userPrincipal := &Principal{UserID: u.ID, AllowedModels: []string{"test-model"}}
	if err := s.o.attachPortalScope(userPrincipal); err != nil {
		t.Fatal(err.Body)
	}
	if err := s.prepareModel(userPrincipal, map[string]any{"model": "test-model"}); err != nil || !userPrincipal.AccountScope[acc.UID] {
		t.Fatal("shared user could not access supplied platform account", err)
	}
	if err := s.o.db.SetPlatformAccountSharing(acc.UID, "private", nil); err != nil {
		t.Fatal(err)
	}
	userPrincipal = &Principal{UserID: u.ID, AllowedModels: []string{"test-model"}}
	if err := s.o.attachPortalScope(userPrincipal); err != nil {
		t.Fatal(err.Body)
	}
	if err := s.prepareModel(userPrincipal, map[string]any{"model": "test-model"}); err != nil {
		t.Fatal("legacy private mode changed explicit user grant", err)
	}
	if _, err := s.o.pickAccountExcludingIn("test-model", "", nil, nil); err != nil {
		t.Fatal("private conversion did not restore platform access", err)
	}
}

func portalAuthorizeFixture(t *testing.T) (*Server, *store.User, int64, string) {
	t.Helper()
	s, u := portalFixture(t)
	acc := s.o.wb.Pool.Get("test-account")
	s.o.wb.Pool = pool.New(map[string]pool.Credential{"test-account": portalCatalogCredential{acc.Mgr, "model-a,model-b"}, "second-account": portalCatalogCredential{acc.Mgr, "model-b"}}, "")
	s.o.wb.Catalog = models.NewWithCatalogClient(s.o.wb.Pool, s.o.db, &http.Client{Transport: portalCatalogTransport{}})
	s.o.wb.Catalog.Refresh()
	other, err := portalauth.CreateUser(s.o.db, "account-provider", "testpassword", "user")
	if err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{"test-account", "second-account"} {
		if _, err := s.o.db.CreateContribution(other, uid, "workbuddy", "codebuddy", "active"); err != nil {
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
	grants, err := s.o.db.UserModelGrants(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	list := append([]string{}, models...)
	for _, g := range grants {
		if len(g.AccountUIDs) > 0 && g.AccountUIDs[0] == "*" {
			list = append(list, g.Model)
		}
	}
	if err := s.o.db.SetUserPublicModels(u.ID, list, nil); err != nil {
		t.Fatal(err)
	}
	return id
}
func portalKeyPrincipal(t *testing.T, s *Server, key string) *Principal {
	t.Helper()
	p, e := s.o.checkAPIKey("Bearer "+key, "")
	if e != nil {
		t.Fatal(e.Body)
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
			t.Fatal(err.Body)
		}
		if !p.AccountScope[uid] || model == "model-a" && len(p.AccountScope) != 1 {
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
		t.Fatal("unique canonical group/key intersection rejected", err.Body)
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
		t.Fatal(err.Body)
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
	_ = group
	if err := s.o.db.SetUserPublicModels(u.ID, nil, nil); err != nil {
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
		t.Fatal(err.Body)
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
	s.o.wb.Limiters = map[string]*ratelimit.Limiter{"test-account": limit}
	done := make(chan error, 1)
	go func() {
		_, err := s.o.runOnce(r.Context(), s.o.wb.Pool.Get("test-account"), payload, func(string) error { return nil })
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
	if s.o.wb.UpstreamClient.(*failingUpstream).calls != 0 {
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
		t.Fatal(err.Body)
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
	_, err := s.o.runOnce(r.Context(), s.o.wb.Pool.Get("test-account"), map[string]any{"model": "model-a"}, func(string) error { return nil })
	if _, ok := err.(*apiError); !ok {
		t.Fatal("stale alias target dispatched", err)
	}
	if s.o.wb.UpstreamClient.(*failingUpstream).calls != 0 {
		t.Fatal("stale model reached upstream")
	}
}

type portalCatalogCredential struct {
	pool.Credential
	models string
}

func (c portalCatalogCredential) CatalogHeaders() (map[string]string, error) {
	return map[string]string{"x-test-models": c.models}, nil
}

type portalCatalogTransport struct{}

func (portalCatalogTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	ids := strings.Split(r.Header.Get("x-test-models"), ",")
	items := []map[string]any{}
	for _, id := range ids {
		items = append(items, map[string]any{"id": id, "name": id})
	}
	body, _ := json.Marshal(map[string]any{"data": map[string]any{"models": items, "agents": []map[string]any{{"name": "cli", "models": ids}}}})
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(body))), Request: r}, nil
}

type mediaAppTransport func(*http.Request) (*http.Response, error)

func (f mediaAppTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMediaHTTPTaskOwnershipAndSingleSubmit(t *testing.T) {
	s, user := portalFixture(t)
	path := filepath.Join(t.TempDir(), "account.info")
	raw := fmt.Sprintf(`{"auth":{"accessToken":"fixture","domain":"www.codebuddy.cn","expiresAt":%d},"account":{"uid":"media-account"}}`, time.Now().Add(time.Hour).UnixMilli())
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	mgr := credentials.NewManager(path)
	s.o.wb.SetManager("media-account", mgr)
	s.o.wb.Pool = pool.New(map[string]pool.Credential{"media-account": mgr}, "")
	submissions, queries := 0, 0
	queryEntered, queryRelease := make(chan struct{}), make(chan struct{})
	transport := mediaAppTransport(func(r *http.Request) (*http.Response, error) {
		body := `{"code":0,"data":{"agents":[{"name":"cli","models":[]}],"models":[{"id":"movie","tags":["text-to-video"],"videoGeneration":{"defaultSeconds":3,"minSeconds":3,"maxSeconds":15}},{"id":"draw","tags":["text-to-image"]},{"id":"edit","tags":["image-to-image"]}]}}`
		switch r.URL.Path {
		case "/v2/videos/generations":
			submissions++
			body = `{"code":0,"data":{"id":"upstream-task"}}`
		case "/v2/images/generations", "/v2/images/edits":
			var p map[string]any
			json.NewDecoder(r.Body).Decode(&p)
			if r.URL.Path == "/v2/images/edits" {
				images, ok := p["image"].([]any)
				if !ok || len(images) != 1 {
					t.Fatal("edit images lost", p)
				}
			}
			body = `{"code":0,"data":{"created":1,"data":[{"url":"https://example.invalid/image.png"}],"usage":{"credit":5.71}}}`
		case "/v2/async/tasks/create":
			body = `{"code":0,"data":{"task_id":"audio-upstream"}}`
		case "/v2/async/tasks/query":
			body = `{"code":0,"data":{"status":"completed","audio_transcription":{"text":"hello"},"usage":{"credit":0.01}}}`
		case "/v2/videos/tasks":
			queries++
			if queries == 1 {
				close(queryEntered)
				<-queryRelease
			}
			var p map[string]any
			json.NewDecoder(r.Body).Decode(&p)
			if p["task_id"] != "upstream-task" {
				t.Fatal(p)
			}
			body = `{"code":0,"data":{"status":"completed","data":[{"url":"https://example.invalid/video.mp4"}],"usage":{"credit":9.5,"output_tokens":180000}}}`
		}
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	s.o.wb.Catalog = models.NewWithCatalogClient(s.o.wb.Pool, s.o.db, &http.Client{Transport: transport})
	s.o.wb.Catalog.Refresh()
	s.o.wb.MediaClient = &http.Client{Transport: transport}
	defaults, _ := s.o.defaultModelAccess(map[string]string{})
	if defaults["movie"] {
		t.Fatal("unpriced movie auto-authorized")
	}
	for _, key := range []string{"media-owner", "media-other"} {
		if _, err := s.o.db.CreateApp(key, s.o.hashKey(key), "", "", "", `["movie","draw","edit","wb-asr"]`, 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.o.db.CreateApp("portal-media", s.o.hashKey("portal-media"), "", "", "", `["movie","wb-asr"]`, user.ID); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	request := func(method, path, body, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	w := request("POST", "/v1/videos", `{"model":"movie","prompt":"test"}`, "portal-media")
	if w.Code != 403 {
		t.Fatal("unowned model accepted", w.Code, w.Body.String())
	}
	w = request("POST", "/v1/chat/completions", `{"model":"movie","messages":[]}`, "media-owner")
	if w.Code != 400 || submissions != 0 {
		t.Fatal("media sent through chat", w.Code, w.Body.String())
	}
	w = request("POST", "/v1/videos", `{"model":"movie","prompt":"test"}`, "media-owner")
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var accepted map[string]any
	json.Unmarshal(w.Body.Bytes(), &accepted)
	id := accepted["id"].(string)
	if w = request("GET", "/v1/videos/"+id, "", "media-other"); w.Code != 404 {
		t.Fatal("task leaked across keys", w.Code)
	}
	if w = request("GET", "/v1/videos/"+id+"?cached=1", "", "media-owner"); w.Code != 200 || queries != 0 {
		t.Fatal("UI polling duplicated upstream queries", w.Code, queries)
	}
	if err := s.o.db.DeferMediaTaskCheck(id, "", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	trackingDone := make(chan struct{})
	go func() {
		defer close(trackingDone)
		(&Scheduler{o: s.o}).reconcileMediaTasks(context.Background())
	}()
	select {
	case <-queryEntered:
	case <-time.After(3 * time.Second):
		t.Fatal("background query did not start")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.o.queryMediaTask(canceled, id); err != context.Canceled || queries != 1 {
		t.Fatal("canceled waiter duplicated the task query", err, queries)
	}
	close(queryRelease)
	<-trackingDone
	if submissions != 1 || queries != 1 {
		t.Fatal("background tracking failed or resubmitted generation", submissions, queries)
	}
	for i := 0; i < 2; i++ {
		w = request("GET", "/v1/videos/"+id, "", "media-owner")
		if w.Code != 200 || !strings.Contains(w.Body.String(), "completed") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if submissions != 1 || queries != 1 {
		t.Fatal("task resubmitted or terminal query repeated", submissions, queries)
	}
	task, err := s.o.db.MediaTask(id)
	if err != nil {
		t.Fatal(err)
	}
	row, err := s.o.db.GetUsage(int(task.UsageID))
	if err != nil || row["credits"] != 9.5 || row["status"] != "ok" {
		t.Fatal(row, err)
	}
	w = request("POST", "/v1/images/generations", `{"model":"draw","prompt":"test"}`, "media-owner")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "5.71") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request("POST", "/v1/images/edits", `{"model":"edit","prompt":"test","image":["data:image/png;base64,aGVsbG8="]}`, "media-owner")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if _, err := s.o.db.CreateContribution(user.ID, "media-account", "workbuddy", "codebuddy", "private"); err != nil {
		t.Fatal(err)
	}
	p, aerr := s.o.checkAPIKey("Bearer portal-media", "")
	if aerr != nil || !p.PortalModels["movie"] {
		t.Fatal("owner media permission missing", aerr)
	}
	w = request("POST", "/v1/audio/transcriptions", `{"model":"wb-asr","audio_url":"https://example.invalid/voice.wav","async":true}`, "portal-media")
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	var audioAccepted map[string]any
	json.Unmarshal(w.Body.Bytes(), &audioAccepted)
	w = request("GET", "/v1/media/tasks/"+audioAccepted["id"].(string), "", "portal-media")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "hello") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = request("GET", "/v1/media/tasks", "", "portal-media")
	if w.Code != 200 || !strings.Contains(w.Body.String(), audioAccepted["id"].(string)) {
		t.Fatal("task list omitted owned audio", w.Code, w.Body.String())
	}

	if err := s.o.db.SetAppModels(task.AppID, `["draw"]`); err != nil {
		t.Fatal(err)
	}
	w = request("GET", "/v1/videos/"+id, "", "media-owner")
	if w.Code != 403 {
		t.Fatal("cached task bypassed revoked model permission", w.Code)
	}

	if err := s.o.db.SaveSettings(map[string]string{"model_aliases": "cinema=movie"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.o.db.CreateApp("media-alias", s.o.hashKey("media-alias"), "", "", "", `["cinema"]`, user.ID); err != nil {
		t.Fatal(err)
	}
	w = request("POST", "/v1/videos", `{"model":"cinema","prompt":"test"}`, "media-alias")
	if w.Code != 202 {
		t.Fatal("alias-only key submit failed", w.Code, w.Body.String())
	}
	json.Unmarshal(w.Body.Bytes(), &accepted)
	aliasID := accepted["id"].(string)
	aliasTask, err := s.o.db.MediaTask(aliasID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.o.db.SetAppModels(aliasTask.AppID, `["draw"]`); err != nil {
		t.Fatal(err)
	}
	if err := s.o.db.DeferMediaTaskCheck(aliasID, "", time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	beforeQueries := queries
	(&Scheduler{o: s.o}).reconcileMediaTasks(context.Background())
	deferred, err := s.o.db.MediaTask(aliasID)
	if err != nil || queries != beforeQueries || deferred.Status != "queued" || deferred.CheckFailures == 0 || deferred.NextCheckAt <= nowSec() {
		t.Fatal("revoked task dispatched or lost recoverable state", deferred, err, queries)
	}
	if err := s.o.db.SetAppModels(aliasTask.AppID, `["cinema"]`); err != nil {
		t.Fatal(err)
	}
	w = request("GET", "/v1/videos/"+aliasID, "", "media-alias")
	if w.Code != 200 {
		t.Fatal("alias-only key query failed", w.Code, w.Body.String())
	}

}

type decisionFixtureRuntime struct{}

func (*decisionFixtureRuntime) Name() string   { return "decisionfixture" }
func (*decisionFixtureRuntime) Prefix() string { return "decisionfixture/" }
func (*decisionFixtureRuntime) Ready() bool    { return true }
func (*decisionFixtureRuntime) Models(context.Context) []provider.CatalogModel {
	return []provider.CatalogModel{{ID: "decisionfixture/d", Extra: map[string]any{"native_protocol": "systemone"}}}
}
func (*decisionFixtureRuntime) Serve(ctx context.Context, req provider.ServeRequest) (provider.UsageReport, error) {
	if req.Protocol != provider.ProtocolSystemOne {
		return provider.UsageReport{}, fmt.Errorf("wrong protocol")
	}
	writeJSON(req.Writer, 200, map[string]any{"state": req.Payload["state"], "questions": req.Payload["questions"]})
	return provider.UsageReport{Status: "ok"}, nil
}
func TestSystemOnePublicRoutePreservesDecisionPayload(t *testing.T) {
	s, _ := portalFixture(t)
	s.o.cfg.ModelLimitsDisabled = true
	if err := s.o.runtimes.Register(&decisionFixtureRuntime{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.o.db.CreateApp("decision", s.o.hashKey("decision"), "", "", "", `["decisionfixture/d"]`, 0); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	r := httptest.NewRequest("POST", "http://127.0.0.1/v1/systemone", strings.NewReader(`{"model":"decisionfixture/d","state":"keep","questions":[{"type":"boolean","question":"test"}]}`))
	r.Header.Set("Authorization", "Bearer decision")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"keep"`) || !strings.Contains(w.Body.String(), `"questions"`) {
		t.Fatal(w.Code, w.Body.String())
	}
}
