package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"work2api/internal/config"
	"work2api/internal/core/protocol"
	"work2api/internal/core/provider"
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

func TestModelsETagRevalidatesKeyAndScope(t *testing.T) {
	o, _ := newDNSFailoverOrch(t, &failingUpstream{})
	o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: sessionCostCatalog{}})
	o.wb.Catalog.Refresh()
	id, err := o.db.CreateApp("etag", o.hashKey("etag-key"), "", "", "", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	s := NewServer(o)
	ask := func(tag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.Header.Set("Authorization", "Bearer etag-key")
		r.Header.Set("If-None-Match", tag)
		w := httptest.NewRecorder()
		s.handleModels(w, r)
		return w
	}
	first := ask("")
	tag := first.Header().Get("ETag")
	if first.Code != 200 || tag == "" || first.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatal(first.Code, first.Header())
	}
	unchanged := ask(`"other", W/` + tag)
	if unchanged.Code != 304 || unchanged.Body.Len() != 0 || unchanged.Header().Get("Content-Length") != "" {
		t.Fatal("not a proper 304", unchanged.Code, unchanged.Body.String())
	}
	if err = o.db.SetAppModels(id, `["nonexistent"]`); err != nil {
		t.Fatal(err)
	}
	changed := ask(tag)
	if changed.Code != 200 || changed.Header().Get("ETag") == tag || strings.Contains(changed.Body.String(), "test-model") {
		t.Fatal("stale scope reused", changed.Code, changed.Body.String())
	}
	if _, err = o.db.DeleteApp(id); err != nil {
		t.Fatal(err)
	}
	if deleted := ask("*"); deleted.Code == 304 || deleted.Code == 200 {
		t.Fatal("deleted key bypassed auth", deleted.Code)
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

func TestWorkBuddyNativeMessagesAcrossPublicProtocols(t *testing.T) {
	for _, kind := range []string{"chat", "anthropic", "responses"} {
		for _, stream := range []bool{false, true} {
			for _, sourceStream := range []bool{false, true} {
				t.Run(kind+fmt.Sprint(stream)+"/upstream-stream="+fmt.Sprint(sourceStream), func(t *testing.T) {
					respond := func(req *http.Request, raw string) *http.Response {
						contentType := "application/json"
						if sourceStream {
							var message map[string]any
							json.Unmarshal([]byte(raw), &message)
							start := map[string]any{}
							for k, v := range message {
								start[k] = v
							}
							start["content"] = []any{}
							start["stop_reason"] = nil
							var wire strings.Builder
							emit := func(event map[string]any) {
								encoded, _ := json.Marshal(event)
								wire.WriteString("data: " + string(encoded) + "\n\n")
							}
							emit(map[string]any{"type": "message_start", "message": start})
							for i, raw := range message["content"].([]any) {
								block := raw.(map[string]any)
								if block["type"] == "thinking" {
									emit(map[string]any{"type": "content_block_start", "index": i, "content_block": map[string]any{"type": "thinking", "thinking": ""}})
									if text := block["thinking"].(string); text != "" {
										emit(map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "thinking_delta", "thinking": text}})
									}
									signature := block["signature"].(string)
									for _, part := range []string{signature[:len(signature)/2], signature[len(signature)/2:]} {
										emit(map[string]any{"type": "content_block_delta", "index": i, "delta": map[string]any{"type": "signature_delta", "signature": part}})
									}
								} else {
									emit(map[string]any{"type": "content_block_start", "index": i, "content_block": block})
								}
								emit(map[string]any{"type": "content_block_stop", "index": i})
							}
							emit(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": message["stop_reason"]}, "usage": message["usage"]})
							emit(map[string]any{"type": "message_stop"})
							raw = wire.String()
							contentType = "text/event-stream"
						}
						return &http.Response{Request: req, StatusCode: 200, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(raw))}
					}
					o, _ := newDNSFailoverOrch(t, &failingUpstream{})
					o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: mediaAppTransport(func(r *http.Request) (*http.Response, error) {
						return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"models":[{"id":"claude-test","credits":1}],"agents":[{"name":"cli","models":["claude-test"]}]}}`))}, nil
					})})
					o.wb.Catalog.Refresh()
					calls := 0
					o.wb.UpstreamClient = upstream.NewClient(&http.Client{Transport: mediaAppTransport(func(r *http.Request) (*http.Response, error) {
						calls++
						if r.URL.Path != "/v1/messages" {
							t.Error("native endpoint not selected")
						}
						var sent map[string]any
						if err := json.NewDecoder(r.Body).Decode(&sent); err != nil {
							t.Fatal(err)
						}
						if kind == "anthropic" {
							if sent["output_config"].(map[string]any)["effort"] != "high" {
								t.Error("effort lost")
							}
						}
						if kind == "responses" && calls == 2 {
							raw, _ := json.Marshal(sent)
							for _, token := range []string{"opaque-signature", "empty-signature", "redacted-native", "tool-result", "lookup"} {
								if !strings.Contains(string(raw), token) {
									t.Errorf("native Responses tool loop lost %s: %s", token, raw)
								}
							}
							if strings.Contains(string(raw), "work2api-anthropic1:") {
								t.Fatal("transport envelope leaked to Claude")
							}
						}
						if kind == "responses" && calls == 1 {
							return respond(r, `{"id":"msg_native","type":"message","role":"assistant","model":"claude-test","content":[{"type":"thinking","thinking":"reason","signature":"opaque-signature"},{"type":"thinking","thinking":"","signature":"empty-signature"},{"type":"redacted_thinking","data":"redacted-native"},{"type":"text","text":"OK"},{"type":"tool_use","id":"call1","name":"lookup","input":{"q":"business"}}],"stop_reason":"tool_use","usage":{"input_tokens":7,"output_tokens":5}}`), nil
						}
						return respond(r, `{"id":"msg_native","type":"message","role":"assistant","model":"claude-test","content":[{"type":"thinking","thinking":"reason","signature":"opaque-signature"},{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":5}}`), nil
					})})
					if _, err := o.db.CreateApp("native", o.hashKey("native-key"), "", "", "", "", 0); err != nil {
						t.Fatal(err)
					}
					s := NewServer(o)
					body := map[string]any{"model": "claude-test", "stream": stream, "max_tokens": 256, "messages": []any{map[string]any{"role": "user", "content": "hello"}}}
					path := "/v1/chat/completions"
					if kind == "anthropic" {
						path = "/v1/messages"
						body["thinking"] = map[string]any{"type": "adaptive"}
						body["output_config"] = map[string]any{"effort": "high"}
					}
					if kind == "responses" {
						path = "/v1/responses"
						delete(body, "messages")
						body["input"] = "hello"
					}
					raw, _ := json.Marshal(body)
					r := httptest.NewRequest("POST", path, strings.NewReader(string(raw)))
					r.Header.Set("Authorization", "Bearer native-key")
					w := httptest.NewRecorder()
					handler := http.HandlerFunc(s.handleChat)
					if kind != "chat" {
						handler = func(w http.ResponseWriter, r *http.Request) { s.handleConverted(w, r, kind) }
					}
					s.requestGuard(handler).ServeHTTP(w, r)
					if w.Code != 200 || calls != 1 || !strings.Contains(w.Body.String(), "OK") || strings.Contains(w.Body.String(), upstream.NativeEvent) {
						t.Fatal(w.Code, calls, w.Body.String())
					}
					if kind == "anthropic" {
						raw := w.Body.Bytes()
						if stream {
							var err error
							raw, err = protocol.CollapseStream(strings.NewReader(w.Body.String()), protocol.Anthropic, "claude-test")
							if err != nil {
								t.Fatal(err)
							}
						}
						if !strings.Contains(string(raw), "opaque-signature") {
							t.Fatal("native signature lost", string(raw))
						}
					}
					if kind == "responses" {
						var response map[string]any
						if stream {
							for _, line := range strings.Split(w.Body.String(), "\n") {
								if !strings.HasPrefix(line, "data:") {
									continue
								}
								var event map[string]any
								if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) == nil && event["type"] == "response.completed" {
									response, _ = event["response"].(map[string]any)
								}
							}
						} else if json.Unmarshal(w.Body.Bytes(), &response) != nil {
							t.Fatal("invalid Responses body")
						}
						output, _ := response["output"].([]any)
						if len(output) != 5 {
							t.Fatal("native block boundaries lost", response)
						}
						for i := 0; i < 3; i++ {
							item, _ := output[i].(map[string]any)
							block, ours := protocol.OpenAnthropicReasoning(item["encrypted_content"].(string))
							if !ours || block == nil {
								t.Fatal("missing signed envelope", item)
							}
						}
						next := append([]any{map[string]any{"type": "message", "role": "user", "content": "hello"}}, output...)
						next = append(next, map[string]any{"type": "function_call_output", "call_id": "call1", "output": "tool-result"})
						body["input"] = next
						raw, _ := json.Marshal(body)
						req := httptest.NewRequest("POST", path, strings.NewReader(string(raw)))
						req.Header.Set("Authorization", "Bearer native-key")
						follow := httptest.NewRecorder()
						s.requestGuard(handler).ServeHTTP(follow, req)
						if follow.Code != 200 || calls != 2 {
							t.Fatal("native Responses round trip failed", follow.Code, calls, follow.Body.String())
						}
					}
				})
			}
		}
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
	until := nowSec() + 3600
	if err := o.db.SetModelCooldown("free", "test-model", until, "该模型今日已达上限"); err != nil {
		t.Fatal(err)
	}
	o.wb.AttachModelAccounts(rows)
	foundCooldown := false
	for _, row := range rows {
		for _, account := range row["accounts"].([]map[string]any) {
			if account["uid"] != "free" || row["id"] != "test-model" {
				continue
			}
			foundCooldown = true
			if account["healthy"] != false || account["model_cooldown"] != true || account["model_cooldown_until"] != until || account["model_cooldown_reason"] != "该模型今日已达上限" {
				t.Fatal("model cooldown details lost", account)
			}
		}
	}
	if !foundCooldown {
		t.Fatal("model account missing")
	}
	if err := o.db.SetModelCooldown("free", "test-model", nowSec()-1, "expired"); err != nil {
		t.Fatal(err)
	}
	o.wb.AttachModelAccounts(rows)
	for _, row := range rows {
		for _, account := range row["accounts"].([]map[string]any) {
			if account["uid"] == "free" && row["id"] == "test-model" && (account["model_cooldown"] != false || account["model_cooldown_until"] != float64(0) || account["model_cooldown_reason"] != "") {
				t.Fatal("expired cooldown remained visible", account)
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

func historyTestServer(t *testing.T) *Server {
	t.Helper()
	c := newResponseHistoryCache(256<<20, 6*time.Hour)
	t.Cleanup(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.timer != nil {
			c.timer.Stop()
		}
	})
	return &Server{o: &Orchestrator{cfg: &config.Config{MaxRequestBytes: config.DefaultMaxRequestBytes, MaxJSONDepth: 512, MaxJSONItems: 1000000}}, responseHistory: c}
}
func historyTestRequest(key string) *http.Request {
	r := httptest.NewRequest("POST", "/v1/responses", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	return r
}
func historyTestPrepare(t *testing.T, s *Server, key string, body map[string]any) (http.ResponseWriter, func()) {
	t.Helper()
	w, finish, err := s.prepareResponseHistory(httptest.NewRecorder(), historyTestRequest(key), body, &Principal{AppID: 1, UserID: 2})
	if err != nil {
		t.Fatal(err)
	}
	return w, finish
}
func historyTestComplete(w http.ResponseWriter, id string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"id":%q,"object":"response","status":"completed","output":[{"type":"function_call","call_id":"call_1","name":"shell","arguments":"{}"}]}`, id)
}
func TestResponseHistoryContinuationAndIsolation(t *testing.T) {
	s := historyTestServer(t)
	first := map[string]any{"input": "first", "instructions": "old instructions", "tools": []any{map[string]any{"type": "function", "name": "old-tool"}}}
	w, finish := historyTestPrepare(t, s, "key-a", first)
	historyTestComplete(w, "resp_first")
	// A second request must resolve the ID even before the first handler's defer runs.
	second := map[string]any{"previous_response_id": "resp_first", "input": []any{map[string]any{"type": "function_call_output", "call_id": "call_1", "output": "result"}}, "instructions": "new instructions"}
	w2, finish2 := historyTestPrepare(t, s, "key-a", second)
	got := second["input"].([]any)
	if len(got) != 3 || got[0].(map[string]any)["content"] != "first" || got[1].(map[string]any)["call_id"] != "call_1" || second["previous_response_id"] != nil || second["tools"] != nil || second["instructions"] != "new instructions" {
		t.Fatalf("wrong restore: %#v", second)
	}
	historyTestComplete(w2, "resp_second")
	finish2()
	finish()
	third := map[string]any{"previous_response_id": "resp_second", "input": "third", "store": false}
	w3, finish3 := historyTestPrepare(t, s, "key-a", third)
	historyTestComplete(w3, "resp_unstored")
	finish3()
	if len(third["input"].([]any)) != 5 {
		t.Fatal("lost ancestors")
	}
	for _, tc := range []struct {
		key, id   string
		principal *Principal
	}{
		{"key-b", "resp_first", &Principal{AppID: 1, UserID: 2}},
		{"key-a", "resp_first", &Principal{AppID: 1, UserID: 3}},
		{"key-a", "resp_unstored", &Principal{AppID: 1, UserID: 2}},
	} {
		_, f, err := s.prepareResponseHistory(httptest.NewRecorder(), historyTestRequest(tc.key), map[string]any{"previous_response_id": tc.id, "input": "next"}, tc.principal)
		f()
		if err == nil {
			t.Fatal("scope/store isolation failed")
		}
	}
	branch := map[string]any{"previous_response_id": "resp_first", "input": "branch", "store": false}
	_, f := historyTestPrepare(t, s, "key-a", branch)
	f()
	if len(branch["input"].([]any)) != 3 {
		t.Fatal("branch mutated ancestor")
	}
	if first["input"] != "first" {
		t.Fatal("initial body mutated")
	}
	now := time.Now().Add(7 * time.Hour)
	s.responseHistory.now = func() time.Time { return now }
	_, f, err := s.prepareResponseHistory(httptest.NewRecorder(), historyTestRequest("key-a"), map[string]any{"previous_response_id": "resp_first", "input": "next"}, &Principal{AppID: 1, UserID: 2})
	f()
	if err == nil {
		t.Fatal("expired history accepted")
	}
	s.responseHistory.timer.Stop()
	s.responseHistory.expire()
	if s.responseHistory.used != 0 || len(s.responseHistory.items) != 0 {
		t.Fatal("expired cache not reclaimed")
	}
}
func TestResponseHistorySharedAccountingAndConcurrency(t *testing.T) {
	s := historyTestServer(t)
	c := s.responseHistory
	root := []json.RawMessage{json.RawMessage(`{"role":"user","content":"` + strings.Repeat("x", 8192) + `"}`)}
	if !c.put(responseHistoryKey{"scope", "root"}, nil, root) {
		t.Fatal("put")
	}
	parent := c.acquire(responseHistoryKey{"scope", "root"})
	if !c.put(responseHistoryKey{"scope", "child"}, parent, []json.RawMessage{json.RawMessage(`{"role":"assistant","content":"ok"}`)}) {
		t.Fatal("child")
	}
	c.mu.Lock()
	c.drop(c.items[responseHistoryKey{"scope", "root"}])
	before := c.used
	c.mu.Unlock()
	c.release(parent)
	if c.used >= before || c.used < 8192 {
		t.Fatal("shared ancestor undercounted", before, c.used)
	}
	lease := c.acquire(responseHistoryKey{"scope", "child"})
	c.limit = c.used + 10
	if c.put(responseHistoryKey{"scope", "oversized"}, nil, root) {
		t.Fatal("over budget accepted")
	}
	c.release(lease)
	c.limit = 256 << 20
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			p := c.acquire(responseHistoryKey{"scope", "child"})
			if p == nil {
				return
			}
			defer c.release(p)
			c.put(responseHistoryKey{"scope", fmt.Sprint(i)}, p, []json.RawMessage{json.RawMessage(`{"role":"user","content":"next"}`)})
		}(i)
	}
	wg.Wait()
	c.mu.Lock()
	for c.order.Len() > 0 {
		c.drop(c.order.Back())
	}
	used := c.used
	c.mu.Unlock()
	if used != 0 {
		t.Fatal("leaked accounting", used)
	}
}
func TestResponseHistoryCaptureBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		good       bool
	}{
		{"json", `{"id":"resp_ok","status":"completed","output":[]}`, 200, true},
		{"incomplete", `{"id":"resp_ok","status":"incomplete","output":[]}`, 200, true},
		{"failed", `{"id":"resp_ok","status":"failed","output":[]}`, 200, false},
		{"http error", `{"id":"resp_ok","status":"completed","output":[]}`, 500, false},
		{"truncated", `{"id":"resp_ok","status":"completed","output":[`, 200, false},
		{"primitive output", `{"id":"resp_ok","status":"completed","output":[3]}`, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := historyTestServer(t)
			w, f := historyTestPrepare(t, s, "key", map[string]any{"input": "hello"})
			w.WriteHeader(tc.status)
			_, _ = io.WriteString(w, tc.body)
			capture := w.(*responseHistoryFlushWriter).responseHistoryWriter
			path := ""
			if capture.file != nil {
				path = capture.file.Name()
			}
			f()
			if (len(s.responseHistory.items) > 0) != tc.good {
				t.Fatal("unexpected cache state")
			}
			if path != "" {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("spool not removed", err)
				}
			}
		})
	}
	t.Run("oversize and disk failure preserve output", func(t *testing.T) {
		for _, diskFailure := range []bool{false, true} {
			s := historyTestServer(t)
			w, f := historyTestPrepare(t, s, "key", map[string]any{"input": "hello"})
			capture := w.(*responseHistoryFlushWriter).responseHistoryWriter
			if diskFailure {
				file, err := os.CreateTemp(t.TempDir(), "closed-")
				if err != nil {
					t.Fatal(err)
				}
				_ = file.Close()
				capture.file = file
			} else {
				capture.limit = 1
			}
			historyTestComplete(w, "resp_failed")
			f()
			if len(s.responseHistory.items) != 0 {
				t.Fatal("cached failed capture")
			}
			if !strings.Contains(capture.ResponseWriter.(*httptest.ResponseRecorder).Body.String(), "resp_failed") {
				t.Fatal("capture failure damaged response")
			}
		}
	})
	t.Run("delivered terminal survives client close", func(t *testing.T) {
		s := historyTestServer(t)
		r := historyTestRequest("key")
		ctx, cancel := context.WithCancel(r.Context())
		r = r.WithContext(ctx)
		w, f, err := s.prepareResponseHistory(httptest.NewRecorder(), r, map[string]any{"input": "hello"}, &Principal{})
		if err != nil {
			t.Fatal(err)
		}
		historyTestComplete(w, "resp_delivered")
		cancel()
		f()
		if len(s.responseHistory.items) != 1 {
			t.Fatal("client close lost delivered response")
		}
	})
	t.Run("cancel and downstream error", func(t *testing.T) {
		s := historyTestServer(t)
		r := historyTestRequest("key")
		ctx, cancel := context.WithCancel(r.Context())
		r = r.WithContext(ctx)
		w, f, err := s.prepareResponseHistory(httptest.NewRecorder(), r, map[string]any{"input": "hello"}, &Principal{})
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		historyTestComplete(w, "resp_cancel")
		f()
		if len(s.responseHistory.items) != 0 {
			t.Fatal("cancelled response retained")
		}
		w, f, err = s.prepareResponseHistory(&historyFailWriter{header: make(http.Header)}, historyTestRequest("key"), map[string]any{"input": "hello"}, &Principal{})
		if err != nil {
			t.Fatal(err)
		}
		historyTestComplete(w, "resp_fail")
		f()
		if len(s.responseHistory.items) != 0 {
			t.Fatal("failed downstream retained")
		}
	})
}

type historyFailWriter struct{ header http.Header }

func (w *historyFailWriter) Header() http.Header       { return w.header }
func (w *historyFailWriter) WriteHeader(int)           {}
func (w *historyFailWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestResponseHistorySSEAndRestoredLimits(t *testing.T) {
	stream := "event: response.output_item.done\ndata: {\"output_index\":0,\"item\":{\"type\":\"custom_tool_call\",\"call_id\":\"call_x\",\"name\":\"apply_patch\",\"input\":\"patch\"}}\n\nevent: response.completed\ndata: {\"response\":{\"id\":\"resp_sse\",\"status\":\"completed\"}}\n\n"
	s := historyTestServer(t)
	w, f := historyTestPrepare(t, s, "key", map[string]any{"input": "first"})
	w.Header().Set("Content-Type", "text/event-stream")
	for _, b := range []byte(stream) {
		_, _ = w.Write([]byte{b})
	}
	if len(s.responseHistory.items) != 1 {
		t.Fatal("split SSE terminal not published")
	}
	f()
	body := map[string]any{"previous_response_id": "resp_sse", "input": []any{map[string]any{"type": "custom_tool_call_output", "call_id": "call_x", "output": "done"}}}
	_, release := historyTestPrepare(t, s, "key", body)
	release()
	input := body["input"].([]any)
	if len(input) != 3 || input[1].(map[string]any)["type"] != "custom_tool_call" {
		t.Fatal("custom tool lost")
	}
	for _, bad := range []string{strings.TrimSuffix(stream, "\n\n"), strings.ReplaceAll(stream, "response.completed", "response.failed"), stream + "data: {\"type\":\"error\"}\n\n"} {
		if _, _, ok := readResponseHistoryResult(strings.NewReader(bad), "text/event-stream"); ok {
			t.Fatal("cached invalid stream")
		}
	}
	s.o.cfg.MaxRequestBytes = 5
	_, release, err := s.prepareResponseHistory(httptest.NewRecorder(), historyTestRequest("key"), map[string]any{"previous_response_id": "resp_sse", "input": "next"}, &Principal{AppID: 1, UserID: 2})
	release()
	if _, ok := err.(responseHistoryLimitError); !ok {
		t.Fatal("restored byte limit bypassed", err)
	}
	if responseHistoryWithinLimits(map[string]any{"input": []any{map[string]any{"role": "user"}}}, 1<<20, 2, 512) {
		t.Fatal("restored node limit bypassed")
	}
	if responseHistoryWithinLimits(map[string]any{"input": []any{map[string]any{"role": "user"}}}, 1<<20, 0, 2) {
		t.Fatal("restored depth limit bypassed")
	}
}
func BenchmarkResponseHistoryRestore(b *testing.B) {
	s := &Server{o: &Orchestrator{cfg: &config.Config{MaxRequestBytes: 256 << 20, MaxJSONDepth: 512, MaxJSONItems: 1000000}}, responseHistory: newResponseHistoryCache(256<<20, 6*time.Hour)}
	b.Cleanup(func() {
		c := s.responseHistory
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.timer != nil {
			c.timer.Stop()
		}
	})
	req := historyTestRequest("key")
	body := map[string]any{"input": strings.Repeat("long tool result ", 65536)}
	w, f, err := s.prepareResponseHistory(httptest.NewRecorder(), req, body, &Principal{})
	if err != nil {
		b.Fatal(err)
	}
	historyTestComplete(w, "resp_large")
	f()
	full, _ := json.Marshal(body)
	delta, _ := json.Marshal(map[string]any{"previous_response_id": "resp_large", "input": "continue", "store": false})
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		payload := map[string]any{"previous_response_id": "resp_large", "input": "continue", "store": false}
		_, release, err := s.prepareResponseHistory(httptest.NewRecorder(), req, payload, &Principal{})
		if err != nil {
			b.Fatal(err)
		}
		raw, _ := json.Marshal(payload["input"])
		if !bytes.Contains(raw, []byte("long tool result")) {
			b.Fatal("history missing")
		}
		release()
	}
	b.ReportMetric(float64(len(full)), "full-wire-B")
	b.ReportMetric(float64(len(delta)), "delta-wire-B")
}

func TestResponseHistoryHTTPPermissionsAndWireSavings(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream_%v", stream), func(t *testing.T) {
			o, _ := newDNSFailoverOrch(t, &failingUpstream{})
			o.wb.UpstreamClient = sessionSuccessfulUpstream{}
			o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: sessionCostCatalog{}})
			o.wb.Catalog.Refresh()
			o.cfg.ResponseHistoryBytes = 256 << 20
			o.cfg.ResponseHistoryTTLSeconds = 21600
			app, err := o.db.CreateApp("history", o.hashKey("history-key"), "", "", "", "", 0)
			if err != nil {
				t.Fatal(err)
			}
			s := NewServer(o)
			t.Cleanup(func() {
				c := s.responseHistory
				c.mu.Lock()
				defer c.mu.Unlock()
				if c.timer != nil {
					c.timer.Stop()
				}
			})
			request := func(body map[string]any) *httptest.ResponseRecorder {
				raw, _ := json.Marshal(body)
				req := httptest.NewRequest("POST", "/v1/responses", bytes.NewReader(raw))
				req.Header.Set("Authorization", "Bearer history-key")
				w := httptest.NewRecorder()
				s.requestGuard(http.HandlerFunc(s.handleResponses)).ServeHTTP(w, req)
				return w
			}
			first := map[string]any{"model": "test-model", "input": strings.Repeat("history ", 32768), "stream": stream}
			w := request(first)
			if w.Code != 200 {
				t.Fatal(w.Code, w.Body.String())
			}
			var result map[string]any
			rawResponse := w.Body.Bytes()
			if stream {
				rawResponse, err = protocol.CollapseStream(bytes.NewReader(rawResponse), protocol.Responses, "test-model")
				if err != nil {
					t.Fatal(err)
				}
			}
			if json.Unmarshal(rawResponse, &result) != nil {
				t.Fatal(w.Body.String())
			}
			delta := map[string]any{"model": "test-model", "input": "continue", "previous_response_id": result["id"], "stream": stream}
			second := request(delta)
			if second.Code != 200 || !strings.Contains(second.Body.String(), "hello") {
				t.Fatal(second.Code, second.Body.String())
			}
			fullWire, _ := json.Marshal(first)
			deltaWire, _ := json.Marshal(delta)
			t.Logf("full input request=%d B, continuation request=%d B", len(fullWire), len(deltaWire))
			if len(fullWire) < 1000*len(deltaWire) {
				t.Fatal("unexpected wire savings")
			}
			if err := o.db.SetAppModels(app, `["forbidden"]`); err != nil {
				t.Fatal(err)
			}
			if denied := request(delta); denied.Code == 200 {
				t.Fatal("cached history bypassed current model permission")
			}
			if _, err := o.db.DeleteApp(app); err != nil {
				t.Fatal(err)
			}
			if denied := request(delta); denied.Code != 401 {
				t.Fatal("deleted key bypassed identity", denied.Code)
			}
		})
	}
}

func TestResponseHistoryOriginOnlyCleansForeignRestoredItems(t *testing.T) {
	origin := responseHistoryOrigin{"opencode", "old", "m"}
	replay := &responseHistoryReplay{restored: []responseHistoryOrigin{origin, origin, origin, origin}, total: 5}
	body := []byte(`{"input":[{"role":"assistant","id":"old_message","content":"hello"},{"type":"function_call","id":"old_call","call_id":"c1","name":"read","arguments":"{\"id\":\"business\"}"},{"type":"reasoning","id":"old_ref"},{"type":"reasoning","id":"old_sealed","encrypted_content":"opaque"},{"type":"message","id":"new_input","role":"user","content":"next"}],"tools":[{"type":"function","name":"read"}]}`)
	unchanged, err := replay.prepareBody(provider.AccountRef{Provider: "opencode", LocalID: "old"}, "m", body)
	if err != nil || !bytes.Equal(unchanged, body) {
		t.Fatal("same origin changed", err)
	}
	for _, target := range []responseHistoryOrigin{{"opencode", "new", "m"}, {"opencode", "old", "other"}, {"other", "old", "m"}} {
		result, err := replay.prepareBody(provider.AccountRef{Provider: target.Provider, LocalID: target.Account}, target.Model, body)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err = json.Unmarshal(result, &payload); err != nil {
			t.Fatal(err)
		}
		items := payload["input"].([]any)
		if len(items) != 4 {
			t.Fatal("reference reasoning retained", string(result))
		}
		for _, raw := range items[:3] {
			if raw.(map[string]any)["id"] != nil {
				t.Fatal("foreign ID retained", string(result))
			}
		}
		call := items[1].(map[string]any)
		if call["call_id"] != "c1" || call["arguments"] != `{"id":"business"}` || items[2].(map[string]any)["encrypted_content"] != "opaque" || items[3].(map[string]any)["id"] != "new_input" || payload["tools"] == nil {
			t.Fatal("tool/new input contract changed", string(result))
		}
	}
	replay.total++
	unchanged, err = replay.prepareBody(provider.AccountRef{Provider: "opencode", LocalID: "new"}, "m", body)
	if err != nil || !bytes.Equal(unchanged, body) {
		t.Fatal("guessed mismatched history")
	}
}
