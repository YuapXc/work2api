package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"work2api/internal/workbuddy/models"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/upstream"
)

func TestSessionAffinityIsolatedByUserAndModel(t *testing.T) {
	body := map[string]any{"prompt_cache_key": "same-conversation"}
	a := principalSessionKey(&Principal{UserID: 1}, "model-a", body)
	for _, other := range []string{
		principalSessionKey(&Principal{UserID: 2}, "model-a", body),
		principalSessionKey(&Principal{UserID: 1}, "model-b", body),
		principalSessionKey(&Principal{}, "model-a", body),
	} {
		if a == other || len(a) != 64 {
			t.Fatal("affinity namespace collision")
		}
	}
	if a != principalSessionKey(&Principal{UserID: 1}, "model-a", body) {
		t.Fatal("unstable affinity")
	}
}

func TestExtractSessionKey(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"prompt_cache_key wins", map[string]any{"prompt_cache_key": "abc", "user": "longuser123"}, "pck:abc"},
		{"metadata conversation_id", map[string]any{"metadata": map[string]any{"conversation_id": "conv-9"}}, "cid:conv-9"},
		{"user field", map[string]any{"user": "session-12345"}, "user:session-12345"},
		{"user too short ignored, falls to msg", map[string]any{"user": "abc", "messages": []any{map[string]any{"role": "user", "content": "hi there"}}}, "fb:"},
		{"message text fingerprint", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello world"}}}, "fb:"},
		{"content blocks", map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}}}}}, "fb:"},
		{"no session features", map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": "x"}}}, ""},
		{"nil", nil, ""},
	}
	for _, c := range cases {
		got := extractSessionKey(c.body)
		if c.want == "fb:" {
			if len(got) < 3 || got[:3] != "fb:" {
				t.Errorf("%s: got %q, want fb:* prefix", c.name, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	// 同一首条 user 消息在多轮追加后指纹保持稳定（粘性键的核心保证）
	turn1 := extractSessionKey(map[string]any{"messages": []any{map[string]any{"role": "user", "content": "start"}}})
	turn2 := extractSessionKey(map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "start"},
		map[string]any{"role": "assistant", "content": "ok"},
		map[string]any{"role": "user", "content": "next"},
	}})
	if turn1 != turn2 || turn1 == "" {
		t.Errorf("fingerprint should be stable across turns: %q vs %q", turn1, turn2)
	}
}

func TestSessionRouter(t *testing.T) {
	r := newSessionRouter()
	if r.lookup("k") != "" {
		t.Error("empty router should miss")
	}
	r.bind("k", "uid1")
	if got := r.lookup("k"); got != "uid1" {
		t.Errorf("lookup after bind = %q, want uid1", got)
	}
	r.unbind("k")
	if r.lookup("k") != "" {
		t.Error("lookup after unbind should miss")
	}
	// 过期项应被解粘
	r.bind("k2", "uid2")
	r.mu.Lock()
	e := r.m["k2"]
	e.expires = nowSec() - 1
	r.m["k2"] = e
	r.mu.Unlock()
	if r.lookup("k2") != "" {
		t.Error("expired entry should be pruned on lookup")
	}
	// 空键不绑定
	r.bind("", "uid")
	if r.lookup("") != "" {
		t.Error("empty key must not bind")
	}
}

func TestSessionRouterHardCapAndIndexCleanup(t *testing.T) {
	r := newSessionRouter()
	r.max = 2
	r.bind("first", "a")
	r.bind("second", "b")
	expires := r.m["first"].expires
	r.lookup("first")
	if r.m["first"].expires != expires {
		t.Fatal("lookup renewed affinity TTL")
	}
	r.bind("third", "c")
	if r.lookup("first") != "" || r.lookup("second") != "b" {
		t.Fatal("oldest binding was not evicted")
	}
	r.bind("second", "b")
	r.bind("fourth", "d")
	if r.lookup("third") != "" {
		t.Fatal("rebinding did not update eviction order")
	}
	if len(r.m) != 2 || r.order.Len() != 2 {
		t.Fatal("affinity cap or index mismatch")
	}
	r.removeAccount("b")
	r.unbind("fourth")
	if len(r.m) != 0 || r.order.Len() != 0 {
		t.Fatal("affinity indexes leaked on removal")
	}
	r.bind("expired", "e")
	entry := r.m["expired"]
	entry.expires = nowSec() - 1
	r.m["expired"] = entry
	if r.lookup("expired") != "" || r.order.Len() != 0 {
		t.Fatal("expired affinity index leaked")
	}
}

func TestSessionControlRevisionAndObservation(t *testing.T) {
	r := newSessionRouter()
	body := map[string]any{"prompt_cache_key": "sensitive-session", "messages": []any{map[string]any{"role": "user", "content": "private-content"}}}
	key := principalSessionKey(&Principal{}, "test-model", body)
	r.bind(key, "a")
	ctx, finish := r.begin(context.Background(), key, "test-model", &Principal{AppName: "agent"}, body)
	sessionPhase(ctx, true)
	v, _ := r.view(key)
	old := &sessionSelection{key: key, version: v.Version}
	if !r.control(key, v.Version, "switch", "b") {
		t.Fatal("control rejected")
	}
	r.bind(key, "a")
	if r.commit(old, "a") {
		t.Fatal("stale selection consumed new preference")
	}
	v, _ = r.view(key)
	if v.UID != "a" || v.Target != "b" || v.Running != 1 {
		t.Fatalf("inflight was changed: %+v", v)
	}
	if !r.commit(&sessionSelection{key: key, version: v.Version}, "b") {
		t.Fatal("fresh selection failed")
	}
	value := 2.5
	sessionCredits(ctx, &value)
	sessionCredits(ctx, nil)
	finish()
	v, _ = r.view(key)
	if v.UID != "b" || v.Pending != "" || v.RouteStatus != "applied" || v.Running != 0 || v.Waiting != 0 || v.Credits != 2.5 || v.Known != 1 || v.Unknown != 1 {
		t.Fatalf("bad completed state: %+v", v)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "private-content") || strings.Contains(string(raw), "sensitive-session") {
		t.Fatal("raw conversation retained")
	}
	if r.control(key, old.version, "cancel", "") {
		t.Fatal("stale control accepted")
	}
}
func TestSessionActiveCapAndExpiry(t *testing.T) {
	r := newSessionRouter()
	r.max = 1
	_, finish := r.begin(context.Background(), "active", "model", nil, nil)
	r.bind("legacy", "a")
	other, done := r.begin(context.Background(), "overflow", "model", nil, nil)
	done()
	if other.Value(sessionCallKey{}) != nil || len(r.m) != 1 || r.order.Len() != 1 {
		t.Fatal("active evicted or hard cap exceeded")
	}
	e := r.m["active"]
	e.expires = nowSec() - 1
	r.m["active"] = e
	if len(r.views()) != 1 {
		t.Fatal("queued session expired")
	}
	finish()
	e = r.m["active"]
	e.expires = nowSec() - 1
	r.m["active"] = e
	if len(r.views()) != 0 || r.order.Len() != 0 {
		t.Fatal("idle session did not expire")
	}
}

type sessionCostCatalog struct{}

func (sessionCostCatalog) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":{"models":[{"id":"test-model","name":"Test","credits":1}],"agents":[{"name":"cli","models":["test-model"]}]}}`)), Request: r}, nil
}
func TestSessionSelectorPendingAndScope(t *testing.T) {
	o, acc := newDNSFailoverOrch(t, &failingUpstream{})
	o.pool = pool.New(map[string]pool.Credential{acc.UID: catalogOffline{acc.Mgr}, "second": catalogOffline{acc.Mgr}}, "")
	o.models = models.NewWithCatalogClient(o.pool, o.db, &http.Client{Transport: sessionCostCatalog{}})
	o.models.Refresh()
	key := "tracked"
	o.sessions.bind(key, acc.UID)
	_, finish := o.sessions.begin(context.Background(), key, "test-model", &Principal{AccountScope: map[string]bool{acc.UID: true, "second": true}}, nil)
	defer finish()
	v, _ := o.sessions.view(key)
	if !o.sessions.control(key, v.Version, "switch", "second") {
		t.Fatal("control")
	}
	selection := &sessionSelection{key: key}
	choose, aerr := o.accountSelector("test-model", acc.UID, nil, map[string]bool{acc.UID: true, "second": true}, selection)
	if aerr != nil {
		t.Fatal(aerr)
	}
	picked, aerr := choose(map[string]bool{"second": true})
	if picked != nil || aerr != nil {
		t.Fatalf("busy manual target bypassed: %v %v", picked, aerr)
	}
	v, _ = o.sessions.view(key)
	if v.Pending != "switch" {
		t.Fatal("dispatch consumed pending preference")
	}
	picked, aerr = choose(map[string]bool{})
	if aerr != nil || picked == nil || picked.UID != "second" {
		t.Fatalf("pending target not selected: %v %v", picked, aerr)
	}
	if !o.sessions.commit(selection, picked.UID) {
		t.Fatal("commit failed")
	}
	v, _ = o.sessions.view(key)
	o.sessions.control(key, v.Version, "switch", acc.UID)
	choose, aerr = o.accountSelector("test-model", "second", nil, map[string]bool{"second": true}, selection)
	if aerr != nil {
		t.Fatal(aerr)
	}
	picked, aerr = choose(map[string]bool{})
	if picked != nil || aerr != nil {
		t.Fatal("unauthorized pending account selected")
	}
	v, _ = o.sessions.view(key)
	if v.Pending != "" || v.RouteStatus != "failed" {
		t.Fatal("invalid target not rejected")
	}
	picked, aerr = choose(map[string]bool{})
	if aerr != nil || picked == nil || picked.UID != "second" {
		t.Fatal("automatic routing did not recover")
	}
}
func TestSessionAdminAuthenticationAndVersion(t *testing.T) {
	o, _ := newDNSFailoverOrch(t, &failingUpstream{})
	o.cfg.AdminToken = "test-admin-token"
	_, finish := o.sessions.begin(context.Background(), "tracked", "test-model", nil, nil)
	defer finish()
	h := NewServer(o).Handler()
	request := func(method, path, body, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, "http://localhost"+path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("X-Admin-Token", token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	if w := request("GET", "/admin/sessions", "", ""); w.Code != 401 {
		t.Fatalf("unauthenticated session endpoint: %d", w.Code)
	}
	v, _ := o.sessions.view("tracked")
	raw, _ := json.Marshal(map[string]any{"version": v.Version, "action": "reselect"})
	if w := request("POST", "/admin/sessions/tracked", string(raw), "test-admin-token"); w.Code != 200 {
		t.Fatalf("control rejected: %d %s", w.Code, w.Body)
	}
	if w := request("POST", "/admin/sessions/tracked", string(raw), "test-admin-token"); w.Code != 409 {
		t.Fatalf("stale control accepted: %d", w.Code)
	}
	if w := request("GET", "/admin/sessions", "", "test-admin-token"); w.Code != 200 || !strings.Contains(w.Body.String(), `"id":"tracked"`) || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("invalid session response: %d %s", w.Code, w.Body)
	}
}

func TestSessionQueuedRequestUsesManualTarget(t *testing.T) {
	o, acc := newDNSFailoverOrch(t, &failingUpstream{emit: true})
	o.cfg.MaxConcurrentRequests = 1
	o.upstreamClient = sessionSuccessfulUpstream{}
	o.pool = pool.New(map[string]pool.Credential{acc.UID: catalogOffline{acc.Mgr}, "second": catalogOffline{acc.Mgr}}, "")
	o.managers["second"] = o.managers[acc.UID]
	o.models = models.NewWithCatalogClient(o.pool, o.db, &http.Client{Transport: sessionCostCatalog{}})
	o.models.Refresh()
	if _, err := o.db.CreateApp("queue", o.hashKey("queue-key"), "", "", "", "", 0); err != nil {
		t.Fatal(err)
	}
	s := NewServer(o)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	blocker, err := s.modelsAdmission.acquire(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.release()
	body := map[string]any{"model": "test-model", "prompt_cache_key": "queue-session", "messages": []any{map[string]any{"role": "user", "content": "hello"}}}
	key := principalSessionKey(&Principal{}, "test-model", body)
	o.sessions.bind(key, acc.UID)
	payload, _ := json.Marshal(body)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(payload))).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer queue-key")
		w := httptest.NewRecorder()
		s.requestGuard(http.HandlerFunc(s.handleChat)).ServeHTTP(w, req)
		result <- w
	}()
	awaitQueue(t, s.modelsAdmission, 1)
	v, ok := o.sessions.view(key)
	if !ok || v.Waiting != 1 || v.Requests != 1 {
		t.Fatalf("global queue not observed: %+v", v)
	}
	if !o.sessions.control(key, v.Version, "switch", "second") {
		t.Fatal("control failed")
	}
	blocker.release()
	select {
	case w := <-result:
		if w.Code != 200 {
			t.Fatalf("queued call failed: %d %s", w.Code, w.Body)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	v, _ = o.sessions.view(key)
	if v.UID != "second" || v.Pending != "" || v.RouteStatus != "applied" || v.Requests != 1 || v.Running != 0 || v.Waiting != 0 {
		t.Fatalf("queued control not honored: %+v", v)
	}
	if s.modelsAdmission.snapshot()["running"] != 0 {
		t.Fatal("admission leaked")
	}
}

type sessionSuccessfulUpstream struct{}

func (sessionSuccessfulUpstream) StreamUpstream(_ context.Context, _ map[string]string, _ map[string]any, _ string, yield upstream.LineFunc) error {
	if err := yield(`data: {"choices":[{"delta":{"content":"hello"}}]}`); err != nil {
		return err
	}
	return yield(`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"credit":1.5}}`)
}

func TestSessionOldFailurePreservesNewBinding(t *testing.T) {
	r := newSessionRouter()
	r.bind("session", "old")
	_, finish := r.begin(context.Background(), "session", "model", nil, nil)
	defer finish()
	v, _ := r.view("session")
	if !r.control("session", v.Version, "switch", "new") {
		t.Fatal("control")
	}
	v, _ = r.view("session")
	if !r.commit(&sessionSelection{key: "session", version: v.Version}, "new") {
		t.Fatal("commit")
	}
	v, _ = r.view("session")
	r.unbindMatching("session", "old")
	after, _ := r.view("session")
	if after.UID != "new" || after.Version != v.Version {
		t.Fatal("old request unbound newer affinity")
	}
	r.unbindMatching("session", "new")
	after, _ = r.view("session")
	if after.UID != "" || after.Version == v.Version {
		t.Fatal("actual failed affinity was not cleared")
	}
}
