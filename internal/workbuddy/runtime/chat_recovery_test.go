package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"work2api/internal/workbuddy/credentials"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/upstream"
)

func TestNativeMessagesTransportAndSignatureRecovery(t *testing.T) {
	for _, mode := range []string{"stream", "json", "signature", "signature_memory", "signature_wording", "after_output", "truncated", "budget", "auth", "image", "tool"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "native.info")
			raw, _ := json.Marshal(map[string]any{"auth": map[string]any{"accessToken": "fixture", "refreshToken": "r", "domain": "www.codebuddy.cn", "expiresAt": time.Now().Add(time.Hour).UnixMilli()}, "account": map[string]any{"uid": "fixture"}})
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			mgr := credentials.NewManager(path)
			p := pool.New(map[string]pool.Credential{"fixture": mgr}, "")
			original := map[string]any{"model": "claude-test", "max_tokens": 256, "output_config": map[string]any{"effort": "high"}, "messages": []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": "unsigned"}, map[string]any{"type": "thinking", "thinking": "signed", "signature": "opaque"}, map[string]any{"type": "text", "text": "prior"}}}, map[string]any{"role": "user", "content": "next"}}}
			if mode == "image" {
				original["messages"].([]any)[1].(map[string]any)["content"] = []any{map[string]any{"type": "text", "text": "next"}, map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": "https://example.com/expired.png"}}}
			}
			before, _ := json.Marshal(original)
			calls := 0
			transport := maintenanceTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				if req.URL.String() != "https://copilot.tencent.com/v1/messages" || req.Header.Get("anthropic-version") != "2023-06-01" {
					t.Error("wrong native target or version")
				}
				var sent map[string]any
				if err := json.NewDecoder(req.Body).Decode(&sent); err != nil {
					t.Fatal(err)
				}
				if sent["stream"] != true || sent["output_config"].(map[string]any)["effort"] != "high" {
					t.Error("lost native controls")
				}
				blocks := sent["messages"].([]any)[0].(map[string]any)["content"].([]any)
				want := 2
				if (mode == "signature" || mode == "signature_memory" || mode == "signature_wording") && calls > 1 {
					want = 1
				}
				if len(blocks) != want {
					t.Errorf("history blocks %d want %d", len(blocks), want)
				}
				ct, status, payload := "text/event-stream", 200, ""
				events := []string{`{"type":"message_start","message":{"id":"msg_native","type":"message","role":"assistant","model":"claude-test","content":[],"usage":{"input_tokens":7,"output_tokens":0}}}`, `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reason"}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"signed-response"}}`, `{"type":"content_block_stop","index":0}`, `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`, `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"OK"}}`, `{"type":"content_block_stop","index":1}`, `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`, `{"type":"message_stop"}`}
				if mode == "truncated" {
					events = events[:len(events)-1]
				}
				if mode == "after_output" {
					events = append(events[:4], `{"type":"error","error":{"code":11159}}`)
				}
				for _, event := range events {
					payload += "data: " + event + "\n\n"
				}
				if mode == "tool" {
					payload = "data: " + events[0] + "\n\n"
					for _, event := range []string{`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"c1","name":"echo_check","input":{}}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"value\":"}}`, `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"\"ok\"}"}}`, `{"type":"content_block_stop","index":0}`, `{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`, `{"type":"message_stop"}`} {
						payload += "data: " + event + "\n\n"
					}
				}
				if mode == "image" {
					images := sent["messages"].([]any)[1].(map[string]any)["content"].([]any)
					if calls == 1 {
						ct, status, payload = "application/json", 400, `{"code":11135,"error":{"param":"messages[1].content[1]"}}`
					} else if images[1].(map[string]any)["type"] != "text" {
						t.Error("rejected image not removed")
					}
				}
				if (mode == "signature" || mode == "signature_memory" || mode == "signature_wording" || mode == "budget") && calls == 1 {
					ct, status, payload = "application/json", 400, `{"error":{"code":400001,"internal_code":11159}}`
					if mode == "signature_wording" {
						payload = `{"error":{"type":"invalid_request_error","message":"messages.0.content.0: Invalid signature in thinking block. The block is bound to a different conversation."}}`
					}
				}
				if mode == "auth" {
					ct, status, payload = "application/json", 401, `{"code":12153}`
				}
				if mode == "json" {
					ct, payload = "application/json", `{"id":"msg_native","type":"message","role":"assistant","model":"claude-test","content":[{"type":"thinking","thinking":"reason","signature":"signed-response"},{"type":"text","text":"OK"}],"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":5}}`
				}
				return &http.Response{Request: req, StatusCode: status, Header: http.Header{"Content-Type": []string{ct}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
			})
			rt := &Runtime{Managers: map[string]*credentials.Manager{"fixture": mgr}, Pool: p, UpstreamClient: upstream.NewClient(&http.Client{Transport: transport})}
			budget := &recoveryBudget{remaining: 5}
			if mode == "budget" || mode == "auth" {
				budget.remaining = 1
			}
			ctx := context.WithValue(context.WithValue(context.Background(), messagesRequestKey{}, original), recoveryBudgetKey{}, budget)
			var result nativeMessageResult
			started, err := rt.Stream(ctx, p.Get("fixture"), map[string]any{"model": "claude-test"}, func(line string) error {
				chunk := upstream.DecodeFrame(ctx, line)
				if event, ok := chunk[upstream.NativeEvent].(map[string]any); ok {
					result.observe(event)
				}
				return nil
			}, AttemptOptions{})
			failed := mode == "after_output" || mode == "truncated" || mode == "budget" || mode == "auth"
			if (err != nil) != failed {
				t.Fatal(started, err)
			}
			if mode == "after_output" && len(rt.refusedThinking) != 0 {
				t.Fatal("output-started refusal must not learn a replay rule")
			}
			want := 1
			if mode == "signature" || mode == "signature_memory" || mode == "signature_wording" || mode == "image" {
				want = 2
			}
			if calls != want {
				t.Fatal("unexpected replay", calls)
			}
			if mode == "tool" {
				blocks := result.message["content"].([]any)
				b := blocks[0].(map[string]any)
				if b["name"] != "echo_check" || b["input"].(map[string]any)["value"] != "ok" {
					t.Fatal("native tool arguments lost", b)
				}
			} else if !failed {
				blocks := result.message["content"].([]any)
				if len(blocks) != 2 || blocks[0].(map[string]any)["signature"] != "signed-response" || blocks[1].(map[string]any)["text"] != "OK" {
					t.Fatal("lost native content", result.message)
				}
			}
			if mode == "signature_memory" {
				budget.remaining = 5
				if _, err := rt.Stream(ctx, p.Get("fixture"), map[string]any{"model": "claude-test"}, func(string) error { return nil }, AttemptOptions{}); err != nil {
					t.Fatal(err)
				}
				if calls != 3 || budget.remaining != 4 {
					t.Fatal("rejected signature was sent again", calls, budget.remaining)
				}
			}
			after, _ := json.Marshal(original)
			if string(before) != string(after) {
				t.Fatal("mutated original request")
			}
		})
	}
	for _, profile := range []string{"cn-work", "intl-cli", "intl-work", "unknown"} {
		if nativeMessagesEndpoint(profile, "claude-test") != "" {
			t.Fatal("unexpected native site", profile)
		}
	}
	if nativeMessagesEndpoint("cn-cli", "deepseek-v4.1-flash") != "" {
		t.Fatal("non-Claude switched")
	}
}

func TestToolTailRecoveryBoundaries(t *testing.T) {
	for _, mode := range []string{"success", "failure", "started", "budget", "auth", "auth_empty"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "auth.info")
			raw, _ := json.Marshal(map[string]any{"auth": map[string]any{"accessToken": "fixture", "refreshToken": "r", "domain": "www.workbuddy.cn", "expiresAt": time.Now().Add(time.Hour).UnixMilli()}, "account": map[string]any{"uid": "fixture"}})
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			calls, refreshes := 0, 0
			shapes := []int{}
			transport := maintenanceTransport(func(req *http.Request) (*http.Response, error) {
				if strings.Contains(req.URL.Path, "refresh") {
					refreshes++
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`))}, nil
				}
				calls++
				var sent map[string]any
				if err := json.NewDecoder(req.Body).Decode(&sent); err != nil {
					t.Fatal(err)
				}
				shapes = append(shapes, len(sent["messages"].([]any)))
				status := 200
				payload := `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`
				ct := "application/json"
				if calls == 1 || mode == "failure" {
					payload = `{"error":{"code":11153}}`
				}
				if mode == "auth" && calls == 2 {
					status = 401
					payload = `{"code":12153}`
				}
				if mode == "auth_empty" {
					if calls == 2 {
						payload = `{"code":11151}`
					}
					if calls == 3 {
						status = 401
						payload = `{"code":12153}`
					}
				}
				if mode == "started" {
					ct = "text/event-stream"
					payload = "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\ndata: {\"error\":{\"code\":11153}}\n\n"
				}
				return &http.Response{Request: req, StatusCode: status, Header: http.Header{"Content-Type": []string{ct}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
			})
			old := http.DefaultTransport
			var clientTransport http.RoundTripper = transport
			if mode == "auth" || mode == "auth_empty" {
				srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
					resp, e := transport.RoundTrip(req)
					if e != nil {
						t.Error(e)
						w.WriteHeader(500)
						return
					}
					defer resp.Body.Close()
					for k, v := range resp.Header {
						w.Header()[k] = v
					}
					w.WriteHeader(resp.StatusCode)
					io.Copy(w, resp.Body)
				}))
				defer srv.Close()
				tr := srv.Client().Transport.(*http.Transport).Clone()
				tr.TLSClientConfig.ServerName = "example.com"
				tr.DialContext = func(ctx context.Context, n, a string) (net.Conn, error) {
					return (&net.Dialer{}).DialContext(ctx, n, srv.Listener.Addr().String())
				}
				defer tr.CloseIdleConnections()
				http.DefaultTransport = tr
				clientTransport = tr
				defer func() { http.DefaultTransport = old }()
			}
			mgr := credentials.NewManager(path)
			p := pool.New(map[string]pool.Credential{"fixture": mgr}, "")
			rt := &Runtime{Managers: map[string]*credentials.Manager{"fixture": mgr}, Pool: p, UpstreamClient: upstream.NewClient(&http.Client{Transport: clientTransport})}
			body := map[string]any{"model": "m", "messages": []any{map[string]any{"role": "tool", "tool_call_id": "c1", "content": "result"}, map[string]any{"role": "user", "content": "continue"}}}
			if mode == "auth_empty" {
				body["messages"].([]any)[0].(map[string]any)["content"] = `[{"type":"text","text":""}]`
			}
			before, _ := json.Marshal(body)
			budget := &recoveryBudget{remaining: 5}
			if mode == "budget" {
				budget.remaining = 1
			}
			ctx := context.WithValue(context.Background(), recoveryBudgetKey{}, budget)
			started, err := rt.Stream(ctx, p.Get("fixture"), body, func(string) error { return nil }, AttemptOptions{})
			after, _ := json.Marshal(body)
			if string(before) != string(after) {
				t.Fatal("input mutated")
			}
			success := mode == "success" || mode == "auth" || mode == "auth_empty"
			if success {
				if err != nil || !started {
					t.Fatal(started, err)
				}
			} else if err == nil {
				t.Fatal("lost error")
			}
			want := 2
			if mode == "budget" || mode == "started" {
				want = 1
			}
			if mode == "auth" {
				want = 3
			}
			if mode == "auth_empty" {
				want = 4
			}
			if calls != want || budget.remaining != func() int {
				if mode == "budget" {
					return 0
				}
				return 5 - want
			}() {
				t.Fatal("send budget", calls, budget.remaining)
			}
			if (len(rt.toolTailModels) > 0) != success {
				t.Fatal("learned unsuccessful repair")
			}
			if (mode == "auth" || mode == "auth_empty") && refreshes != 1 {
				t.Fatal("authorization recovery lost", refreshes)
			}
			if mode == "success" {
				if _, err = rt.Stream(context.Background(), p.Get("fixture"), body, func(string) error { return nil }, AttemptOptions{}); err != nil {
					t.Fatal(err)
				}
				other := map[string]any{"model": "other", "messages": body["messages"]}
				if _, err = rt.Stream(context.Background(), p.Get("fixture"), other, func(string) error { return nil }, AttemptOptions{}); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(shapes, []int{2, 3, 3, 2}) {
					t.Fatal("learning scope", shapes)
				}
			}
		})
	}
	for _, roles := range [][]string{{"user"}, {"tool"}, {"assistant", "user"}, {"tool", "assistant", "user"}} {
		messages := []any{}
		for _, role := range roles {
			messages = append(messages, map[string]any{"role": role})
		}
		if _, ok := replyAfterToolResults(map[string]any{"messages": messages}); ok {
			t.Fatal("rewrote unrelated arrangement", roles)
		}
	}
}

func TestWrappedBusinessErrorClassification(t *testing.T) {
	for code, want := range map[string]ErrKind{"11128": ErrChannelDenied, "11135": ErrImageInvalid, "11115": ErrPromptTooLong, "11101": ErrBadParams, "12153": ErrSessionDead, "6004": ErrModelRate, "14018": ErrHardCredit, "11102": ErrModelBlocked} {
		raw := []byte(`{"error":{"code":400,"details":"{\"code\":` + code + `}"}}`)
		if got := ClassifyUpstream(400, raw, nil); got != want {
			t.Fatal(code, got, want)
		}
	}
}

func TestConcurrentChat401Recovery(t *testing.T) {
	for _, mode := range []string{"success", "dead", "still401"} {
		t.Run(mode, func(t *testing.T) {
			dead := mode == "dead"
			var refresh, old, fresh atomic.Int32
			allOld := make(chan struct{})
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if strings.Contains(req.URL.Path, "refresh") {
					refresh.Add(1)
					time.Sleep(40 * time.Millisecond)
					w.Header().Set("Content-Type", "application/json")
					if dead {
						io.WriteString(w, `{"code":12153}`)
					} else {
						io.WriteString(w, `{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`)
					}
					return
				}
				if req.Header.Get("Authorization") == "Bearer old" {
					if old.Add(1) == 8 {
						close(allOld)
					}
					<-allOld
					w.WriteHeader(401)
					io.WriteString(w, "<html>Authorization Required</html>")
					return
				}
				fresh.Add(1)
				if mode == "still401" {
					w.WriteHeader(401)
					io.WriteString(w, "private upstream HTML")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
			}))
			defer srv.Close()
			transport := srv.Client().Transport.(*http.Transport).Clone()
			transport.TLSClientConfig.ServerName = "example.com"
			transport.DialContext = func(ctx context.Context, n, a string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, n, srv.Listener.Addr().String())
			}
			previous := http.DefaultTransport
			http.DefaultTransport = transport
			defer func() { http.DefaultTransport = previous; transport.CloseIdleConnections() }()
			path := filepath.Join(t.TempDir(), "auth.info")
			raw, _ := json.Marshal(map[string]any{"auth": map[string]any{"accessToken": "old", "refreshToken": "r", "domain": "www.workbuddy.cn", "expiresAt": time.Now().Add(time.Hour).UnixMilli()}, "account": map[string]any{"uid": "fixture"}})
			os.WriteFile(path, raw, 0600)
			mgr := credentials.NewManager(path)
			p := pool.New(map[string]pool.Credential{"fixture": mgr}, "")
			rt := &Runtime{Managers: map[string]*credentials.Manager{"fixture": mgr}, Pool: p, UpstreamClient: upstream.NewClient(&http.Client{Transport: transport})}
			var wg sync.WaitGroup
			errs := make(chan error, 8)
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_, err := rt.Stream(ctx, p.Get("fixture"), map[string]any{"model": "m", "messages": []any{}}, func(string) error { return nil }, AttemptOptions{})
					errs <- err
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if dead {
					e, ok := err.(*upstream.UpstreamError)
					if !ok || e.StatusCode != 401 || !strings.Contains(string(e.Raw), "重新授权") {
						t.Fatal("waiter lost login guidance", err)
					}
				} else if mode == "still401" {
					e, ok := err.(*upstream.UpstreamError)
					if !ok || e.StatusCode != 401 || !strings.Contains(string(e.Raw), "续期后") {
						t.Fatal(err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			if refresh.Load() != 1 || old.Load() != 8 || (!dead && fresh.Load() != 8) || (dead && fresh.Load() != 0) {
				t.Fatal(refresh.Load(), old.Load(), fresh.Load())
			}
		})
	}
}

func TestChat401LastRecoverySlotDoesNotRefreshOrReplay(t *testing.T) {
	var sends atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		w.WriteHeader(401)
		io.WriteString(w, "private upstream HTML")
	}))
	defer srv.Close()
	transport := srv.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = "example.com"
	transport.DialContext = func(ctx context.Context, n, a string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, n, srv.Listener.Addr().String())
	}
	defer transport.CloseIdleConnections()
	path := filepath.Join(t.TempDir(), "auth.info")
	raw, _ := json.Marshal(map[string]any{"auth": map[string]any{"accessToken": "old", "refreshToken": "r", "domain": "www.workbuddy.cn", "expiresAt": time.Now().Add(time.Hour).UnixMilli()}, "account": map[string]any{"uid": "fixture"}})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	mgr := credentials.NewManager(path)
	p := pool.New(map[string]pool.Credential{"fixture": mgr}, "")
	rt := &Runtime{Managers: map[string]*credentials.Manager{"fixture": mgr}, Pool: p, UpstreamClient: upstream.NewClient(&http.Client{Transport: transport})}
	ctx := context.WithValue(context.Background(), recoveryBudgetKey{}, &recoveryBudget{remaining: 1})
	_, err := rt.Stream(ctx, p.Get("fixture"), map[string]any{"model": "m", "messages": []any{}}, func(string) error { return nil }, AttemptOptions{})
	remote, ok := err.(*upstream.UpstreamError)
	if !ok || remote.StatusCode != 401 || !strings.Contains(string(remote.Raw), "授权") || strings.Contains(string(remote.Raw), "private upstream") || sends.Load() != 1 {
		t.Fatalf("lost final auth error or replayed: sends=%d err=%v", sends.Load(), err)
	}
}

func TestStreamSanitizerPreservesUnchangedFramesAndToolFragments(t *testing.T) {
	normal := `data: { "choices": [{"index":0,"delta":{"content":"text"},"finish_reason":null}],"extension":9007199254740993 }`
	var chunk map[string]any
	json.Unmarshal([]byte(normal[6:]), &chunk)
	role := false
	if out := SanitizeChatChunkWithRole(normal, chunk, &role); out != normal {
		t.Fatal("unchanged frame reencoded", out)
	}
	multiline := "data: {\n\"choices\":[{\"delta\":{\"content\":\"text\"},\"finish_reason\":null}]}"
	if out := SanitizeChatSSE(multiline); strings.ContainsAny(out, "\r\n") || !strings.Contains(out, `"content":"text"`) {
		t.Fatal("multiline JSON leaked into single-line SSE", out)
	}
	first := `data: {"choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call","function":{"name":"lookup","arguments":"{"}}]},"finish_reason":""}]}`
	clean := SanitizeChatSSEWithRole(first, &role)
	var decoded map[string]any
	json.Unmarshal([]byte(clean[6:]), &decoded)
	choice := decoded["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != nil || !role || !strings.Contains(clean, `"name":"lookup"`) {
		t.Fatal("first fragment changed", clean)
	}
	next := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"","arguments":"}"}}],"content":"","reasoning_content":""},"finish_reason":null}]}`
	clean = SanitizeChatSSEWithRole(next, &role)
	if strings.Contains(clean, `"name"`) || !strings.Contains(clean, `"arguments":"}"`) || strings.Contains(clean, "reasoning_content") {
		t.Fatal("argument fragment corrupted", clean)
	}
	if out := SanitizeChatSSE(`data: {"choices":[{"delta":{"content":""},"finish_reason":""}],"usage":{"total_tokens":1}}`); !strings.Contains(out, `"choices":[]`) || !strings.Contains(out, `"total_tokens":1`) {
		t.Fatal("usage-only frame lost", out)
	}
	if out := SanitizeChatSSE(`data: {"choices":[{"delta":{"role":"assistant"},"finish_reason":null}]}`); !strings.Contains(out, "assistant") {
		t.Fatal("first role lost")
	}
}
func BenchmarkStreamSanitizerPassThrough(b *testing.B) {
	line := `data: {"id":"fixture","choices":[{"index":0,"delta":{"content":"` + strings.Repeat("chunk", 40) + `"},"finish_reason":null}]}`
	var chunk map[string]any
	json.Unmarshal([]byte(line[6:]), &chunk)
	role := true
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		SanitizeChatChunkWithRole(line, chunk, &role)
	}
}
func TestEmptyTextRecoveryIsRequestLocalAndBudgeted(t *testing.T) {
	for _, mode := range []string{"success", "failure", "started", "budget", "other", "plain", "shape"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "auth.info")
			raw, _ := json.Marshal(map[string]any{"auth": map[string]any{"accessToken": "fixture", "domain": "www.workbuddy.cn", "expiresAt": time.Now().Add(time.Hour).UnixMilli()}, "account": map[string]any{"uid": "fixture"}})
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			tr := maintenanceTransport(func(req *http.Request) (*http.Response, error) {
				calls++
				var sent map[string]any
				if err := json.NewDecoder(req.Body).Decode(&sent); err != nil {
					t.Fatal(err)
				}
				messages := sent["messages"].([]any)
				text := messages[0].(map[string]any)["content"].(string)
				if calls == 1 && text != `[{"type":"text","text":""}]` && mode != "plain" {
					t.Fatal("premature string rewrite", text)
				}
				status, ct, payload := 200, "application/json", `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`
				if calls == 1 || mode == "failure" {
					status = 400
					payload = `{"code":11151,"msg":"empty text"}`
				}
				if mode == "other" {
					payload = `{"code":11101}`
				}
				if mode == "started" {
					status = 200
					ct = "text/event-stream"
					payload = "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\ndata: {\"error\":{\"code\":11151}}\n\n"
				}
				if calls == 2 && (mode == "success" || mode == "shape") && !strings.Contains(text, `"text":" "`) {
					t.Fatal("missing repair", text)
				}
				if mode == "shape" && calls == 2 {
					status = 400
					payload = `{"code":11153}`
				}
				if mode == "shape" && calls == 3 && len(messages) != 3 {
					t.Fatal("shape repair lost")
				}
				return &http.Response{Request: req, StatusCode: status, Header: http.Header{"Content-Type": []string{ct}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
			})
			mgr := credentials.NewManager(path)
			p := pool.New(map[string]pool.Credential{"fixture": mgr}, "")
			rt := &Runtime{Managers: map[string]*credentials.Manager{"fixture": mgr}, Pool: p, UpstreamClient: upstream.NewClient(&http.Client{Transport: tr})}
			text := `[{"type":"text","text":""}]`
			if mode == "plain" {
				text = "ordinary result"
			}
			body := map[string]any{"model": "m", "messages": []any{map[string]any{"role": "tool", "tool_call_id": "c1", "content": text}, map[string]any{"role": "user", "content": "continue"}}}
			before, _ := json.Marshal(body)
			slots := 5
			if mode == "budget" {
				slots = 1
			}
			budget := &recoveryBudget{remaining: slots}
			ctx := context.WithValue(context.Background(), recoveryBudgetKey{}, budget)
			_, err := rt.Stream(ctx, p.Get("fixture"), body, func(string) error { return nil }, AttemptOptions{})
			want := 1
			if mode == "success" || mode == "failure" {
				want = 2
			}
			if mode == "shape" {
				want = 3
			}
			if calls != want || budget.remaining != slots-want || (err == nil) != (mode == "success" || mode == "shape") {
				t.Fatal(mode, calls, budget.remaining, err)
			}
			after, _ := json.Marshal(body)
			if string(after) != string(before) {
				t.Fatal("source mutated")
			}
			if kind := ClassifyUpstream(502, []byte(`{"error":{"code":11151}}`), nil); kind != ErrEmptyText || !ActionFor(kind, nil, nil, 0).FailFast {
				t.Fatal("11151 causes futile account rotation", kind)
			}
		})
	}
}

func TestKimiReferenceCompatibilityReachesPreparedRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.info")
	raw, _ := json.Marshal(map[string]any{"auth": map[string]any{"accessToken": "fixture", "domain": "www.workbuddy.cn", "expiresAt": time.Now().Add(time.Hour).UnixMilli()}, "account": map[string]any{"uid": "fixture"}})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	mgr := credentials.NewManager(path)
	p := pool.New(map[string]pool.Credential{"fixture": mgr}, "")
	var seen []string
	tr := maintenanceTransport(func(req *http.Request) (*http.Response, error) {
		var sent map[string]any
		if err := json.NewDecoder(req.Body).Decode(&sent); err != nil {
			t.Fatal(err)
		}
		fn := sent["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
		params := fn["parameters"].(map[string]any)
		model := sent["model"].(string)
		encoded, _ := json.Marshal(params)
		if (params["$defs"] == nil) != (model == "kimi-k3.1-pro") || !strings.Contains(string(encoded), `"minLength":1`) || !strings.Contains(string(encoded), `"required":["value"]`) {
			t.Fatal("schema scope or constraint changed", model, params)
		}
		seen = append(seen, model)
		return &http.Response{Request: req, StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`))}, nil
	})
	rt := &Runtime{Managers: map[string]*credentials.Manager{"fixture": mgr}, Pool: p, UpstreamClient: upstream.NewClient(&http.Client{Transport: tr})}
	for _, model := range []string{"kimi-k3.1-pro", "glm-5.3-flash"} {
		var body map[string]any
		json.Unmarshal([]byte(`{"messages":[{"role":"user","content":"synthetic"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","$defs":{"value":{"type":"string","minLength":1}},"required":["value"],"properties":{"value":{"$ref":"#/$defs/value"}}}}}]}`), &body)
		body["model"] = model
		before, _ := json.Marshal(body)
		if _, err := rt.Stream(context.Background(), p.Get("fixture"), body, func(string) error { return nil }, AttemptOptions{}); err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(body)
		if string(before) != string(after) {
			t.Fatal("source request mutated")
		}
	}
	if len(seen) != 2 {
		t.Fatal("unexpected sends", seen)
	}
}

func TestNativeThinkingFilteringAndScopedRecovery(t *testing.T) {
	thinking := func(signature any) map[string]any {
		return map[string]any{"type": "thinking", "thinking": "analysis", "signature": signature}
	}
	foreign := thinking("12345678-1234-1234-1234-123456789abc")
	text := map[string]any{"type": "text", "text": "keep"}
	call := map[string]any{"type": "tool_use", "id": "c1", "name": "read", "input": map[string]any{"signature": "business"}}
	before := thinking("before")
	target := thinking("target")
	redacted := map[string]any{"type": "redacted_thinking", "data": "opaque"}
	body := map[string]any{"model": "claude-test", "messages": []any{
		map[string]any{"role": "assistant", "content": []any{thinking(nil), thinking("  "), foreign}},
		map[string]any{"role": "assistant", "content": []any{before, target, redacted, call, text}},
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "c1", "content": "result"}}},
	}}
	original, _ := json.Marshal(body)
	clean := withoutMessagesThinking(body, false)
	messages := clean["messages"].([]any)
	if len(messages) != 2 || len(messages[0].(map[string]any)["content"].([]any)) != 5 {
		t.Fatal("foreign-only message was not removed or native blocks lost", clean)
	}
	if reflect.ValueOf(withoutMessagesThinking(clean, false)).Pointer() != reflect.ValueOf(clean).Pointer() {
		t.Fatal("filter not idempotent")
	}
	rt := &Runtime{}
	err := &upstream.UpstreamError{StatusCode: 400, Raw: []byte(`{"error":{"message":"messages.0.content.1: Invalid signature in thinking block"}}`)}
	fixed, ok := rt.recoverBoundThinking("account-a/model-a", clean, err)
	if !ok {
		t.Fatal("explicit signature refusal not recovered")
	}
	kept := fixed["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(kept) != 3 || kept[0].(map[string]any)["signature"] != "before" || kept[1].(map[string]any)["id"] != "c1" {
		t.Fatal("lost earlier signature or tool contract", fixed)
	}
	learned := rt.withoutRefusedThinking("account-a/model-a", clean)
	if !reflect.DeepEqual(learned, fixed) {
		t.Fatal("refusal not remembered")
	}
	for _, owner := range []string{"account-b/model-a", "account-a/model-b", "new-credentials"} {
		if reflect.ValueOf(rt.withoutRefusedThinking(owner, clean)).Pointer() != reflect.ValueOf(clean).Pointer() {
			t.Fatal("refusal escaped owner", owner)
		}
	}
	for _, e := range []*upstream.UpstreamError{
		{StatusCode: 401, Raw: err.Raw},
		{StatusCode: 400, Raw: []byte(`{"error":{"message":"please include thinking in tool turns"}}`)},
		{StatusCode: 400, Raw: []byte(`{"code":11135}`)},
	} {
		if _, ok := rt.recoverBoundThinking("unrelated", clean, e); ok {
			t.Fatal("unrelated error rewrote thinking")
		}
	}
	missing := &upstream.UpstreamError{StatusCode: 400, Raw: []byte(`{"code":11159,"msg":"messages.99999999999999999999999.content.9"}`)}
	all, ok := rt.recoverBoundThinking("missing-position", clean, missing)
	if !ok || len(all["messages"].([]any)[0].(map[string]any)["content"].([]any)) != 2 {
		t.Fatal("unlocatable refusal did not preserve text/tools", all)
	}
	after, _ := json.Marshal(body)
	if string(original) != string(after) {
		t.Fatal("original history mutated")
	}
}

func TestNativeThinkingRecoveryCacheBound(t *testing.T) {
	rt := &Runtime{refusedThinking: map[thinkingRefusalKey]bool{}}
	for i := 0; i < 1<<14; i++ {
		rt.refusedThinking[thinkingRefusalKey{owner: "old", block: [32]byte{byte(i), byte(i >> 8)}}] = true
	}
	body := map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": "reason", "signature": "new"}}}, map[string]any{"role": "user", "content": "continue"}}}
	out, ok := rt.recoverBoundThinking("new", body, &upstream.UpstreamError{StatusCode: 400, Raw: []byte(`{"code":11159}`)})
	if !ok || len(rt.refusedThinking) != 1 || len(out["messages"].([]any)) != 1 || out["messages"].([]any)[0].(map[string]any)["role"] != "user" {
		t.Fatal("cache did not remain bounded or preserved an empty assistant", len(rt.refusedThinking), out)
	}
}
