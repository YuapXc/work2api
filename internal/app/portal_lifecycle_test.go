package app

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"work2api/internal/portal/portalauth"
	"work2api/internal/store"
)

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
