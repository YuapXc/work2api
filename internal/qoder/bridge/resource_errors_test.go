package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"work2api/internal/core/provider"

	"work2api/internal/qoder/cosy"
)

func queueErrorFixture() (string, string, string) {
	ticket := `{"isQueued":true,"modelKey":"qfmodel","queueType":"p3","retryAfterSeconds":30,"serviceAvailable":false,"subCode":"RESOURCE_LIMIT"}`
	business := fmt.Sprintf(`{"code":"10605","subCode":"RESOURCE_LIMIT","message":%q}`, ticket)
	return ticket, business, fmt.Sprintf(`{"code":403,"message":%q}`, business)
}

func TestResourceErrorsAcrossCompleteFrames(t *testing.T) {
	ticket, business, outer := queueErrorFixture()
	for _, tc := range []struct {
		name, body, kind string
		status, want     int
	}{
		{"nested_envelope", outer, "upstream_queued", 403, 429},
		{"call_count_queue", `{"code":10605,"subCode":"CALL_COUNT_LIMIT","message":"{\"isQueued\":true,\"retryAfterSeconds\":17}"}`, "upstream_queued", 200, 429},
		{"call_count_only", `{"code":10605,"subCode":"CALL_COUNT_LIMIT"}`, ErrTypeCallCount, 200, 429},
		{"explicit_not_queued", `{"code":10605,"subCode":"CALL_COUNT_LIMIT","isQueued":false,"retryAfterSeconds":17}`, ErrTypeCallCount, 200, 429},
		{"unrelated_queue", `{"code":10605,"error":{"subCode":"RESOURCE_LIMIT"},"details":{"isQueued":true}}`, "resource_limit", 200, 403},

		{"business_string", business, "upstream_queued", 200, 429},
		{"business_number", `{"code":10605,"subCode":"RESOURCE_LIMIT","isQueued":true}`, "upstream_queued", 200, 429},
		{"details_string", fmt.Sprintf(`{"code":"10605","details":%q}`, ticket), "upstream_queued", 200, 429},
		{"details_object", `{"code":10605,"details":{"subCode":"RESOURCE_LIMIT","isQueued":true}}`, "upstream_queued", 200, 429},
		{"parent_resource_child_queue", `{"code":10605,"subCode":"RESOURCE_LIMIT","details":{"isQueued":true}}`, "upstream_queued", 200, 429},
		{"entitlement", `{"code":10605,"subCode":"RESOURCE_LIMIT","serviceAvailable":false}`, "resource_limit", 200, 403},
		{"code_alone", `{"code":10605}`, ErrTypeUpstream, 200, 500},
		{"numeric_unknown", `{"code":115,"message":"failure"}`, ErrTypeUpstream, 200, 500},
		{"workspace", `{"error":{"message":"Workspace allocated quota exceeded"}}`, ErrTypeWorkspaceQuota, 429, 429},
		{"workspace_business", `{"code":429,"details":{"error":{"message":"Workspace allocated quota exceeded"}}}`, ErrTypeWorkspaceQuota, 200, 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"body": tc.body, "statusCodeValue": tc.status})
			d := ExtractDelta(string(raw))
			var e *UpstreamError
			if !errors.As(d.Err, &e) || e.ErrType != tc.kind || ErrorStatus(e) != tc.want {
				t.Fatalf("got %#v, want %s/%d", d.Err, tc.kind, tc.want)
			}
			if IsTransientUpstream(e.Status, e.Detail) {
				t.Fatal("business rejection became a transient retry")
			}
			if e.Detail != tc.body {
				t.Fatal("original structured error was lost")
			}
		})
	}
}

func TestResourceParserBoundsAndNegativeEvidence(t *testing.T) {
	for _, raw := range []string{
		`{"code":10605}`, `{"message":"RESOURCE_LIMIT isQueued"}`,
		`{"message":"The quota exceeded"}`, `{"isQueued":true}`,
		`{"message":"Workspace allocated quota exceeded yesterday"}`,
		`{"message":"` + strings.Repeat("x", 65536) + `","subCode":"RESOURCE_LIMIT"}`,
	} {
		if kind := resourceFailure(raw, 0); kind != "" {
			t.Fatalf("misclassified as %s", kind)
		}
	}
	raw := `{"subCode":"RESOURCE_LIMIT","isQueued":true}`
	for i := 0; i < 5; i++ {
		raw = fmt.Sprintf(`{"details":%s}`, raw)
	}
	if got := resourceFailure(raw, 0); got != "" {
		t.Fatal("depth bound ignored", got)
	}
	for _, status := range []int{401, 402} {
		e := NewUpstreamError(status, `{"error":{"message":"Workspace allocated quota exceeded"}}`)
		if ErrorStatus(e) != status || e.ErrType == ErrTypeWorkspaceQuota {
			t.Fatal("auth/payment status lost", e)
		}
	}
	for _, code := range []any{nil, false, map[string]any{}, []any{}} {
		if businessCode(code) != "" {
			t.Fatal("invalid code accepted")
		}
	}
	for _, code := range []string{`0`, `"0"`, `null`} {
		raw := fmt.Sprintf(`{"body":%q}`, `{"code":`+code+`,"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}`)
		d := ExtractDelta(raw)
		if d.Err != nil || d.Content != "OK" {
			t.Fatal("success frame regressed", d)
		}
	}
}

func TestResourceRejectionsDoNotReplayOrEmitSuccess(t *testing.T) {
	_, business, outer := queueErrorFixture()
	for _, tc := range []struct {
		name, body              string
		httpStatus, frameStatus int
		partial                 bool
	}{
		{"http", outer, 403, 0, false}, {"sse_envelope", outer, 200, 403, false}, {"sse_business", business, 200, 200, false},
		{"after_output", business, 200, 200, true},
		{"quota112", `{"code":112,"message":"quota exhausted"}`, 200, 200, false},
		{"quota112-after-output", `{"code":112,"message":"quota exhausted"}`, 200, 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					w.WriteHeader(404)
					return
				}
				hits.Add(1)
				if tc.httpStatus != 200 {
					w.WriteHeader(tc.httpStatus)
					fmt.Fprint(w, tc.body)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.partial {
					frame, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": `{"choices":[{"delta":{"content":"first"}}]}`})
					fmt.Fprintf(w, "data: %s\n\n", frame)
				}
				b, _ := json.Marshal(map[string]any{"statusCodeValue": tc.frameStatus, "body": tc.body})
				fmt.Fprintf(w, "data: %s\n\n", b)
			}))
			defer srv.Close()
			sess, e := cosy.NewSession(cosy.AuthIdentity{Uid: "fixture", UserType: "personal_standard"}, "fixture", "fixture", "fixture")
			if e != nil {
				t.Fatal(e)
			}
			b := &Bridge{sess: sess, client: NewBearerClient(sess), templateBase: map[string]any{}, chatStreamURL: srv.URL}
			emitted := 0
			wantEmitted := 0
			if tc.partial {
				wantEmitted = 1
			}
			err := b.CallQoderWithOpts(context.Background(), "codex", nil, "qfmodel", nil, CallOpts{}, func(Delta) { emitted++ })
			var upstream *UpstreamError
			if !errors.As(err, &upstream) || ErrorStatus(err) != 429 || hits.Load() != 1 || emitted != wantEmitted {
				t.Fatal(err, hits.Load(), emitted)
			}
		})
	}
}

func TestQueueRecoveryAndTermination(t *testing.T) {
	for _, mode := range []string{"ready", "unsupported", "rejected", "invalid", "cancel", "revoke", "diagnostic", "after_output", "budget"} {
		t.Run(mode, func(t *testing.T) {
			var sends, queries atomic.Int32
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					queries.Add(1)
					if r.URL.Path != "/algo/api/v2/service/ask/queue/status" || r.URL.Query().Get("requestSetId") == "" || r.URL.Query().Get("modelKey") != "qfmodel" || r.Header.Get("X-Request-ID") == "" || r.Header.Get("X-Session-ID") == "" {
						t.Error("lost queue identity")
					}
					switch mode {
					case "unsupported":
						w.WriteHeader(404)
					case "rejected":
						w.WriteHeader(403)
						fmt.Fprint(w, `{"message":"permission denied"}`)
					case "invalid":
						fmt.Fprint(w, `{"code":0}`)
					case "cancel":
						cancel()
						<-r.Context().Done()
					case "revoke":
						fmt.Fprint(w, `{"isQueued":false}`)
					default:
						fmt.Fprint(w, `{"data":{"isQueued":false}}`)
					}
					return
				}
				count := sends.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				emit := func(raw string) {
					encoded, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": raw})
					fmt.Fprintf(w, "data: %s\n\n", encoded)
				}
				if count == 1 || mode == "budget" {
					if mode == "after_output" {
						emit(`{"choices":[{"delta":{"content":"first"}}]}`)
					}
					_, business, _ := queueErrorFixture()
					emit(business)
					return
				}
				emit(`{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}`)
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer srv.Close()
			sess, e := cosy.NewSession(cosy.AuthIdentity{Uid: "fixture", UserType: "personal_standard"}, "fixture", "fixture", "fixture")
			if e != nil {
				t.Fatal(e)
			}
			b := &Bridge{sess: sess, client: NewBearerClient(sess), templateBase: map[string]any{}, chatStreamURL: srv.URL}
			var beats, waits atomic.Int32
			call := provider.Invocation{Diagnostic: mode == "diagnostic", Wait: func(ctx context.Context, fn func(context.Context) error) error { waits.Add(1); return fn(ctx) }}
			ctx = provider.WithInvocation(ctx, call)
			ctx = WithQueueDispatchCheck(ctx, func(context.Context) error {
				if mode == "revoke" && queries.Load() > 0 {
					return errors.New("revoked")
				}
				return nil
			})
			ctx = withQueueHeartbeat(ctx, func() { beats.Add(1) })
			var out strings.Builder
			err := b.CallQoderWithOpts(ctx, "codex", nil, "qfmodel", nil, CallOpts{Stream: true}, func(d Delta) { out.WriteString(d.Content) })
			switch mode {
			case "ready":
				if err != nil || out.String() != "OK" || sends.Load() != 2 || queries.Load() != 1 || beats.Load() != 1 || waits.Load() != 1 {
					t.Fatal(err, out.String(), sends.Load(), queries.Load(), beats.Load(), waits.Load())
				}
			case "budget":
				if err == nil || sends.Load() != 3 || queries.Load() != 2 {
					t.Fatal(err, sends.Load(), queries.Load())
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) || sends.Load() != 1 {
					t.Fatal(err, sends.Load())
				}
			case "revoke":
				if err == nil || err.Error() != "revoked" || sends.Load() != 1 {
					t.Fatal(err, sends.Load())
				}
			case "rejected":
				if ErrorStatus(err) != 403 || sends.Load() != 1 {
					t.Fatal(err, sends.Load())
				}
			case "diagnostic", "after_output":
				if err == nil || queries.Load() != 0 || sends.Load() != 1 {
					t.Fatal(err, sends.Load(), queries.Load())
				}
			default:
				if err == nil || sends.Load() != 1 || queries.Load() != 1 {
					t.Fatal(err, sends.Load(), queries.Load())
				}
			}
		})
	}
}

func TestQueueStateProgressAndBounds(t *testing.T) {
	now := time.Now()
	waiting, done := true, false
	state := queueState{Queued: &waiting}
	key := queueStuckKey(state)
	if _, _, stalled := queueProgress(state, key, now.Add(-5*time.Minute), now); !stalled {
		t.Fatal("stalled queue not stopped")
	}
	if _, _, stalled := queueProgress(state, key, now.Add(-4*time.Minute), now); stalled {
		t.Fatal("queue stopped early")
	}
	count := 2.
	state.Count = &count
	if _, _, stalled := queueProgress(state, key, now.Add(-time.Hour), now); stalled {
		t.Fatal("stopped with others ahead")
	}
	state.Count = nil
	state.Queued = &done
	if _, _, stalled := queueProgress(state, queueStuckKey(state), now.Add(-time.Hour), now); stalled {
		t.Fatal("ready queue stopped")
	}

	for _, raw := range []string{`{"data":"{\"isQueued\":false}"}`, `{"result":{"isQueued":false}}`} {
		s, ok := readQueueState([]byte(raw))
		if !ok || s.Queued == nil || *s.Queued {
			t.Fatal(raw, s, ok)
		}
	}
	if _, ok := readQueueState([]byte(`{"messages":[{"isQueued":true}]}`)); ok {
		t.Fatal("parsed user content")
	}
	if _, ok := readQueueState([]byte(strings.Repeat(" ", 65537))); ok {
		t.Fatal("accepted oversized status")
	}
	n := 2.
	s := queueState{Count: &n}
	if queueStuckKey(s) != "" {
		t.Fatal("stopped with others ahead")
	}
	n = 1
	if queueStuckKey(s) == "" {
		t.Fatal("missing stuck detector")
	}
	for _, n := range []float64{-1, 0, .1, 1, 300} {
		s.Retry = &n
		d := queuePause(s)
		if d < 500*time.Millisecond || d > 30*time.Second {
			t.Fatal(d)
		}
	}
}

func TestQueuedRequestsAcrossPublicProtocols(t *testing.T) {
	for _, proto := range []string{"chat", "messages", "responses"} {
		for _, streaming := range []bool{false, true} {
			for _, recover := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%v/recover=%v", proto, streaming, recover), func(t *testing.T) {
					var sends atomic.Int32
					srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.Method == "GET" {
							if recover {
								fmt.Fprint(w, `{"isQueued":false}`)
							} else {
								w.WriteHeader(404)
							}
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						var raw string
						if sends.Add(1) == 1 {
							_, raw, _ = queueErrorFixture()
						} else {
							raw = `{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`
						}
						frame, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": raw})
						fmt.Fprintf(w, "data: %s\n\n", frame)
					}))
					defer srv.Close()
					sess, err := cosy.NewSession(cosy.AuthIdentity{Uid: "fixture", UserType: "personal_standard"}, "fixture", "fixture", "fixture")
					if err != nil {
						t.Fatal(err)
					}
					b := &Bridge{sess: sess, client: NewBearerClient(sess), templateBase: map[string]any{}, chatStreamURL: srv.URL}
					req := map[string]any{"model": "qfmodel", "stream": streaming, "messages": []any{map[string]any{"role": "user", "content": "hi"}}, "input": "hi"}
					w := httptest.NewRecorder()
					switch proto {
					case "chat":
						_, err = b.ServeChat(t.Context(), w, req)
					case "messages":
						_, err = b.ServeClaude(t.Context(), w, req)
					case "responses":
						_, err = b.ServeCodex(t.Context(), w, req)
					}
					text := w.Body.String()
					if recover {
						if err != nil || !strings.Contains(text, "OK") || sends.Load() != 2 {
							t.Fatal(err, text, sends.Load())
						}
					} else {
						if err == nil || sends.Load() != 1 {
							t.Fatal("queue refusal became success", err, text)
						}
					}
					if streaming {
						if !strings.Contains(text, ": queued\n\n") {
							t.Fatal("missing queue heartbeat", text)
						}
					} else {
						if strings.Contains(text, ": queued") {
							t.Fatal("SSE in JSON", text)
						}
						var v any
						if json.Unmarshal(w.Body.Bytes(), &v) != nil {
							t.Fatal("invalid nonstream response", text)
						}
					}
				})
			}
		}
	}
}

func TestQuota112AcrossErrorEnvelopes(t *testing.T) {
	for _, code := range []string{`112`, `"112"`} {
		raw := `{"code":` + code + `,"message":"quota exhausted","pricingUrl":"https://example.invalid/pricing"}`
		wrapper, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": raw})
		d := ExtractDelta(string(wrapper))
		var upstream *UpstreamError
		if !errors.As(d.Err, &upstream) || upstream.ErrType != ErrTypeQuota || ErrorStatus(d.Err) != 429 || isRetryableStreamError(d.Err) {
			t.Fatal(d.Err)
		}
		for _, status := range []int{401, 402} {
			if ErrorStatus(NewUpstreamError(status, raw)) != status {
				t.Fatal(status)
			}
		}
	}
	if resourceFailure(`{"message":"code 112 quota exhausted"}`, 0) != "" {
		t.Fatal("matched free text")
	}
}

func TestQueueExitPreservesReasonAndDeadline(t *testing.T) {
	_, raw, _ := queueErrorFixture()
	initial := NewStreamBusinessError(raw)
	for _, tc := range []struct {
		cause   error
		expired bool
		status  int
		kind    string
	}{
		{errQueueStalled, false, 503, ErrTypeQueueStalled},
		{context.DeadlineExceeded, false, 503, ErrTypeQueueTimeout},
		{context.DeadlineExceeded, true, 504, ErrTypeQueueTimeout},
	} {
		err := queueWaitError(initial, tc.cause, tc.expired)
		var ue *UpstreamError
		if !errors.As(err, &ue) || ErrorStatus(err) != tc.status || ue.ErrType != tc.kind || ue.Detail != raw || !errors.Is(err, tc.cause) || ue.RetryAfter != 30*time.Second {
			t.Fatal(err, ue)
		}
		for _, write := range []func(http.ResponseWriter, error){WriteErr, WriteClaudeErr, WriteCodexErr} {
			w := httptest.NewRecorder()
			write(w, err)
			if w.Code != tc.status || w.Header().Get("Retry-After") != "30" || !strings.Contains(w.Body.String(), "排队") {
				t.Fatal(w.Code, w.Body.String())
			}
		}
	}
	if err := queueWaitError(initial, context.Canceled, true); !errors.Is(err, context.Canceled) || ErrorStatus(err) != 499 {
		t.Fatal(err)
	}
	if ErrorStatus(context.DeadlineExceeded) != 504 {
		t.Fatal("bare deadline became 500")
	}
}

func TestStalledQueueFallsBackOnlyBeforeOutput(t *testing.T) {
	for _, mode := range []string{"fallback", "revoked", "deadline", "cancel", "after_output"} {
		t.Run(mode, func(t *testing.T) {
			previous := queueNow
			defer func() { queueNow = previous }()
			now := time.Now()
			ticks := 0
			queueNow = func() time.Time {
				ticks++
				if ticks > 1 {
					return now.Add(6 * time.Minute)
				}
				return now
			}
			var bad, good, queries, nextCalls atomic.Int32
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			if mode == "deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 100*time.Millisecond)
			}
			defer cancel()
			ticket, business, _ := queueErrorFixture()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "GET" {
					queries.Add(1)
					if mode == "deadline" {
						<-r.Context().Done()
						return
					}
					if mode == "cancel" {
						cancel()
						<-r.Context().Done()
						return
					}
					fmt.Fprint(w, ticket)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				emit := func(raw string) {
					frame, _ := json.Marshal(map[string]any{"statusCodeValue": 200, "body": raw})
					fmt.Fprintf(w, "data: %s\n\n", frame)
				}
				if r.URL.Path == "/good" {
					good.Add(1)
					emit(`{"choices":[{"delta":{"content":"OK"},"finish_reason":"stop"}]}`)
					return
				}
				bad.Add(1)
				if mode == "after_output" {
					emit(`{"choices":[{"delta":{"content":"first"}}]}`)
				}
				emit(business)
			}))
			defer srv.Close()
			sess, e := cosy.NewSession(cosy.AuthIdentity{Uid: "fixture", UserType: "personal_standard"}, "fixture", "fixture", "fixture")
			if e != nil {
				t.Fatal(e)
			}
			b := &Bridge{sess: sess, client: NewBearerClient(sess), templateBase: map[string]any{}, chatStreamURL: srv.URL}
			other := &Bridge{sess: sess, client: NewBearerClient(sess), templateBase: map[string]any{}, chatStreamURL: srv.URL + "/good"}
			ctx = WithAccountFallback(ctx, func(ctx context.Context, model string, err error) (*Bridge, error) {
				nextCalls.Add(1)
				var ue *UpstreamError
				if !errors.As(err, &ue) || ue.ErrType != ErrTypeQueueStalled || ErrorStatus(err) != 503 {
					t.Fatalf("lost queue exit: %v", err)
				}
				if mode == "revoked" {
					return nil, provider.NewAPIError(403, "revoked", "account_not_allowed")
				}
				return other, nil
			})
			var content strings.Builder
			err := b.CallQoderWithOpts(ctx, "codex", nil, "qfmodel", nil, CallOpts{}, func(d Delta) { content.WriteString(d.Content) })
			if bad.Load() != 1 {
				t.Fatal("replayed queued request", bad.Load())
			}
			switch mode {
			case "fallback":
				if err != nil || good.Load() != 1 || nextCalls.Load() != 1 || content.String() != "OK" {
					t.Fatal(err, good.Load(), nextCalls.Load(), content.String())
				}
			case "deadline":
				if !errors.Is(err, context.DeadlineExceeded) || ErrorStatus(err) != 504 || nextCalls.Load() != 0 {
					t.Fatal(err, nextCalls.Load())
				}
			case "cancel":
				if !errors.Is(err, context.Canceled) || ErrorStatus(err) != 499 || nextCalls.Load() != 0 {
					t.Fatal(err, nextCalls.Load())
				}
			case "revoked":
				if err == nil || good.Load() != 0 || nextCalls.Load() != 1 {
					t.Fatal(err, good.Load())
				}
			case "after_output":
				if err == nil || good.Load() != 0 || nextCalls.Load() != 0 || queries.Load() != 0 || content.String() != "first" {
					t.Fatal(err, good.Load(), nextCalls.Load(), content.String())
				}
			}
		})
	}
}
