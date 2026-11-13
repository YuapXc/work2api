package upstream_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"work2api/internal/toolschema"
	"work2api/internal/workbuddy/credentials"
	"work2api/internal/workbuddy/models"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/reasoning"
	"work2api/internal/workbuddy/siterouting"
	"work2api/internal/workbuddy/upstream"
)

func TestBusinessFaultTransportNormalization(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want int
	}{
		{`{"error":{"code":11135,"message":"Image not recognized"}}`, 400},
		{`{"error":{"code":400,"details":"{\"code\":11128}"}}`, 400},
		{`{"error":{"code":"11-128"}}`, 400},
		{`{"errCode":11135}`, 400}, {`{"details":"{\"code\":11135}"}`, 400},
		{`{"code":12153}`, 401}, {`{"code":6004}`, 429}, {`{"code":11115}`, 400}, {`{"code":11153}`, 400},
	} {
		for _, mode := range []string{"http", "json", "sse"} {
			t.Run(mode+tc.raw, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if mode == "http" {
						w.WriteHeader(400)
					}
					if mode == "sse" {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprintf(w, "data: %s\n\n", tc.raw)
					} else {
						io.WriteString(w, tc.raw)
					}
				}))
				defer srv.Close()
				err := upstream.NewClient(srv.Client()).StreamUpstream(context.Background(), nil, map[string]any{"model": "test"}, srv.URL, func(string) error { return nil })
				var remote *upstream.UpstreamError
				if !errors.As(err, &remote) || remote.StatusCode != tc.want {
					t.Fatalf("%v %#v", err, remote)
				}
				wantHTTP := 200
				if mode == "http" {
					wantHTTP = 400
				}
				if remote.HTTPStatus != wantHTTP {
					t.Fatal("lost transport status", remote)
				}
			})
		}
	}
	for _, raw := range []string{`{"message":"code 11128","requestId":"11135"}`, `{"messages":[{"code":11128}]}`, `{"arguments":{"code":11135}}`, strings.Repeat("x", 1<<20+1)} {
		if upstream.ParseBusinessFault([]byte(raw)).Code != "" {
			t.Fatal("parsed unrelated content")
		}
	}
	for _, code := range []string{"11135", "11153"} {
		e := (&upstream.UpstreamError{StatusCode: 403, Raw: []byte(`{"code":` + code + `}`)}).Normalized()
		if e.StatusCode != 400 || e.HTTPStatus != 403 {
			t.Fatal(e)
		}
	}
}

// Explicitly paid, synthetic comparison only: two tool calls and one result
// round trip at most. No user content, credentials, or identifiers are printed.
func TestLiveKimiObjectUnionCompatibility(t *testing.T) {
	if os.Getenv("WORK2API_LIVE_KIMI_SCHEMA") != "1" {
		t.Skip("explicit paid model verification only")
	}
	files := append(credentials.FindAuthFiles(filepath.Join("..", "..", "..", "auths"), ""), credentials.FindAuthFiles("", "")...)
	var mgr *credentials.Manager
	checked := 0
	for _, path := range files {
		candidate := credentials.NewManager(path)
		if siterouting.ProfileSite(candidate.Profile()) != siterouting.Domestic {
			continue
		}
		checked++
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		_, e := candidate.GetChatHeadersContext(ctx)
		cancel()
		if e == nil {
			mgr = candidate
			break
		}
		if checked >= 2 {
			break
		}
	}
	if mgr == nil {
		t.Fatal("live-schema: no usable domestic credential; no model calls sent")
	}
	p := pool.New(map[string]pool.Credential{"live": mgr}, "")
	registry := models.New(p, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	catalog, err := registry.RefreshWithStatus(ctx)
	cancel()
	if err != nil {
		t.Fatal("live-schema: model catalog unavailable; no model calls sent")
	}
	model := ""
	for _, m := range catalog {
		id, _ := m["id"].(string)
		name, _ := m["name"].(string)
		if strings.Contains(strings.ToLower(id+" "+name), "kimi") && (strings.Contains(strings.ToLower(id+" "+name), "3.1") || strings.Contains(id, "k3-1")) {
			model = id
			break
		}
	}
	if model == "" {
		t.Fatal("live-schema: requested Kimi generation absent from current catalog")
	}
	endpoint, _ := siterouting.ChatURLForProfile(mgr.Profile())
	var schema any
	json.Unmarshal([]byte(`{"$defs":{"ok":{"type":"object","properties":{"kind":{"type":"string","enum":["ok"]}},"required":["kind"],"additionalProperties":false},"skip":{"type":"object","properties":{"kind":{"type":"string","enum":["skip"]}},"required":["kind"],"additionalProperties":false}},"oneOf":[{"$ref":"#/$defs/ok"},{"$ref":"#/$defs/skip"}]}`), &schema)
	original := map[string]any{"model": model, "max_tokens": 1024, "messages": []any{map[string]any{"role": "system", "content": "Use the supplied reporting tool. Keep reasoning brief."}, map[string]any{"role": "user", "content": "Call report_status once with kind ok. This is a synthetic compatibility check."}}, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "report_status", "description": "Report a synthetic test result.", "parameters": schema}}}, "tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "report_status"}}}
	original = reasoning.NormalizeToolChoice(upstream.BuildUpstreamBody(original))
	repaired := map[string]any{}
	for k, v := range original {
		repaired[k] = v
	}
	repaired["tools"] = toolschema.Tools(original["tools"])
	run := func(mode string, body map[string]any) (map[string]any, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		headers, e := mgr.GetChatHeadersContext(ctx)
		if e != nil {
			t.Fatal("live-schema: credential preparation failed")
		}
		start := time.Now()
		reply, e := upstream.CollectStream(model, func(yield upstream.LineFunc) error {
			return upstream.Shared().StreamUpstream(ctx, headers, body, endpoint, yield)
		})
		if e != nil {
			var remote *upstream.UpstreamError
			if errors.As(e, &remote) {
				var raw map[string]any
				json.Unmarshal(remote.Raw, &raw)
				t.Logf("live-schema: mode=%s model=%s status=%d code=%v duration=%s", mode, model, remote.StatusCode, raw["code"], time.Since(start))
			} else {
				t.Logf("live-schema: mode=%s transport_failed duration=%s", mode, time.Since(start))
			}
			return nil, e
		}
		usage, _ := reply["usage"].(map[string]any)
		t.Logf("live-schema: mode=%s model=%s status=200 duration=%s prompt_tokens=%v completion_tokens=%v credit=%v", mode, model, time.Since(start), usage["prompt_tokens"], usage["completion_tokens"], usage["credit"])
		return reply, nil
	}
	baseline, baselineErr := run("original", original)
	_ = baseline
	if baselineErr != nil {
		var rejected *upstream.UpstreamError
		if !errors.As(baselineErr, &rejected) || rejected.StatusCode == 401 || rejected.StatusCode == 403 || rejected.StatusCode == 429 {
			t.Fatal("live-schema: account/rate/transport failure; comparison stopped")
		}
	}
	reply, err := run("object-type", repaired)
	if err != nil {
		t.Fatal("live-schema: repaired schema did not complete")
	}
	choices, _ := reply["choices"].([]any)
	if len(choices) == 0 {
		t.Fatal("live-schema: missing choices")
	}
	choice := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	calls, _ := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatal("live-schema: expected exactly one tool call")
	}
	call := calls[0].(map[string]any)
	fn, _ := call["function"].(map[string]any)
	var args map[string]any
	arguments, _ := fn["arguments"].(string)
	if fn["name"] != "report_status" || json.Unmarshal([]byte(arguments), &args) != nil || args["kind"] != "ok" {
		t.Fatal("live-schema: tool contract mismatch")
	}
	follow := map[string]any{}
	for k, v := range repaired {
		follow[k] = v
	}
	follow["tool_choice"] = "none"
	follow["messages"] = append(append([]any{}, repaired["messages"].([]any)...), message, map[string]any{"role": "tool", "tool_call_id": call["id"], "content": "Synthetic check passed. Reply OK briefly."})
	final, err := run("tool-result", follow)
	if err != nil {
		t.Fatal("live-schema: tool-result round trip failed")
	}
	finalChoices, _ := final["choices"].([]any)
	if len(finalChoices) == 0 {
		t.Fatal("live-schema: missing final output")
	}
	last := finalChoices[0].(map[string]any)
	msg, _ := last["message"].(map[string]any)
	if last["finish_reason"] != "stop" || msg["content"] == nil || msg["content"] == "" {
		t.Fatal("live-schema: incomplete final answer")
	}
	t.Logf("live-schema: tool contract and result round trip verified; baseline_success=%t", baselineErr == nil)
}

func TestToolNamesTransportRoundTrip(t *testing.T) {
	for _, name := range []string{"report.status", "报告/状态", strings.Repeat("x", 65), "report_status"} {
		t.Run(name, func(t *testing.T) {
			const arguments = `{"name":"report.status","number":9007199254740993}`
			body := map[string]any{"model": "test", "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": name, "parameters": map[string]any{"type": "object", "default": json.Number("9007199254740993")}}}}, "tool_choice": name, "messages": []any{map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "old", "function": map[string]any{"name": name, "arguments": arguments}}}}, map[string]any{"role": "tool", "tool_call_id": "old", "name": name, "content": "report.status"}}}
			if name == "report.status" {
				body["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
			}
			original, _ := json.Marshal(body)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var got map[string]any
				dec := json.NewDecoder(r.Body)
				dec.UseNumber()
				if err := dec.Decode(&got); err != nil {
					t.Error(err)
					return
				}
				fn := got["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
				alias := fn["name"].(string)
				if alias == "" || len(alias) > 64 || strings.ContainsAny(alias, "./报告状态") {
					t.Error("invalid alias")
				}
				if name == "report_status" && alias != name {
					t.Error("valid name changed")
				}
				history := got["messages"].([]any)
				call := history[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
				wantChoice := any(alias)
				if name == "report.status" {
					wantChoice = map[string]any{"type": "function", "function": map[string]any{"name": alias}}
				}
				if !reflect.DeepEqual(got["tool_choice"], wantChoice) || call["name"] != alias || call["arguments"] != arguments || history[1].(map[string]any)["name"] != alias || history[1].(map[string]any)["content"] != "report.status" {
					t.Error("request contract changed")
				}
				if fn["parameters"].(map[string]any)["default"] != json.Number("9007199254740993") {
					t.Error("schema precision lost")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				parts := []string{alias[:2], alias[2:]}
				if name == "report_status" {
					parts = []string{"", alias}
				}
				for i, part := range parts {
					delta := map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "new", "type": "function", "function": map[string]any{"name": part, "arguments": ""}}}}
					if i == 1 {
						delta["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"] = arguments
					}
					raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta}}})
					fmt.Fprintf(w, "data: %s\n\n", raw)
				}
				fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			reply, err := upstream.CollectStream("test", func(y upstream.LineFunc) error {
				return upstream.NewClient(server.Client()).StreamUpstream(context.Background(), nil, body, server.URL, y)
			})
			if err != nil {
				t.Fatal(err)
			}
			fn := reply["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
			if fn["name"] != name || fn["arguments"] != arguments {
				t.Fatal("response contract lost", fn)
			}
			after, _ := json.Marshal(body)
			if string(after) != string(original) {
				t.Fatal("input mutated")
			}
		})
	}
}

func TestCollectStreamKeepsInterleavedToolArguments(t *testing.T) {
	var first, second strings.Builder
	reply, err := upstream.CollectStream("test", func(yield upstream.LineFunc) error {
		for i := range 64 {
			for _, index := range []int{1, 0} {
				fragment := fmt.Sprintf("%d:%d,", index, i)
				if i == 0 {
					fragment = `{"text":"` + fragment
				}
				if i == 63 {
					fragment += `"}`
				}
				if index == 0 {
					first.WriteString(fragment)
				} else {
					second.WriteString(fragment)
				}
				line := fmt.Sprintf(`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":%d,"id":"call_%d","function":{"name":"test","arguments":%q}}]}}]}`, index, index, fragment)
				if err := yield(line); err != nil {
					return err
				}
			}
		}
		return yield(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := reply["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatal("expected two tool calls")
	}
	for i, want := range []string{second.String(), first.String()} {
		call := calls[i].(map[string]any)
		if call["id"] != fmt.Sprintf("call_%d", 1-i) || call["function"].(map[string]any)["arguments"] != want {
			t.Fatal("interleaved arguments or call order changed")
		}
	}
}

func TestInvalidToolDeclarationsFailBeforeTransport(t *testing.T) {
	for _, names := range [][]string{{""}, {" "}} {
		tools := []any{}
		for _, name := range names {
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{"name": name}})
		}
		err := upstream.NewClient(&http.Client{}).StreamUpstream(context.Background(), nil, map[string]any{"model": "test", "tools": tools}, "http://127.0.0.1:1", func(string) error { return nil })
		var remote *upstream.UpstreamError
		if !errors.As(err, &remote) || remote.StatusCode != 400 {
			t.Fatalf("expected actionable local 400, got %v", err)
		}
	}
}

func TestRequestGzipPreservesBodyAndIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, identity    string
		large, compressed bool
	}{{"large-cli", "CLI", true, true}, {"small-cli", "CLI", false, false}, {"desktop", "WorkBuddy", true, false}, {"unknown", "", true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			text := "small"
			if tc.large {
				text = strings.Repeat("synthetic text with tool examples and exact whitespace\n", 3000)
			}
			body := map[string]any{"model": "test", "messages": []any{map[string]any{"role": "user", "content": text}}, "stream": true}
			want, _ := json.Marshal(body)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+strings.TrimPrefix(r.URL.Path, "/") {
					t.Error("prepared body reused stale authorization")
				}
				compressed := tc.compressed && r.Header.Get("X-IDE-Type") == "CLI"
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				if (r.Header.Get("Content-Encoding") == "gzip") != compressed {
					t.Error("wrong compression policy")
				}
				if r.ContentLength != int64(len(raw)) {
					t.Error("incorrect content length")
				}
				if compressed {
					if len(raw) >= len(want)/2 {
						t.Error("no meaningful size reduction")
					}
					reader, e := gzip.NewReader(bytes.NewReader(raw))
					if e != nil {
						t.Error(e)
						return
					}
					raw, err = io.ReadAll(reader)
					reader.Close()
					if err != nil {
						t.Error(err)
					}
				}
				if !bytes.Equal(raw, want) {
					t.Error("request content changed")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"OK\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			prepared, err := upstream.PrepareStreamBody(body)
			if err != nil {
				t.Fatal(err)
			}
			client := upstream.NewClient(server.Client())
			for _, send := range []struct{ token, identity string }{{"old", tc.identity}, {"renewed", tc.identity}, {"desktop", "WorkBuddy"}} {
				headers := map[string]string{"X-IDE-Type": send.identity, "Authorization": "Bearer " + send.token}
				if err := client.StreamPrepared(t.Context(), headers, prepared, server.URL+"/"+send.token, func(string) error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestAliasedToolJSONResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		alias := body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["name"]
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": alias, "arguments": `{"name":"unchanged.value"}`}}}}, "finish_reason": "tool_calls"}}})
	}))
	defer server.Close()
	body := map[string]any{"model": "test", "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "read.file", "parameters": map[string]any{"type": "object"}}}}}
	reply, err := upstream.CollectStream("test", func(y upstream.LineFunc) error {
		return upstream.NewClient(server.Client()).StreamUpstream(context.Background(), nil, body, server.URL, y)
	})
	if err != nil {
		t.Fatal(err)
	}
	fn := reply["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "read.file" || fn["arguments"] != `{"name":"unchanged.value"}` {
		t.Fatal("JSON tool contract changed")
	}
}

func TestCanceledLargeRequestDoesNotDispatch(t *testing.T) {
	called := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called <- struct{}{} }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	body := map[string]any{"model": "test", "messages": []any{map[string]any{"role": "user", "content": strings.Repeat("cancel this synthetic request ", 5000)}}}
	err := upstream.NewClient(server.Client()).StreamUpstream(ctx, map[string]string{"X-IDE-Type": "CLI"}, body, server.URL, func(string) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
	select {
	case <-called:
		t.Fatal("canceled request reached transport")
	default:
	}
}

func TestCollectStreamSeparatesIDsReusingIndex(t *testing.T) {
	reply, err := upstream.CollectStream("test", func(yield upstream.LineFunc) error {
		for _, line := range []string{
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"first","arguments":"{\"key\":"}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"A\"}"}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"b","function":{"name":"second","arguments":"{\"key\":\"B\"}"}}]}}]}`,
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		} {
			if err := yield(line); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := reply["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["tool_calls"].([]any)
	if len(calls) != 2 {
		t.Fatalf("calls=%v", calls)
	}
	for i, call := range calls {
		v := call.(map[string]any)
		var args map[string]string
		if err := json.Unmarshal([]byte(v["function"].(map[string]any)["arguments"].(string)), &args); err != nil || args["key"] != []string{"A", "B"}[i] {
			t.Fatalf("corrupt tool call: %v %v", v, err)
		}
	}
}

func TestAlternateCacheUsageReachesStreamingAndCollectedClients(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`+"\n\n"+`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":2,"num_cached_tokens":12,"prompt_tokens_details":{"cached_tokens":0}}}`+"\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	seen := false
	result, err := upstream.CollectStream("test", func(y upstream.LineFunc) error {
		return upstream.NewClient(server.Client()).StreamUpstream(context.Background(), nil, map[string]any{"model": "test"}, server.URL, func(line string) error {
			if strings.Contains(line, `"cached_tokens":12`) {
				seen = true
			}
			return y(line)
		})
	})
	if err != nil || !seen {
		t.Fatal("stream alias not projected", err)
	}
	usage, _ := result["usage"].(map[string]any)
	details, _ := usage["prompt_tokens_details"].(map[string]any)
	if fmt.Sprint(details["cached_tokens"]) != "12" {
		t.Fatal("collected cache lost", usage)
	}
}
