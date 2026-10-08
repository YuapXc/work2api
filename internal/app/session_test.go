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
		principalSessionKey(&Principal{UserID: 1, AppID: 9}, "model-a", body),
	} {
		if a == other || len(a) != 64 {
			t.Fatal("affinity namespace collision")
		}
	}
	if a != principalSessionKey(&Principal{UserID: 1}, "model-a", body) {
		t.Fatal("unstable affinity")
	}
}

func TestRequestSessionIdentitySurvivesChangedMessagesAndConversion(t *testing.T) {
	principal := &Principal{UserID: 1, AppID: 7}
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "first"}}, "metadata": map[string]any{"conversation_id": "body-session"}}
	req := httptest.NewRequest("POST", "/v1/messages", nil)
	req.Header.Set("X-Claude-Code-Session-Id", "session-123")
	req.Header.Set("X-Session-ID", "other-id")
	req = withRequestSessionIdentity(req, body)
	key := requestSessionKey(req, principal, "model", body)
	if key != scopedSessionKey(principal, "model", "sid:session-123") {
		t.Fatal("official header priority lost")
	}
	if key != requestSessionKey(req, principal, "model", nil) {
		t.Fatal("conversion changed session identity")
	}
	for _, raw := range []string{`{"session_id":"session-123","device_id":"private"}`, "user_device_account_account_session_12345678-1234-1234-1234-123456789abc"} {
		if got := extractSessionKey(map[string]any{"metadata": map[string]any{"user_id": raw}}); !strings.HasPrefix(got, "sid:") {
			t.Fatal("legacy session not extracted", got)
		}
	}
	if extractSessionKey(map[string]any{"metadata": map[string]any{"user_id": "ordinary-user-id"}}) != "" {
		t.Fatal("user treated as conversation")
	}
	for _, value := range []string{strings.Repeat("x", 513), "bad\x00id"} {
		r := httptest.NewRequest("POST", "/v1/messages", nil)
		r.Header.Set("X-Session-Id", value)
		r = withRequestSessionIdentity(r, map[string]any{"session_id": "body-session"})
		if got := requestSessionKey(r, principal, "model", nil); got != scopedSessionKey(principal, "model", "sid:body-session") {
			t.Fatal("invalid header overrode valid body")
		}
	}
}

func TestManualOutcomeTracksFallbackAndIgnoresOlderControl(t *testing.T) {
	r := newSessionRouter()
	ctx, finish := r.begin(context.Background(), "tracked", "model", nil, nil)
	defer finish()
	v, _ := r.view("tracked")
	r.control(v.ID, v.Version, "switch", "target")
	v, _ = r.view("tracked")
	call := ctx.Value(sessionCallKey{}).(*sessionCall)
	if !r.commit(&sessionSelection{key: v.ID, version: v.Version, action: "switch", call: call}, "target") {
		t.Fatal("commit")
	}
	v, _ = r.view("tracked")
	if v.RouteStatus != "selected" {
		t.Fatal("selection prematurely succeeded")
	}
	r.attempt(ctx, "target")
	r.outcome(ctx, "target", io.ErrUnexpectedEOF)
	r.unbindMatching("tracked", "target")
	v, _ = r.view("tracked")
	r.commit(&sessionSelection{key: v.ID, version: v.Version}, "backup")
	r.attempt(ctx, "backup")
	r.outcome(ctx, "backup", nil)
	v, _ = r.view("tracked")
	if v.RouteStatus != "fallback" || v.LastSuccess != "backup" || v.LastAttempt != "backup" || v.UID != "backup" {
		t.Fatalf("wrong fallback state: %+v", v)
	}
	r.control(v.ID, v.Version, "switch", "new-target")
	r.outcome(ctx, "backup", io.ErrUnexpectedEOF)
	v, _ = r.view("tracked")
	if v.RouteStatus != "pending" || v.Target != "new-target" {
		t.Fatal("old outcome overwrote new control")
	}
}

func TestPaidSessionMigrationHonorsManualPinCostAndScope(t *testing.T) {
	o, first := newDNSFailoverOrch(t, &failingUpstream{})
	o.pool = pool.New(map[string]pool.Credential{first.UID: catalogOffline{first.Mgr}, "domestic": catalogOffline{first.Mgr}}, "")
	intl, cn := o.pool.Get(first.UID), o.pool.Get("domestic")
	intl.Profile = "intl-work"
	cn.Profile = "cn-cli"
	ready := map[string]bool{intl.UID: true, cn.UID: true}
	paid := map[string]float64{intl.UID: 1, cn.UID: 1}
	if got := o.preferDomesticSession("s", intl, ready, paid, 7); got != cn {
		t.Fatal("paid session did not migrate")
	}
	for _, cost := range []map[string]float64{nil, {intl.UID: 0, cn.UID: 0}, {intl.UID: 1, cn.UID: 2}} {
		if got := o.preferDomesticSession("s", intl, ready, cost, 7); got != intl {
			t.Fatal("free/unknown/cost priority changed")
		}
	}
	if got := o.preferDomesticSession("s", intl, map[string]bool{intl.UID: true}, paid, 7); got != intl {
		t.Fatal("migration escaped scope")
	}
	o.sessions.bind("s", cn.UID)
	_, finish := o.sessions.begin(context.Background(), "s", "model", nil, nil)
	defer finish()
	v, _ := o.sessions.view("s")
	o.sessions.control("s", v.Version, "switch", intl.UID)
	v, _ = o.sessions.view("s")
	o.sessions.commit(&sessionSelection{key: "s", version: v.Version, action: "switch"}, intl.UID)
	if got := o.preferDomesticSession("s", intl, ready, paid, 7); got != intl {
		t.Fatal("automatic migration overrode manual international pin")
	}
}

func TestStableSessionAcrossProtocolHandlers(t *testing.T) {
	o, acc := newDNSFailoverOrch(t, &failingUpstream{})
	o.upstreamClient = sessionSuccessfulUpstream{}
	o.models = models.NewWithCatalogClient(o.pool, o.db, &http.Client{Transport: sessionCostCatalog{}})
	o.models.Refresh()
	app, err := o.db.CreateApp("identity", o.hashKey("identity-key"), "", "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(o)
	for _, protocol := range []string{"chat", "anthropic", "responses"} {
		for turn := 0; turn < 2; turn++ {
			body := map[string]any{"model": "test-model", "stream": false, "messages": []any{map[string]any{"role": "user", "content": strings.Repeat("changed", turn+1)}}}
			path := "/v1/chat/completions"
			if protocol == "anthropic" {
				path = "/v1/messages"
				body["max_tokens"] = 64
			}
			if protocol == "responses" {
				path = "/v1/responses"
				body["input"] = "changed input"
				delete(body, "messages")
			}
			raw, _ := json.Marshal(body)
			req := httptest.NewRequest("POST", path, strings.NewReader(string(raw)))
			req.Header.Set("Authorization", "Bearer identity-key")
			req.Header.Set("X-Claude-Code-Session-ID", "stable")
			if turn == 1 {
				req.Header.Set("X-Claude-Code-Agent-ID", "child")
			}
			w := httptest.NewRecorder()
			handler := http.HandlerFunc(s.handleChat)
			if protocol != "chat" {
				handler = func(w http.ResponseWriter, r *http.Request) { s.handleConverted(w, r, protocol) }
			}
			s.requestGuard(handler).ServeHTTP(w, req)
			if w.Code != 200 {
				t.Fatalf("%s: %d %s", protocol, w.Code, w.Body)
			}
		}
	}
	key := scopedSessionKey(&Principal{AppID: app}, "test-model", "sid:stable")
	v, ok := o.sessions.view(key)
	if !ok || v.Requests != 6 || v.AgentRequests != 3 || v.Running != 0 || v.Waiting != 0 || v.UID != acc.UID || len(o.sessions.views()) != 1 {
		t.Fatalf("fragmented or duplicated requests: %+v", v)
	}
}

func TestExtractSessionKey(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"prompt_cache_key wins", map[string]any{"prompt_cache_key": "abc", "user": "longuser123"}, "pck:abc"},
		{"metadata conversation_id", map[string]any{"metadata": map[string]any{"conversation_id": "conv-9"}}, "sid:conv-9"},
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
	call, _ := ctx.Value(sessionCallKey{}).(*sessionCall)
	if !r.commit(&sessionSelection{key: key, version: v.Version, action: "switch", call: call}, "b") {
		t.Fatal("fresh selection failed")
	}
	r.attempt(ctx, "b")
	r.outcome(ctx, "b", nil)
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
	v, _ = o.sessions.view(key)
	o.sessions.control(key, v.Version, "reselect", "")
	picked, aerr = choose(map[string]bool{})
	v, _ = o.sessions.view(key)
	if picked != nil || aerr != nil || v.Pending != "" || !strings.Contains(v.RouteMessage, "没有其他同成本") {
		t.Fatal("reselection without an authorized alternative was not explained")
	}
	v, _ = o.sessions.view(key)
	o.sessions.control(key, v.Version, "reselect", "")
	choose, aerr = o.accountSelector("test-model", "second", nil, map[string]bool{acc.UID: true, "second": true}, selection)
	if aerr != nil {
		t.Fatal(aerr)
	}
	picked, aerr = choose(map[string]bool{acc.UID: true})
	if picked != nil || aerr != nil {
		t.Fatal("reselection bypassed its busy alternative by returning to the original account")
	}
	picked, aerr = choose(map[string]bool{})
	if aerr != nil || picked == nil || picked.UID != acc.UID || selection.target != acc.UID {
		t.Fatal("reselection did not exclude the original account")
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
	app, err := o.db.CreateApp("queue", o.hashKey("queue-key"), "", "", "", "", 0)
	if err != nil {
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
	key := scopedSessionKey(&Principal{AppID: app}, "test-model", "sid:queue-session")
	o.sessions.bind(key, acc.UID)
	payload, _ := json.Marshal(body)
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(payload))).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer queue-key")
		req.Header.Set("X-Claude-Code-Session-ID", "queue-session")
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
