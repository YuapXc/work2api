package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"work2api/internal/core/provider"
	appcrypto "work2api/internal/crypto"
	"work2api/internal/portal/portalauth"
	"work2api/internal/store"
	"work2api/internal/streamwatch"
	"work2api/internal/workbuddy/models"
	"work2api/internal/workbuddy/pool"
)

func TestResourceRouteRequiresAdminAuthentication(t *testing.T) {
	s, _ := portalFixture(t)
	s.o.cfg.AdminToken = "resource-test-token"
	s.o.cfg.DBPath = filepath.Join(t.TempDir(), "resource.db")
	if err := os.WriteFile(s.o.cfg.DBPath, []byte("file metadata"), 0600); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	request := func(token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "http://127.0.0.1/admin/resources", nil)
		r.RemoteAddr = "127.0.0.1:23456"
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request(""); w.Code == 200 {
		t.Fatal("unauthenticated resource access")
	}
	w := request("resource-test-token")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var fields map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
		t.Fatal(err)
	}
	if fields["database_bytes"] == nil || fields["usage"] == nil || strings.Contains(w.Body.String(), s.o.cfg.DBPath) {
		t.Fatal("missing metrics or leaked path", w.Body.String())
	}
}

func portalFixture(t *testing.T) (*Server, *store.User) {
	t.Helper()
	o, _ := newDNSFailoverOrch(t, &failingUpstream{})
	o.cfg.PortalEnabled = true
	o.cfg.PortalMaxTasksPerUser = 1
	o.wb.ProjectAuths = t.TempDir()
	id, err := portalauth.CreateUser(o.db, "portal-user", "oldpassword", "user")
	if err != nil {
		t.Fatal(err)
	}
	u, err := o.db.GetUser(id)
	if err != nil {
		t.Fatal(err)
	}
	return NewServer(o), u
}

func TestContributionCollisionCannotOverwriteOnRetry(t *testing.T) {
	s, user := portalFixture(t)
	uid := "collision/uid"
	path := filepath.Join(s.o.wb.ProjectAuths, "workbuddy-"+safeUID(uid)+".info")
	original := []byte(`{"auth":{"accessToken":"original"},"account":{"uid":"collision:uid"}}`)
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	res := map[string]any{"auth": map[string]any{"accessToken": "new-token"}, "account": map[string]any{"uid": uid}}
	if err := s.completePortalContribution(user.ID, uid, "codebuddy", res); err == nil {
		t.Fatal("initial collision accepted")
	}
	if c, err := s.o.db.ContributionByAccount(uid); err != nil || c != nil {
		t.Fatal("failed collision reserved identity", c, err)
	}
	// Simulate the verifying row created by the previous implementation.
	if _, err := s.o.db.CreateContribution(user.ID, uid, "workbuddy", "codebuddy", "verifying"); err != nil {
		t.Fatal(err)
	}
	if err := s.completePortalContribution(user.ID, uid, "codebuddy", res); err == nil {
		t.Fatal("retry bypassed collision check")
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatal("other account credential overwritten", err)
	}
}
func portalRequest(user *store.User, path, body string) *http.Request {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	// 写请求要求同源浏览器信号（与 adminGuard 一致），测试统一补 Origin。
	r.Header.Set("Origin", "http://"+r.Host)
	return r.WithContext(context.WithValue(r.Context(), portalUserKey{}, user))
}

func TestPortalPasswordRouteAuthenticatesAndRevokesSessions(t *testing.T) {
	s, u := portalFixture(t)
	_, token, err := portalauth.Login(s.o.db, u.Username, "oldpassword")
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "/portal/api/auth/password", strings.NewReader(`{"old_password":"oldpassword","new_password":"newpassword"}`))
	r.Header.Set("Origin", "http://"+r.Host)
	r.AddCookie(&http.Cookie{Name: portalauth.UserCookieName, Value: token})
	w := httptest.NewRecorder()
	s.portalGuard(http.HandlerFunc(s.portalChangePassword)).ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Body.String(), w.Header())
	}
	if _, err := portalauth.SessionUser(s.o.db, token); err == nil {
		t.Fatal("old session survived password rotation")
	}
	if _, _, err := portalauth.Login(s.o.db, u.Username, "newpassword"); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	second := httptest.NewRequest("POST", "/portal/api/auth/password", strings.NewReader(`{}`))
	second.Header.Set("Origin", "http://"+second.Host)
	s.portalGuard(http.HandlerFunc(s.portalChangePassword)).ServeHTTP(w, second)
	if w.Code != 401 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Header())
	}
}

func TestPortalBeginReservesCapacityBeforeUpstream(t *testing.T) {
	s, u := portalFixture(t)
	previous := portalOAuthBegin
	defer func() { portalOAuthBegin = previous }()
	entered := make(chan struct{})
	release := make(chan struct{})
	portalOAuthBegin = func(string) (map[string]any, error) {
		close(entered)
		<-release
		return map[string]any{"state": "state", "authUrl": "https://example.invalid", "site": "cn"}, nil
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		s.portalContributionBegin(w, portalRequest(u, "/portal/api/contributions/begin", `{"accepted":true}`))
		result <- w
	}()
	<-entered
	w := httptest.NewRecorder()
	s.portalContributionBegin(w, portalRequest(u, "/portal/api/contributions/begin", `{"accepted":true}`))
	if w.Code != 429 {
		t.Fatal(w.Code, w.Body.String())
	}
	close(release)
	if w := <-result; w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(s.portalTasks) != 1 {
		t.Fatal("reservation leaked", len(s.portalTasks))
	}
}

func TestPortalContributionLoginSites(t *testing.T) {
	s, u := portalFixture(t)
	previous := portalOAuthBegin
	t.Cleanup(func() { portalOAuthBegin = previous })
	called := ""
	portalOAuthBegin = func(site string) (map[string]any, error) {
		called = site
		return map[string]any{"state": "test-state", "authUrl": "https://example.invalid/login", "site": site}, nil
	}
	for _, tc := range []struct{ input, want string }{{"", "cn"}, {"cn", "cn"}, {"intl", "intl"}, {"intl-codebuddy", "intl-codebuddy"}, {" CN ", "cn"}, {"workbuddy", ""}, {"unknown", ""}} {
		called = ""
		s.portalTasks = map[string]*portalContributionTask{}
		body, _ := json.Marshal(map[string]any{"site": tc.input, "accepted": true})
		w := httptest.NewRecorder()
		s.portalContributionBegin(w, portalRequest(u, "/portal/api/contributions/begin", string(body)))
		if tc.want == "" {
			if w.Code != 400 || called != "" || len(s.portalTasks) != 0 {
				t.Fatalf("invalid site contacted upstream or reserved a task: %d %s", w.Code, w.Body.String())
			}
		} else if w.Code != 200 || called != tc.want || s.portalTasks["test-state"].site != tc.want {
			t.Fatalf("site %q: %d %s, upstream=%q", tc.input, w.Code, w.Body.String(), called)
		}
	}
}

// Exercise the real middleware and routes with an HTTPS browser and an HTTP
// loopback proxy, as used by Nginx and Cloudflare Tunnel. Forwarded headers
// are not needed to establish the external scheme or session cookie policy.
func TestPortalRoutesBehindHTTPSProxy(t *testing.T) {
	s, _ := portalFixture(t)
	s.o.crypto = appcrypto.NewManager(t.TempDir())
	s.o.cfg.AdminToken = "test-admin-token"
	s.o.cfg.AllowExternalHost = true
	s.o.cfg.AdminCookieSecure = true
	s.o.cfg.AdminCookieSecureAuto = true
	s.o.cfg.PortalRegistrationMode = "open"
	s.o.cfg.PortalKeyMaxPerUser = 5
	h := s.Handler()
	var cookie *http.Cookie
	request := func(method, path, body, origin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://gateway.example.com"+path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:12345"
		r.Header.Set("Origin", origin)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	const origin = "https://gateway.example.com"
	w := request("POST", "/portal/api/auth/register", `{"username":"Proxy-User","password":"testpassword"}`, origin)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly {
		t.Fatal("missing secure session")
	}
	cookie = cookies[0]
	user, err := s.o.db.GetUserByUsername("proxy-user")
	if err != nil || user == nil {
		t.Fatal("registered user missing", err)
	}
	acc := s.o.wb.Pool.Accounts()[0]
	s.o.wb.Pool = pool.New(map[string]pool.Credential{acc.UID: catalogOffline{acc.Mgr}}, "")
	s.o.wb.Catalog = models.NewWithCatalogClient(s.o.wb.Pool, s.o.db, &http.Client{Transport: catalogTransport{}})
	contributionID, err := s.o.db.CreateContribution(user.ID, acc.UID, "workbuddy", "codebuddy", "active")
	if err != nil || !s.o.wb.Catalog.RefreshAccount(context.Background(), acc.UID) {
		t.Fatal("contribution catalog not verified", err)
	}
	for _, path := range []string{"/portal/api/me", "/portal/api/models", "/portal/api/keys", "/portal/api/contributions", "/portal/api/usage"} {
		w = request("GET", path, "", origin)
		if w.Code != 200 {
			t.Fatal(path, w.Code, w.Body.String())
		}
		if (path == "/portal/api/me" || path == "/portal/api/models") && !strings.Contains(w.Body.String(), "test-model") {
			t.Fatal("own model missing", path, w.Body.String())
		}
	}
	previousBegin, previousPoll := portalOAuthBegin, portalOAuthPoll
	t.Cleanup(func() { portalOAuthBegin, portalOAuthPoll = previousBegin, previousPoll })
	portalOAuthBegin = func(site string) (map[string]any, error) {
		if site != "cn" {
			t.Fatal("incorrect login site", site)
		}
		return map[string]any{"state": "proxy-login-task", "authUrl": "https://example.invalid/login", "site": site}, nil
	}
	portalOAuthPoll = func(state, site string) (map[string]any, error) { return map[string]any{"status": "pending"}, nil }
	for _, op := range []struct{ path, body string }{
		{"begin", `{"site":"cn","accepted":true}`},
		{"poll", `{"task_id":"proxy-login-task"}`},
		{"cancel", `{"task_id":"proxy-login-task"}`},
	} {
		w = request("POST", "/portal/api/contributions/"+op.path, op.body, origin)
		if w.Code != 200 {
			t.Fatal(op.path, w.Code, w.Body.String())
		}
	}
	if len(s.portalTasks) != 0 {
		t.Fatal("cancel did not release login task")
	}
	w = request("POST", "/portal/api/keys", `{"name":"proxy-test"}`, "https://attacker.example.com")
	if w.Code != 403 {
		t.Fatal("cross-origin write accepted", w.Code)
	}
	w = request("POST", "/portal/api/keys", `{"name":"proxy-test"}`, origin)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var created struct {
		ID  int64  `json:"id"`
		Key string `json:"key"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || created.ID == 0 || created.Key == "" {
		t.Fatal("invalid key response", err)
	}
	keyPath := fmt.Sprintf("/portal/api/keys/%d", created.ID)
	accountPath := fmt.Sprintf("/portal/api/contributions/%d", contributionID)
	for _, op := range []struct{ method, path, body string }{
		{"POST", keyPath + "/toggle", `{}`},
		{"POST", keyPath + "/models", `{"allowed_models":[]}`},
		{"DELETE", keyPath, ""},
		{"POST", accountPath + "/revoke", `{}`},
		{"POST", accountPath + "/share", `{"accepted":true}`},
		{"POST", "/portal/api/auth/logout", ""},
	} {
		w = request(op.method, op.path, op.body, origin)
		if w.Code != 200 {
			t.Fatal(op.path, w.Code, w.Body.String())
		}
	}
	if w = request("GET", "/portal/api/me", "", origin); w.Code != 401 {
		t.Fatal("logout session survived", w.Code)
	}
	cookie = nil
	w = request("POST", "/portal/api/auth/login", `{"username":"PROXY-USER","password":"testpassword"}`, origin)
	if w.Code != 200 || len(w.Result().Cookies()) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestPortalPollClaimAndCancelPreventCompletion(t *testing.T) {
	s, u := portalFixture(t)
	s.portalTasks["state"] = &portalContributionTask{userID: u.ID, site: "cn", expires: time.Now().Add(time.Minute)}
	previous := portalOAuthPoll
	defer func() { portalOAuthPoll = previous }()
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	portalOAuthPoll = func(string, string) (map[string]any, error) {
		calls.Add(1)
		close(entered)
		<-release
		return map[string]any{"status": "ready", "account": map[string]any{"uid": "new-account"}}, nil
	}
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		s.portalContributionPoll(w, portalRequest(u, "/portal/api/contributions/poll", `{"task_id":"state"}`))
		result <- w
	}()
	<-entered
	w := httptest.NewRecorder()
	s.portalContributionPoll(w, portalRequest(u, "/portal/api/contributions/poll", `{"task_id":"state"}`))
	if calls.Load() != 1 || !strings.Contains(w.Body.String(), "pending") {
		t.Fatal(calls.Load(), w.Body.String())
	}
	stranger := *u
	stranger.ID++
	w = httptest.NewRecorder()
	s.portalContributionCancel(w, portalRequest(&stranger, "/portal/api/contributions/cancel", `{"task_id":"state"}`))
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	s.portalContributionCancel(w, portalRequest(u, "/portal/api/contributions/cancel", `{"task_id":"state"}`))
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	close(release)
	if w := <-result; !strings.Contains(w.Body.String(), "cancelled") {
		t.Fatal(w.Body.String())
	}
	c, err := s.o.db.ContributionByAccount("new-account")
	if err != nil || c != nil {
		t.Fatal("cancelled poll committed", c, err)
	}
}

func TestPortalPrivateCredentialsRemainUnchangedAndOwnRevokedRestores(t *testing.T) {
	s, u := portalFixture(t)
	old := s.o.manager("test-account")
	before, err := os.ReadFile(old.Path())
	if err != nil {
		t.Fatal(err)
	}
	res := map[string]any{"auth": map[string]any{"accessToken": "new-token"}, "account": map[string]any{"uid": "test-account"}}
	if err := s.completePortalContribution(u.ID, "test-account", "codebuddy", res); err == nil {
		t.Fatal("private account accepted")
	}
	after, err := os.ReadFile(old.Path())
	if err != nil || !bytes.Equal(before, after) || s.o.manager("test-account") != old {
		t.Fatal("private credential mutated", err)
	}
	uid := "contributed-account"
	_, err = s.o.db.CreateResourceGroup("shared", "workbuddy", `["auto"]`)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.o.db.CreateContribution(u.ID, uid, "workbuddy", "codebuddy", "revoked")
	if err != nil {
		t.Fatal(err)
	}
	res["account"] = map[string]any{"uid": uid}
	if err := s.completePortalContribution(u.ID, uid, "codebuddy", res); err != nil {
		t.Fatal(err)
	}
	c, err := s.o.db.ContributionByAccount(uid)
	if err != nil || c.ID != id || c.Status != "active" {
		t.Fatal(c, err)
	}
	if err := s.completePortalContribution(u.ID+1, uid, "codebuddy", res); err == nil {
		t.Fatal("other owner accepted")
	}
}

func TestOptionalContributionFollowAndFixedKeys(t *testing.T) {
	s, u, _, _ := portalAuthorizeFixture(t)
	s.o.crypto = appcrypto.NewManager(t.TempDir())
	s.o.cfg.PortalKeyMaxPerUser = 5
	setPolicy := func(model string) {
		t.Helper()
		raw, _ := json.Marshal(store.DefaultModelPolicy{Mode: "manual", Include: []string{model}, Exclude: []string{}})
		if e := s.o.db.SaveSettings(map[string]string{"portal_model_policy": string(raw)}); e != nil {
			t.Fatal(e)
		}
	}
	setPolicy("model-a")
	contributions, _ := s.o.db.UserContributions(u.ID)
	if len(contributions) != 0 {
		t.Fatal("fixture unexpectedly owns credentials")
	}
	create := func(policy string) string {
		t.Helper()
		w := httptest.NewRecorder()
		s.portalCreateKey(w, portalRequest(u, "/portal/api/keys", `{"name":"optional-`+policy+`","model_policy":"`+policy+`"}`))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var body map[string]any
		json.Unmarshal(w.Body.Bytes(), &body)
		key, _ := body["key"].(string)
		if key == "" {
			t.Fatal("key missing", body)
		}
		return key
	}
	follow, fixed := create("follow"), create("fixed")
	for _, key := range []string{follow, fixed} {
		if e := s.prepareModel(portalKeyPrincipal(t, s, key), map[string]any{"model": "model-a"}); e != nil {
			t.Fatal(e.Body)
		}
	}
	setPolicy("model-b")
	if e := s.prepareModel(portalKeyPrincipal(t, s, follow), map[string]any{"model": "model-b"}); e != nil {
		t.Fatal("follow ignored updated policy", e.Body)
	}
	for _, model := range []string{"model-a", "model-b"} {
		if e := s.prepareModel(portalKeyPrincipal(t, s, fixed), map[string]any{"model": model}); e == nil {
			t.Fatal("fixed key widened or ignored current rights", model)
		}
	}
	if e := s.o.db.SaveSettings(map[string]string{"portal_disabled_models": `["model-b"]`}); e != nil {
		t.Fatal(e)
	}
	if e := s.prepareModel(portalKeyPrincipal(t, s, follow), map[string]any{"model": "model-b"}); e == nil {
		t.Fatal("follow bypassed global disabled model")
	}
}

func TestUnifiedLoginSelectsRoleAndClearsPreviousAdminIdentity(t *testing.T) {
	s, user := portalFixture(t)
	hash, e := portalauth.HashPassword("ownerpassword")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.o.db.CreateInitialAdmin("owner", hash); e != nil {
		t.Fatal(e)
	}
	w := httptest.NewRecorder()
	s.portalLogin(w, httptest.NewRequest("POST", "/portal/api/auth/login", strings.NewReader(`{"username":"owner","password":"ownerpassword"}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"redirect_to":"/admin-ui/"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	var admin *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName && c.MaxAge > 0 {
			admin = c
		}
	}
	if admin == nil || !admin.HttpOnly || admin.SameSite != http.SameSiteLaxMode {
		t.Fatal("admin login cookie missing or unsafe")
	}
	request := httptest.NewRequest("GET", "/admin/identity", nil)
	request.AddCookie(admin)
	if ok, _ := s.adminAuth(request); !ok {
		t.Fatal("role login did not grant authenticated admin session")
	}
	if _, e := s.portalUser(request); e == nil {
		t.Fatal("admin session became ordinary portal session")
	}
	r := httptest.NewRequest("POST", "/portal/api/auth/login", strings.NewReader(`{"username":"`+user.Username+`","password":"oldpassword"}`))
	r.AddCookie(admin)
	w = httptest.NewRecorder()
	s.portalLogin(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"redirect_to":"/portal/"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if ok, _ := s.adminAuth(request); ok {
		t.Fatal("ordinary login retained previous administrative identity")
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == cookieName && c.MaxAge > 0 {
			t.Fatal("ordinary user received privileged session")
		}
	}
}

func TestInviteHTTPControlsAndValidation(t *testing.T) {
	s, u := portalFixture(t)
	for _, body := range []string{`{"max_uses":-1}`, `{"max_uses":1.5}`, `{"expires_at":-1}`, `{"expires_at":1}`, `{"valid_days":0}`, `{"valid_days":7,"expires_at":0}`} {
		w := httptest.NewRecorder()
		s.adminPortalCreateInvite(w, portalRequest(u, "/admin/portal/invites", body))
		if w.Code != 400 {
			t.Fatal("invalid invite accepted", body, w.Code)
		}
	}
	w := httptest.NewRecorder()
	s.adminPortalCreateInvite(w, portalRequest(u, "/admin/portal/invites", `{"max_uses":2,"expires_at":0}`))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var created map[string]any
	json.Unmarshal(w.Body.Bytes(), &created)
	code := created["code"].(string)
	if _, e := s.o.db.CreateInvitedUser("invite-first", "hash", code); e != nil {
		t.Fatal(e)
	}
	w = httptest.NewRecorder()
	if s.inviteRejected(w, code) {
		t.Fatal("multi-use code blocked after first redemption", w.Body.String())
	}
	r := portalRequest(u, "/admin/portal/invites/"+code, `{"max_uses":2,"expires_at":0,"enabled":false,"revision":1}`)
	r.SetPathValue("code", code)
	w = httptest.NewRecorder()
	s.adminPortalUpdateInvite(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	if !s.inviteRejected(w, code) {
		t.Fatal("disabled invite remained usable")
	}
	r = portalRequest(u, "/admin/portal/invites/"+code+"/history", "")
	r.SetPathValue("code", code)
	w = httptest.NewRecorder()
	s.adminPortalInviteHistory(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "invite-first") {
		t.Fatal("redemption history missing", w.Code, w.Body.String())
	}
}

func TestPortalKeyRevealAndLogsAreOwnerOnly(t *testing.T) {
	s, user := portalFixture(t)
	s.o.crypto = appcrypto.NewManager(t.TempDir())
	key := "sk-owner-private-value"
	enc, err := s.o.crypto.Encrypt(key)
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.o.db.CreateApp("owned", s.o.hashKey(key), "sk-owner", "", enc, "[]", user.ID)
	if err != nil {
		t.Fatal(err)
	}
	reveal := func(appID int64, uid int64) *httptest.ResponseRecorder {
		r := portalRequest(&store.User{ID: uid}, "/portal/api/keys/key", "")
		r.Method = "GET"
		r.SetPathValue("id", fmt.Sprint(appID))
		w := httptest.NewRecorder()
		s.portalKey(w, r)
		return w
	}
	for i := 0; i < 2; i++ {
		w := reveal(id, user.ID)
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), key) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, appID := range []int64{id, 999999} {
		w := reveal(appID, user.ID+1)
		if w.Code != 404 || strings.Contains(w.Body.String(), key) {
			t.Fatal("foreign key exposed", w.Code, w.Body.String())
		}
	}
	list := httptest.NewRecorder()
	s.portalListKeys(list, portalRequest(user, "/portal/api/keys", ""))
	if strings.Contains(list.Body.String(), key) || strings.Contains(list.Body.String(), "key_enc") {
		t.Fatal("key list disclosed ciphertext/plaintext")
	}
	diag := &store.UsageDiagnostics{RequestedModel: "test", PhysicalModel: "workbuddy/test", Performance: &store.UsagePerformance{RequestID: "0123456789abcdef0123456789abcdef", HTTPStatus: 400, Attempts: 1}}
	own, err := s.o.db.LogUsageWithID(store.UsageParams{UserID: user.ID, AppID: id, Model: "test", AccountUID: "private-pool-uid", InputContent: "private prompt", OutputContent: "private answer", Error: "Bearer secret", Status: "error", Diagnostics: diag})
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.o.db.LogUsageWithID(store.UsageParams{UserID: user.ID + 1, Model: "foreign-model", Error: "foreign-secret"})
	if err != nil {
		t.Fatal(err)
	}
	r := portalRequest(user, "/portal/api/usage/records", "")
	r.Method = "GET"
	w := httptest.NewRecorder()
	s.portalUsageRecords(w, r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for _, forbidden := range []string{"private-pool-uid", "private prompt", "private answer", "Bearer secret", "foreign-model", "foreign-secret", "account_uid", "input_content"} {
		if strings.Contains(w.Body.String(), forbidden) {
			t.Fatal("portal list leaked", forbidden)
		}
	}
	for _, tc := range []struct {
		id     int64
		status int
	}{{own, 200}, {foreign, 404}, {999999, 404}} {
		r.SetPathValue("id", fmt.Sprint(tc.id))
		w = httptest.NewRecorder()
		s.portalUsageRecord(w, r)
		if w.Code != tc.status {
			t.Fatal(tc, w.Code, w.Body.String())
		}
		if strings.Contains(w.Body.String(), "secret") || strings.Contains(w.Body.String(), "private-") {
			t.Fatal("portal detail leaked", w.Body.String())
		}
	}
}

func TestRuntimeUsageGetsWholeRequestTiming(t *testing.T) {
	s, user := portalFixture(t)
	ctx, timing := streamwatch.WithTiming(context.Background())
	timing.RequestID = "0123456789abcdef0123456789abcdef"
	rows := &usageTimingRows{}
	ctx = context.WithValue(ctx, usageTimingKey{}, rows)
	streamwatch.StartAttempt(ctx)
	reasoning, creation := 3, 0
	known := true
	s.o.logRuntimeUsageDiagnostic(provider.UsageReport{Status: "ok", TokensKnown: &known, InputTokens: 10, OutputTokens: 5, ReasoningTokens: &reasoning, CacheCreationTokens: &creation, FinishReason: "stop", RequestedLimits: map[string]int{"max_output_tokens": 256}, EffectiveLimits: map[string]int{"max_tokens": 256}}, "responses", "trae/test", time.Now(), "key", user.ID, 1, nil, ctx)
	if len(rows.pending) != 1 {
		t.Fatal("runtime usage bypassed timing tracker")
	}
	s.persistUsageTiming(rows, timing.Snapshot(), 200, true, time.Second)
	_, id, err := s.o.db.UsageRowSpan()
	if err != nil || id == 0 {
		t.Fatalf("usage not persisted: id=%d err=%v", id, err)
	}
	row, err := s.o.db.GetUsage(int(id))
	if err != nil {
		t.Fatal(err)
	}
	diag, ok := row["diagnostics"].(store.UsageDiagnostics)
	if !ok || diag.Performance == nil || diag.Performance.HTTPStatus != 200 || !diag.Performance.ResponseStream || diag.Performance.RequestID != timing.RequestID {
		t.Fatal("runtime diagnostic not persisted", row)
	}
	if !diag.Started || diag.FinishReason != "stop" || diag.RequestedLimits["max_output_tokens"] != 256 || diag.EffectiveLimits["max_tokens"] != 256 {
		t.Fatal("runtime termination or budgets lost", diag)
	}
	public := portalLog(row)
	details := public["diagnostics"].(store.UsageDiagnostics).TokenDetails
	if details == nil || details.Reasoning == nil || *details.Reasoning != 3 || details.CacheCreation == nil || *details.CacheCreation != 0 {
		t.Fatal("public observed token details lost", public)
	}
	for _, field := range []string{"account_uid", "input_content", "output_content", "reasoning_content", "credits"} {
		if _, exists := public[field]; exists {
			t.Fatal("private log field exposed", field)
		}
	}
}
