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

func TestResponsesSessionFallbackPreservesOriginalIdentity(t *testing.T) {
	first := map[string]any{"role": "user", "content": "first request"}
	want := extractSessionKey(map[string]any{"messages": []any{first}})
	for _, input := range []any{
		"first request",
		[]any{first},
		[]any{map[string]any{"type": "function_call_output", "role": "user", "content": "tool result"}, map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "first request"}}}, map[string]any{"role": "assistant", "content": "answer"}, map[string]any{"role": "user", "content": "next turn"}},
	} {
		body := map[string]any{"input": input}
		if got := extractSessionKey(body); got != want {
			t.Fatalf("Responses fallback mismatch: %s != %s", got, want)
		}
		p := &Principal{UserID: 1, AppID: 7}
		r := withRequestSessionIdentity(httptest.NewRequest("POST", "/v1/responses", nil), body)
		if requestSessionKey(r, p, "model", nil) != scopedSessionKey(p, "model", want) {
			t.Fatal("original identity lost after conversion")
		}
		body["metadata"] = map[string]any{"session_id": "explicit"}
		if got := extractSessionKey(body); got != "sid:explicit" {
			t.Fatal("fallback overrode explicit identity", got)
		}
	}
	for _, input := range []any{" ", []any{map[string]any{"type": "function_call_output", "output": "not a conversation"}}, []any{map[string]any{"role": "assistant", "content": "answer"}}, []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "text": "not user text"}}}}} {
		if got := extractSessionKey(map[string]any{"input": input}); got != "" {
			t.Fatal("invented a session from non-user text", got)
		}
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
	ctx, finish := r.Begin(context.Background(), "tracked", "model", nil, nil)
	defer finish()
	v, _ := r.View("tracked")
	r.Control(v.ID, v.Version, "switch", "target")
	v, _ = r.View("tracked")
	call := ctx.Value(sessionCallKey{}).(*sessionCall)
	if !r.Commit(&sessionSelection{Key: v.ID, Version: v.Version, Action: "switch", Call: call}, "target") {
		t.Fatal("commit")
	}
	v, _ = r.View("tracked")
	if v.RouteStatus != "selected" {
		t.Fatal("selection prematurely succeeded")
	}
	r.Attempt(ctx, "target")
	r.Outcome(ctx, "target", io.ErrUnexpectedEOF)
	r.UnbindMatching("tracked", "target")
	v, _ = r.View("tracked")
	r.Commit(&sessionSelection{Key: v.ID, Version: v.Version}, "backup")
	r.Attempt(ctx, "backup")
	r.Outcome(ctx, "backup", nil)
	v, _ = r.View("tracked")
	if v.RouteStatus != "fallback" || v.LastSuccess != "backup" || v.LastAttempt != "backup" || v.UID != "backup" {
		t.Fatalf("wrong fallback state: %+v", v)
	}
	r.Control(v.ID, v.Version, "switch", "new-target")
	r.Outcome(ctx, "backup", io.ErrUnexpectedEOF)
	v, _ = r.View("tracked")
	if v.RouteStatus != "pending" || v.Target != "new-target" {
		t.Fatal("old outcome overwrote new control")
	}
}

func TestPaidSessionMigrationHonorsManualPinCostAndScope(t *testing.T) {
	o, first := newDNSFailoverOrch(t, &failingUpstream{})
	o.wb.Pool = pool.New(map[string]pool.Credential{first.UID: catalogOffline{first.Mgr}, "domestic": catalogOffline{first.Mgr}}, "")
	intl, cn := o.wb.Pool.Get(first.UID), o.wb.Pool.Get("domestic")
	intl.Profile = "intl-work"
	cn.Profile = "cn-cli"
	ready := map[string]bool{intl.UID: true, cn.UID: true}
	paid := map[string]float64{intl.UID: 1, cn.UID: 1}
	if got := o.preferDomesticSession("test-model", "s", intl, ready, paid, 7); got != cn {
		t.Fatal("paid session did not migrate")
	}
	for _, cost := range []map[string]float64{nil, {intl.UID: 0, cn.UID: 0}, {intl.UID: 1, cn.UID: 2}} {
		if got := o.preferDomesticSession("test-model", "s", intl, ready, cost, 7); got != intl {
			t.Fatal("free/unknown/cost priority changed")
		}
	}
	if got := o.preferDomesticSession("test-model", "s", intl, map[string]bool{intl.UID: true}, paid, 7); got != intl {
		t.Fatal("migration escaped scope")
	}
	o.sessions.Bind("s", cn.UID)
	_, finish := o.sessions.Begin(context.Background(), "s", "model", nil, nil)
	defer finish()
	v, _ := o.sessions.View("s")
	o.sessions.Control("s", v.Version, "switch", intl.UID)
	v, _ = o.sessions.View("s")
	o.sessions.Commit(&sessionSelection{Key: "s", Version: v.Version, Action: "switch"}, intl.UID)
	if got := o.preferDomesticSession("test-model", "s", intl, ready, paid, 7); got != intl {
		t.Fatal("automatic migration overrode manual international pin")
	}
}

func TestStableSessionAcrossProtocolHandlers(t *testing.T) {
	o, acc := newDNSFailoverOrch(t, &failingUpstream{})
	o.wb.UpstreamClient = sessionSuccessfulUpstream{}
	o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: sessionCostCatalog{}})
	o.wb.Catalog.Refresh()
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
	v, ok := o.sessions.View(key)
	if !ok || v.Requests != 6 || v.AgentRequests != 3 || v.Running != 0 || v.Waiting != 0 || v.UID != acc.UID || len(o.sessions.Views()) != 1 {
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
	if r.Lookup("k") != "" {
		t.Error("empty router should miss")
	}
	r.Bind("k", "uid1")
	if got := r.Lookup("k"); got != "uid1" {
		t.Errorf("lookup after bind = %q, want uid1", got)
	}
	r.Unbind("k")
	if r.Lookup("k") != "" {
		t.Error("lookup after unbind should miss")
	}
	// 过期项应被解粘
	r.Bind("k2", "uid2")
	r.Mu.Lock()
	e := r.M["k2"]
	e.Expires = nowSec() - 1
	r.M["k2"] = e
	r.Mu.Unlock()
	if r.Lookup("k2") != "" {
		t.Error("expired entry should be pruned on lookup")
	}
	// 空键不绑定
	r.Bind("", "uid")
	if r.Lookup("") != "" {
		t.Error("empty key must not bind")
	}
}

func TestSessionRouterHardCapAndIndexCleanup(t *testing.T) {
	r := newSessionRouter()
	r.Max = 2
	r.Bind("first", "a")
	r.Bind("second", "b")
	expires := r.M["first"].Expires
	r.Lookup("first")
	if r.M["first"].Expires != expires {
		t.Fatal("lookup renewed affinity TTL")
	}
	r.Bind("third", "c")
	if r.Lookup("first") != "" || r.Lookup("second") != "b" {
		t.Fatal("oldest binding was not evicted")
	}
	r.Bind("second", "b")
	r.Bind("fourth", "d")
	if r.Lookup("third") != "" {
		t.Fatal("rebinding did not update eviction order")
	}
	if len(r.M) != 2 || r.Order.Len() != 2 {
		t.Fatal("affinity cap or index mismatch")
	}
	r.RemoveAccount("b")
	r.Unbind("fourth")
	if len(r.M) != 0 || r.Order.Len() != 0 {
		t.Fatal("affinity indexes leaked on removal")
	}
	r.Bind("expired", "e")
	entry := r.M["expired"]
	entry.Expires = nowSec() - 1
	r.M["expired"] = entry
	if r.Lookup("expired") != "" || r.Order.Len() != 0 {
		t.Fatal("expired affinity index leaked")
	}
}

func TestSessionControlRevisionAndObservation(t *testing.T) {
	r := newSessionRouter()
	body := map[string]any{"prompt_cache_key": "sensitive-session", "messages": []any{map[string]any{"role": "user", "content": "private-content"}}}
	key := principalSessionKey(&Principal{}, "test-model", body)
	r.Bind(key, "a")
	ctx, finish := r.Begin(context.Background(), key, "test-model", &Principal{AppName: "agent"}, body)
	sessionPhase(ctx, true)
	v, _ := r.View(key)
	old := &sessionSelection{Key: key, Version: v.Version}
	if !r.Control(key, v.Version, "switch", "b") {
		t.Fatal("control rejected")
	}
	r.Bind(key, "a")
	if r.Commit(old, "a") {
		t.Fatal("stale selection consumed new preference")
	}
	v, _ = r.View(key)
	if v.UID != "a" || v.Target != "b" || v.Running != 1 {
		t.Fatalf("inflight was changed: %+v", v)
	}
	call, _ := ctx.Value(sessionCallKey{}).(*sessionCall)
	if !r.Commit(&sessionSelection{Key: key, Version: v.Version, Action: "switch", Call: call}, "b") {
		t.Fatal("fresh selection failed")
	}
	r.Attempt(ctx, "b")
	r.Outcome(ctx, "b", nil)
	value := 2.5
	sessionCredits(ctx, &value)
	sessionCredits(ctx, nil)
	finish()
	v, _ = r.View(key)
	if v.UID != "b" || v.Pending != "" || v.RouteStatus != "applied" || v.Running != 0 || v.Waiting != 0 || v.Credits != 2.5 || v.Known != 1 || v.Unknown != 1 {
		t.Fatalf("bad completed state: %+v", v)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "private-content") || strings.Contains(string(raw), "sensitive-session") {
		t.Fatal("raw conversation retained")
	}
	if r.Control(key, old.Version, "cancel", "") {
		t.Fatal("stale control accepted")
	}
}
func TestSessionActiveCapAndExpiry(t *testing.T) {
	r := newSessionRouter()
	r.Max = 1
	_, finish := r.Begin(context.Background(), "active", "model", nil, nil)
	r.Bind("legacy", "a")
	other, done := r.Begin(context.Background(), "overflow", "model", nil, nil)
	done()
	if other.Value(sessionCallKey{}) != nil || len(r.M) != 1 || r.Order.Len() != 1 {
		t.Fatal("active evicted or hard cap exceeded")
	}
	e := r.M["active"]
	e.Expires = nowSec() - 1
	r.M["active"] = e
	if len(r.Views()) != 1 {
		t.Fatal("queued session expired")
	}
	finish()
	e = r.M["active"]
	e.Expires = nowSec() - 1
	r.M["active"] = e
	if len(r.Views()) != 0 || r.Order.Len() != 0 {
		t.Fatal("idle session did not expire")
	}
}

type sessionCostCatalog struct{}

func (sessionCostCatalog) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"data":{"models":[{"id":"test-model","name":"Test","credits":1}],"agents":[{"name":"cli","models":["test-model"]}]}}`)), Request: r}, nil
}

type accountQuoteCatalog struct{ paidFree bool }

func TestDefaultModelPermissionUsesAccountQuoteWithoutConstrainingRouting(t *testing.T) {
	s, owner := portalFixture(t)
	o := s.o
	original := o.wb.Pool.Accounts()[0]
	o.wb.Pool = pool.New(map[string]pool.Credential{
		"free": portalCatalogCredential{original.Mgr, "free"},
		"paid": portalCatalogCredential{original.Mgr, "paid"},
	}, "")
	transport := &accountQuoteCatalog{}
	o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: transport})
	o.wb.Catalog.Refresh()
	check := func(expected bool) {
		t.Helper()
		defaults, scopes := o.defaultModelAccess(map[string]string{})
		if defaults["test-model"] != expected || !scopes["test-model"]["paid"] {
			t.Fatal("free permission lost account evidence or changed routing scope", defaults, scopes)
		}
	}
	check(true)
	o.wb.Pool.SetEnabled("free", false, "manual")
	check(false)
	o.wb.Pool.SetEnabled("free", true, "manual")
	check(true)
	contribution, err := o.db.CreateContribution(owner.ID, "free", "workbuddy", "codebuddy", "active")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := o.db.WithdrawContribution(contribution, owner.ID, "withdraw"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	check(false)
	if ok, err := o.db.RestoreContributionSharing(contribution, owner.ID); err != nil || !ok {
		t.Fatal(ok, err)
	}
	check(true)
	transport.paidFree = true
	o.wb.Catalog.Refresh()
	check(false)
	defaults, _ := o.defaultModelAccess(map[string]string{"portal_model_policy": `{"mode":"auto","include":["test-model"],"exclude":[]}`})
	if !defaults["test-model"] {
		t.Fatal("manual inclusion was overridden by paid price")
	}
}

func (c *accountQuoteCatalog) RoundTrip(r *http.Request) (*http.Response, error) {
	price := any(nil)
	switch r.Header.Get("x-test-models") {
	case "free":
		price = float64(0)
		if c.paidFree {
			price = float64(1)
		}
	case "paid":
		price = float64(1)
	}
	raw, _ := json.Marshal(map[string]any{"data": map[string]any{
		"models": []any{map[string]any{"id": "test-model", "credits": price}, map[string]any{"id": "other-model", "credits": 1}},
		"agents": []any{map[string]any{"name": "cli", "models": []string{"test-model", "other-model"}}},
	}})
	return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(raw))), Request: r}, nil
}

func TestAccountSpecificFreeRoutingAndLiveRevocation(t *testing.T) {
	o, original := newDNSFailoverOrch(t, &failingUpstream{})
	o.wb.Pool = pool.New(map[string]pool.Credential{
		"free":    portalCatalogCredential{original.Mgr, "free"},
		"paid":    portalCatalogCredential{original.Mgr, "paid"},
		"unknown": portalCatalogCredential{original.Mgr, "unknown"},
	}, "")
	transport := &accountQuoteCatalog{}
	o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: transport})
	o.wb.Catalog.Refresh()
	zero, positive := 0.0, 10.0
	for _, uid := range []string{"free", "paid", "unknown"} {
		o.wb.Pool.SetCredits(uid, &zero, &positive, nil, nil)
	}
	cost := o.modelCostByUID("test-model", map[string]bool{"free": true, "paid": true, "unknown": true})
	if len(cost) != 2 || cost["free"] != 0 || cost["paid"] != 1 {
		t.Fatal("account quotes mixed", cost)
	}
	for _, mode := range []string{"1", "0"} {
		if err := o.db.SaveSettings(map[string]string{"cost_aware_routing": mode}); err != nil {
			t.Fatal(err)
		}
		for _, scope := range []map[string]bool{nil, {"free": true}} {
			for i := 0; i < 2; i++ {
				a, err := o.pickAccountExcludingIn("test-model", "free-session", nil, scope)
				if err != nil || a.UID != "free" {
					t.Fatal("free account unavailable or lost sticky binding", a, err)
				}
			}
			choose, err := o.accountSelector("test-model", "free", nil, scope)
			if err != nil {
				t.Fatal(err)
			}
			a, err := choose(nil)
			if err != nil || a == nil || a.UID != "free" {
				t.Fatal("selector discarded free account", a, err)
			}
		}
		for _, scope := range []map[string]bool{{"paid": true}, {"unknown": true}} {
			if a, err := o.pickAccountExcludingIn("test-model", "", nil, scope); err == nil || a != nil {
				t.Fatal("borrowed another account's free quote", a)
			}
		}
	}
	if a, err := o.pickAccountExcludingIn("other-model", "", nil, map[string]bool{"free": true}); err == nil || a != nil {
		t.Fatal("free entitlement applied to paid model")
	}
	account := o.wb.Pool.Get("free")
	if !o.wb.AccountHealthy(account, nowSec()) {
		t.Fatal("free-only account marked unavailable")
	}
	rows := o.wb.Catalog.ListCached()
	o.wb.AttachModelAccounts(rows)
	for _, row := range rows {
		for _, raw := range row["accounts"].([]map[string]any) {
			if raw["uid"] == "free" && raw["healthy"] != (row["id"] == "test-model") {
				t.Fatal("model status ignored per-model entitlement", row)
			}
		}
	}
	o.wb.Pool.SetEnabled("free", false, "manual")
	if a, err := o.pickAccountExcludingIn("test-model", "", nil, nil); err == nil || a != nil {
		t.Fatal("free pricing bypassed disablement")
	}
	o.wb.Pool.SetEnabled("free", true, "")
	o.wb.Pool.OnFailure("free", 600)
	if a, err := o.pickAccountExcludingIn("test-model", "", nil, nil); err == nil || a != nil {
		t.Fatal("free pricing bypassed cooldown")
	}
	o.wb.Pool.ClearCooldown("free")
	choose, err := o.accountSelector("test-model", "free", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	transport.paidFree = true
	o.wb.Catalog.Refresh()
	if a, err := choose(nil); err == nil && a != nil {
		t.Fatal("queued selection retained revoked free entitlement")
	}
	if o.wb.AccountHealthy(account, nowSec()) {
		t.Fatal("revoked free account still marked healthy")
	}
	// Restoring paid balance re-enables a paid model without needing a free quote.
	o.wb.Pool.SetCredits("free", &positive, &positive, nil, nil)
	if a, err := o.pickAccountExcludingIn("other-model", "", nil, map[string]bool{"free": true}); err != nil || a == nil {
		t.Fatal("balance refresh did not restore paid models", err)
	}
}
func TestSessionSelectorPendingAndScope(t *testing.T) {
	o, acc := newDNSFailoverOrch(t, &failingUpstream{})
	o.wb.Pool = pool.New(map[string]pool.Credential{acc.UID: catalogOffline{acc.Mgr}, "second": catalogOffline{acc.Mgr}}, "")
	o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: sessionCostCatalog{}})
	o.wb.Catalog.Refresh()
	key := "tracked"
	o.sessions.Bind(key, acc.UID)
	_, finish := o.sessions.Begin(context.Background(), key, "test-model", &Principal{AccountScope: map[string]bool{acc.UID: true, "second": true}}, nil)
	defer finish()
	v, _ := o.sessions.View(key)
	if !o.sessions.Control(key, v.Version, "switch", "second") {
		t.Fatal("control")
	}
	selection := &sessionSelection{Key: key}
	choose, aerr := o.accountSelector("test-model", acc.UID, nil, map[string]bool{acc.UID: true, "second": true}, selection)
	if aerr != nil {
		t.Fatal(aerr)
	}
	picked, aerr := choose(map[string]bool{"second": true})
	if picked != nil || aerr != nil {
		t.Fatalf("busy manual target bypassed: %v %v", picked, aerr)
	}
	v, _ = o.sessions.View(key)
	if v.Pending != "switch" {
		t.Fatal("dispatch consumed pending preference")
	}
	picked, aerr = choose(map[string]bool{})
	if aerr != nil || picked == nil || picked.UID != "second" {
		t.Fatalf("pending target not selected: %v %v", picked, aerr)
	}
	if !o.sessions.Commit(selection, picked.UID) {
		t.Fatal("commit failed")
	}
	v, _ = o.sessions.View(key)
	o.sessions.Control(key, v.Version, "switch", acc.UID)
	choose, aerr = o.accountSelector("test-model", "second", nil, map[string]bool{"second": true}, selection)
	if aerr != nil {
		t.Fatal(aerr)
	}
	picked, aerr = choose(map[string]bool{})
	if picked != nil || aerr != nil {
		t.Fatal("unauthorized pending account selected")
	}
	v, _ = o.sessions.View(key)
	if v.Pending != "" || v.RouteStatus != "failed" {
		t.Fatal("invalid target not rejected")
	}
	picked, aerr = choose(map[string]bool{})
	if aerr != nil || picked == nil || picked.UID != "second" {
		t.Fatal("automatic routing did not recover")
	}
	v, _ = o.sessions.View(key)
	o.sessions.Control(key, v.Version, "reselect", "")
	picked, aerr = choose(map[string]bool{})
	v, _ = o.sessions.View(key)
	if picked != nil || aerr != nil || v.Pending != "" || !strings.Contains(v.RouteMessage, "没有其他同成本") {
		t.Fatal("reselection without an authorized alternative was not explained")
	}
	v, _ = o.sessions.View(key)
	o.sessions.Control(key, v.Version, "reselect", "")
	choose, aerr = o.accountSelector("test-model", "second", nil, map[string]bool{acc.UID: true, "second": true}, selection)
	if aerr != nil {
		t.Fatal(aerr)
	}
	picked, aerr = choose(map[string]bool{acc.UID: true})
	if picked != nil || aerr != nil {
		t.Fatal("reselection bypassed its busy alternative by returning to the original account")
	}
	picked, aerr = choose(map[string]bool{})
	if aerr != nil || picked == nil || picked.UID != acc.UID || selection.Target != acc.UID {
		t.Fatal("reselection did not exclude the original account")
	}
}
func TestSessionAdminAuthenticationAndVersion(t *testing.T) {
	o, _ := newDNSFailoverOrch(t, &failingUpstream{})
	o.cfg.AdminToken = "test-admin-token"
	_, finish := o.sessions.Begin(context.Background(), "tracked", "test-model", nil, nil)
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
	v, _ := o.sessions.View("tracked")
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
	o.wb.UpstreamClient = sessionSuccessfulUpstream{}
	o.wb.Pool = pool.New(map[string]pool.Credential{acc.UID: catalogOffline{acc.Mgr}, "second": catalogOffline{acc.Mgr}}, "")
	o.wb.Managers["second"] = o.wb.Managers[acc.UID]
	o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: sessionCostCatalog{}})
	o.wb.Catalog.Refresh()
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
	o.sessions.Bind(key, acc.UID)
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
	v, ok := o.sessions.View(key)
	if !ok || v.Waiting != 1 || v.Requests != 1 {
		t.Fatalf("global queue not observed: %+v", v)
	}
	if !o.sessions.Control(key, v.Version, "switch", "second") {
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
	v, _ = o.sessions.View(key)
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
	r.Bind("session", "old")
	_, finish := r.Begin(context.Background(), "session", "model", nil, nil)
	defer finish()
	v, _ := r.View("session")
	if !r.Control("session", v.Version, "switch", "new") {
		t.Fatal("control")
	}
	v, _ = r.View("session")
	if !r.Commit(&sessionSelection{Key: "session", Version: v.Version}, "new") {
		t.Fatal("commit")
	}
	v, _ = r.View("session")
	r.UnbindMatching("session", "old")
	after, _ := r.View("session")
	if after.UID != "new" || after.Version != v.Version {
		t.Fatal("old request unbound newer affinity")
	}
	r.UnbindMatching("session", "new")
	after, _ = r.View("session")
	if after.UID != "" || after.Version == v.Version {
		t.Fatal("actual failed affinity was not cleared")
	}
}
