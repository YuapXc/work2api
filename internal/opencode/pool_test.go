package opencode

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"work2api/internal/core/protocol"
	"work2api/internal/core/provider"
)

func TestUpstreamCompatibilityAndToolAliases(t *testing.T) {
	for _, proto := range []protocol.Protocol{protocol.Chat, protocol.Responses, protocol.Anthropic} {
		t.Run(string(proto), func(t *testing.T) {
			tool := map[string]any{"name": "read", "description": "client reader", "parameters": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}, "required": []any{"value"}}}
			var definition any = tool
			if proto == protocol.Chat {
				definition = map[string]any{"type": "function", "function": tool}
			} else if proto == protocol.Anthropic {
				tool["input_schema"] = tool["parameters"]
				delete(tool, "parameters")
			} else {
				tool["type"] = "function"
			}
			body := map[string]any{"tools": []any{definition}, "messages": []any{map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"function": map[string]any{"name": "read", "arguments": `{"name":"read"}`}}}}}}
			original, _ := json.Marshal(body)
			lowered, names := aliasClientTools(body, proto, false, false)
			if len(names) != 1 {
				t.Fatal("client tool not aliased")
			}
			alias := ""
			for k := range names {
				alias = k
			}
			typed := lowered["tools"].([]any)[0].(map[string]any)
			if proto == protocol.Chat {
				typed = typed["function"].(map[string]any)
			}
			if typed["name"] != alias || typed["description"] != "client reader" {
				t.Fatal("client definition replaced")
			}
			before, _ := json.Marshal(body)
			if !bytes.Equal(original, before) {
				t.Fatal("request mutated")
			}
			var response map[string]any
			switch proto {
			case protocol.Chat:
				response = map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"function": map[string]any{"name": alias, "arguments": `{"name":"read"}`}}}}}}}
			case protocol.Responses:
				response = map[string]any{"type": "response.output_item.added", "item": map[string]any{"type": "function_call", "name": alias, "arguments": "{}"}}
			case protocol.Anthropic:
				response = map[string]any{"type": "content_block_start", "content_block": map[string]any{"type": "tool_use", "name": alias, "id": "call1", "input": map[string]any{}}}
			}
			encoded, _ := json.Marshal(response)
			w := httptest.NewRecorder()
			w.Header().Set("Content-Type", "text/event-stream")
			writer := &toolAliasWriter{ResponseWriter: w, names: names}
			frame := append(append([]byte("event: test\ndata: "), encoded...), []byte("\n\n")...)
			for _, b := range frame {
				if _, err := writer.Write([]byte{b}); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Finish(); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(w.Body.String(), alias) || !strings.Contains(w.Body.String(), `"name":"read"`) {
				t.Fatal("response alias leaked", w.Body.String())
			}
		})
	}
}

func TestAttemptDeadlineDoesNotCutEstablishedStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"first\"}}]}\n\n")
		w.(http.Flusher).Flush()
		time.Sleep(100 * time.Millisecond)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"last\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL, strings.NewReader("{}"))
	resp, err := doInferenceAttempt(srv.Client(), req, protocol.Chat, 60*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || !bytes.Contains(body, []byte("last")) {
		t.Fatal("committed stream cut", err)
	}
	client := &http.Client{Transport: catalogTestTransport(func(r *http.Request) (*http.Response, error) { <-r.Context().Done(); return nil, r.Context().Err() })}
	start := time.Now()
	_, err = doInferenceAttempt(client, req, protocol.Chat, 20*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatal("dial/write not bounded", err)
	}
}

func TestAnonymousCooldownAndProxyRecovery(t *testing.T) {
	proxy := &proxyTransport{}
	proxy.healthy.Store(true)
	p := newAnonymousPool(true, &transportPool{items: []*proxyTransport{proxy}}, time.Second)
	p.MarkFailure(p.nodes[0], &http.Response{StatusCode: 429, Header: http.Header{"Retry-After": []string{"7200"}}}, nil)
	if time.Until(time.Unix(0, p.nodes[0].cooldownUntil.Load())) > 8*time.Second {
		t.Fatal("anonymous cooldown pinned for hours")
	}
	rt := &Runtime{anonymous: p, zenNodes: &nodePool{}, goNodes: &nodePool{}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	rt.restoreProxy(proxy)
	cursor := p.CursorFor("test")
	if cursor.Next() == nil {
		t.Fatal("recovered anonymous lane still locked")
	}
}

func TestAnonymousEndpointAndIdentity(t *testing.T) {
	for _, tc := range []struct{ zen, anonymous, want string }{
		{"https://opencode.ai/zen", "", "https://opencode.ai/inference"},
		{"https://gateway.example/zen", "", "https://gateway.example/zen"},
		{"https://gateway.example/zen", "https://gateway.example/anonymous", "https://gateway.example/anonymous"},
	} {
		cfg := defaultConfig()
		cfg.Anonymous = true
		cfg.Upstream.Zen, cfg.Upstream.Anonymous = tc.zen, tc.anonymous
		cfg, err := Normalize("", cfg)
		if err != nil || cfg.Upstream.Anonymous != tc.want {
			t.Fatalf("anonymous endpoint: %q %v", cfg.Upstream.Anonymous, err)
		}
	}
	ids := RequestIDs{Session: CanonicalSessionID("conversation"), Project: "scoped-project"}
	req, err := newUpstreamRequest(context.Background(), "https://gateway.example", protocol.Chat, []byte("{}"), ids, "")
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("x-opencode-session-id") != ids.Session || len(req.Header.Get("x-opencode-project")) != 40 || req.Header.Get("x-opencode-request") != "" {
		t.Fatal("unexpected upstream identity headers")
	}
}

func TestLiveMetadataShape(t *testing.T) {
	if os.Getenv("WORK2API_LIVE_QUOTES") != "1" {
		t.Skip("explicit live verification only")
	}
	cfg, err := LoadConfig(filepath.Join("..", "..", "data", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal("local transport config unavailable")
	}
	pool, err := newTransportPool(cfg.RuntimeProxies(), cfg.Performance, 25*time.Second)
	if err != nil {
		t.Fatal("transport unavailable")
	}
	defer func() {
		for _, p := range pool.items {
			p.client.CloseIdleConnections()
		}
	}()
	for _, endpoint := range []string{CapabilitiesURL, modelsDevDefaultURL} {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
		req.Header.Set("User-Agent", userAgent())
		resp, err := pool.items[0].client.Do(req)
		if err != nil {
			cancel()
			t.Fatal("metadata transport failed")
		}
		var providers map[string]map[string]any
		err = json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&providers)
		resp.Body.Close()
		cancel()
		if resp.StatusCode != 200 || err != nil {
			t.Fatalf("metadata HTTP %d", resp.StatusCode)
		}
		for _, id := range []string{"opencode", "opencode-go"} {
			models, _ := providers[id]["models"].(map[string]any)
			count := 0
			for _, raw := range models {
				m, _ := raw.(map[string]any)
				encoded, _ := json.Marshal(m)
				var model capabilityModel
				if err := json.Unmarshal(encoded, &model); err != nil {
					t.Fatal("live metadata schema incompatible", err)
				}
				md := model.metadata()
				extra := map[string]any{}
				(&Runtime{cfg: defaultConfig()}).addDiscoveryMetadata(extra, model.ID, md, Tier(id))
				expected := map[string]bool{}
				for _, option := range model.ReasoningOptions {
					if option.Type == "effort" && model.Reasoning {
						for _, value := range option.Values {
							if value != "" {
								expected[value] = true
							}
						}
					}
				}
				actual, _ := extra["reasoning_efforts"].([]string)
				if len(actual) != len(expected) {
					t.Fatal("live reasoning options lost or invented", model.ID)
				}
				for _, value := range actual {
					if !expected[value] {
						t.Fatal("unexpected live effort", model.ID, value)
					}
				}
				if m["reasoning"] != true {
					continue
				}
				keys := []string{}
				for k := range m {
					keys = append(keys, k)
				}
				if count < 2 {
					t.Logf("source=%s provider=%s reasoning_model=%v keys=%v reasoning_options=%v cost=%v", endpoint, id, m["id"], keys, m["reasoning_options"], m["cost"])
				}
				count++
			}
			t.Logf("validated source=%s provider=%s models=%d reasoning_models=%d", endpoint, id, len(models), count)
		}
	}
}

func TestResponsesMinimumBudgetKeepsInvalidAndLargeValues(t *testing.T) {
	rt := &Runtime{cfg: defaultConfig()}
	for _, n := range []float64{0, 1, 2.5, 15, 16, 1024} {
		input := map[string]any{"model": "test", "input": "hi", "max_output_tokens": n}
		body, _, err := rt.prepareRouteBody(protocol.Responses, Route{Tier: TierZen, Protocol: protocol.Responses}, input, false, TierZen)
		if err != nil {
			t.Fatal(err)
		}
		var actual map[string]any
		if err := json.Unmarshal(body, &actual); err != nil {
			t.Fatal(err)
		}
		want := n
		if n == 1 || n == 15 {
			want = 16
		}
		if actual["max_output_tokens"] != want || input["max_output_tokens"] != n {
			t.Fatalf("budget %v: got %v, want %v; input %v", n, actual["max_output_tokens"], want, input["max_output_tokens"])
		}
	}
}

func TestFragmentedToolNamePreservesArguments(t *testing.T) {
	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")
	writer := &toolAliasWriter{ResponseWriter: w, names: map[string]string{"w2a_example": "read"}}
	for _, name := range []string{"w2a_", "example"} {
		frame := fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"name\":%q,\"arguments\":\"w2a_example\"}}]}}]}\n\n", name)
		if _, err := writer.Write([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Finish(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.Body.String(), `"name":"read"`) || strings.Count(w.Body.String(), `"arguments":`) != 1 || !strings.Contains(w.Body.String(), `"arguments":"w2a_examplew2a_example"`) {
		t.Fatal("fragmented name or opaque arguments corrupted", w.Body.String())
	}
}

func TestCompatibilityToolScopePreservesInputAndRejectsPlaceholders(t *testing.T) {
	for _, proto := range []protocol.Protocol{protocol.Chat, protocol.Anthropic, protocol.Responses} {
		input := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "read original text"}}, "system": "original system", "instructions": "original instructions", "tool_choice": "auto"}
		before, _ := json.Marshal(input)
		out := describeClientTools(input, proto, map[string]string{"alias_read": "read"}, true)
		after, _ := json.Marshal(input)
		if !bytes.Equal(before, after) || out["tool_choice"] != "auto" {
			t.Fatal("client input or automatic choice modified")
		}
		var response map[string]any
		switch proto {
		case protocol.Chat:
			response = map[string]any{"choices": []any{map[string]any{"message": map[string]any{"tool_calls": []any{map[string]any{"function": map[string]any{"name": "read", "arguments": "{}"}}}}}}}
		case protocol.Anthropic:
			response = map[string]any{"type": "content_block_start", "content_block": map[string]any{"type": "tool_use", "name": "read"}}
		case protocol.Responses:
			response = map[string]any{"type": "response.output_item.added", "item": map[string]any{"type": "function_call", "name": "read"}}
		}
		raw, _ := json.Marshal(response)
		w := httptest.NewRecorder()
		writer := &toolAliasWriter{ResponseWriter: w, names: map[string]string{"alias_read": "read"}, blocked: map[string]bool{"read": true}}
		if _, err := writer.Write(raw); err == nil || w.Body.Len() != 0 || writer.Finish() == nil {
			t.Fatal("non-executable tool reached client", proto)
		}
	}
	w := httptest.NewRecorder()
	w.Header().Set("Content-Type", "text/event-stream")
	writer := &toolAliasWriter{ResponseWriter: w, blocked: map[string]bool{"read": true}}
	for i, fragment := range []string{"re", "ad"} {
		frame := fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"name\":%q}}]}}]}\n\n", fragment)
		_, err := writer.Write([]byte(frame))
		if (i == 0 && err != nil) || (i == 1 && err == nil) {
			t.Fatal("fragmented placeholder escaped guard", err)
		}
	}
	if strings.Contains(w.Body.String(), `"name":"re"`) {
		t.Fatal("partial placeholder name leaked")
	}
}

// Run explicitly after user approval; never reads keys into test output and
// never dispatches through paid lanes. No server restart is required.
func TestLiveOpenCodeCompatibility(t *testing.T) {
	if os.Getenv("WORK2API_LIVE_OPENCODE") != "1" {
		t.Skip("explicit live invocation only")
	}
	cfg, err := LoadConfig(filepath.Join("..", "..", "data", "opencode", "opencode.json"))
	if err != nil {
		t.Fatal("local OpenCode config unavailable")
	}
	cfg.ZenKeys, cfg.GoKeys = nil, nil
	cfg.Anonymous = true
	cfg.Retry.MaxAttempts = 1
	transports, err := newTransportPool(cfg.RuntimeProxies(), cfg.Performance, 45*time.Second)
	if err != nil {
		t.Fatal("proxy transport unavailable")
	}
	defer func() {
		for _, proxy := range transports.items {
			proxy.client.CloseIdleConnections()
		}
	}()
	rt := &Runtime{cfg: cfg, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), transports: transports, zenNodes: &nodePool{}, goNodes: &nodePool{}, anonymous: newAnonymousPool(true, transports, time.Second), catalog: NewCatalog(TierZen, nil), ready: true}
	model := "deepseek-v4.1-flash-free"
	rt.catalog.ReplaceWithCapabilities([]string{model}, nil, map[Tier]map[string]protocol.Protocol{TierZen: {model: protocol.Chat}}, nil, nil)
	if os.Getenv("WORK2API_LIVE_AUTO_TOOLS") == "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
		defer cancel()
		payload := map[string]any{"model": "opencode/" + model, "max_tokens": 2048, "reasoning_effort": "low", "tool_choice": "auto",
			"messages": []any{map[string]any{"role": "user", "content": "Use the read tool to read /tmp/work2api-probe.txt. Call the tool, do not answer with instructions."}},
			"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "read", "description": "Read a local text file.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}, "required": []any{"file_path"}}}}}}
		for _, external := range []protocol.Protocol{protocol.Chat, protocol.Anthropic, protocol.Responses} {
			converted, err := protocol.PrepareRequest(protocol.Chat, external, payload, "")
			if err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			report, err := rt.Serve(ctx, provider.ServeRequest{Protocol: provider.Protocol(external), Payload: converted, Writer: w})
			if err != nil || w.Code != 200 || report.Status != "ok" {
				t.Fatalf("automatic tool call failed protocol=%s HTTP=%d outcome=%s error=%v response=%s", external, w.Code, report.Status, err, w.Body.String())
			}
			var response map[string]any
			encoded, convertErr := protocol.ConvertResponse(external, protocol.Chat, w.Body.Bytes())
			if convertErr != nil {
				t.Fatal(convertErr)
			}
			if json.Unmarshal(encoded, &response) != nil {
				t.Fatal("invalid response")
			}
			choices, _ := response["choices"].([]any)
			if len(choices) != 1 {
				t.Fatal("expected one choice")
			}
			choice, _ := choices[0].(map[string]any)
			message, _ := choice["message"].(map[string]any)
			calls, _ := message["tool_calls"].([]any)
			if len(calls) != 1 {
				t.Fatal("expected client read call")
			}
			call, _ := calls[0].(map[string]any)
			fn, _ := call["function"].(map[string]any)
			var args map[string]any
			if json.Unmarshal([]byte(fmt.Sprint(fn["arguments"])), &args) != nil || fn["name"] != "read" || args["file_path"] != "/tmp/work2api-probe.txt" {
				t.Fatalf("automatic selection broke client tool contract: function=%v", fn)
			}
			t.Logf("automatic read selection preserved original name and file_path schema protocol=%s; no tool executed", external)
		}
		return
	}
	for _, proto := range []provider.Protocol{provider.ProtocolChat, provider.ProtocolAnthropic, provider.ProtocolResponses} {
		if os.Getenv("WORK2API_LIVE_IMAGES_ONLY") == "1" {
			continue
		}
		payload := map[string]any{"model": "opencode/" + model, "stream": false, "max_tokens": 128, "reasoning_effort": "low", "messages": []any{map[string]any{"role": "user", "content": "Reply with just OK."}}}
		if proto == provider.ProtocolResponses {
			delete(payload, "messages")
			payload["input"] = "Reply with just OK."
			payload["max_output_tokens"] = 128
		}
		ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
		w := httptest.NewRecorder()
		started := time.Now()
		report, err := rt.Serve(ctx, provider.ServeRequest{Protocol: proto, Payload: payload, Writer: w})
		cancel()
		t.Logf("protocol=%s status=%d duration=%s outcome=%s", proto, w.Code, time.Since(started).Round(time.Millisecond), report.Status)
		if err != nil || w.Code != 200 {
			t.Fatalf("upstream verification failed: HTTP %d, %s", w.Code, w.Body.String())
		}
	}
	for _, external := range []protocol.Protocol{protocol.Chat, protocol.Anthropic, protocol.Responses} {
		if os.Getenv("WORK2API_LIVE_IMAGES_ONLY") == "1" {
			continue
		}
		chat := map[string]any{"model": "opencode/" + model, "stream": false, "max_tokens": 2048, "reasoning_effort": "low",
			"messages":    []any{map[string]any{"role": "user", "content": "Call read with value hello. Do not answer without the tool."}},
			"tools":       []any{map[string]any{"type": "function", "function": map[string]any{"name": "read", "description": "Echo the value provided; no file access.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "string"}}, "required": []any{"value"}}}}},
			"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "read"}}}
		payload, err := protocol.PrepareRequest(protocol.Chat, external, chat, "")
		if err != nil {
			t.Fatal(err)
		}
		proto := provider.ProtocolChat
		if external == protocol.Anthropic {
			proto = provider.ProtocolAnthropic
		}
		if external == protocol.Responses {
			proto = provider.ProtocolResponses
		}
		invoke := func(p map[string]any) *httptest.ResponseRecorder {
			ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
			defer cancel()
			w := httptest.NewRecorder()
			start := time.Now()
			report, err := rt.Serve(ctx, provider.ServeRequest{Protocol: proto, Payload: p, Writer: w})
			t.Logf("tool protocol=%s status=%d duration=%s outcome=%s finish=%s", external, w.Code, time.Since(start).Round(time.Millisecond), report.Status, report.FinishReason)
			if err != nil || w.Code != 200 || report.Status != "ok" {
				t.Fatalf("tool verification failed HTTP %d: %s", w.Code, w.Body.String())
			}
			return w
		}
		first := invoke(payload)
		decoded, err := protocol.ConvertResponse(external, protocol.Chat, first.Body.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if json.Unmarshal(decoded, &result) != nil {
			t.Fatal("invalid response")
		}
		choices := result["choices"].([]any)
		message := choices[0].(map[string]any)["message"].(map[string]any)
		calls, _ := message["tool_calls"].([]any)
		if len(calls) != 1 {
			t.Fatal("forced client tool not called", string(decoded))
		}
		call := calls[0].(map[string]any)
		fn := call["function"].(map[string]any)
		if fn["name"] != "read" {
			t.Fatal("client name not restored")
		}
		var arguments map[string]any
		if json.Unmarshal([]byte(fn["arguments"].(string)), &arguments) != nil || arguments["value"] != "hello" {
			t.Fatal("client schema not respected", fn)
		}
		chat["model"] = "opencode/" + model
		chat["messages"] = append(chat["messages"].([]any), message, map[string]any{"role": "tool", "tool_call_id": call["id"], "content": "The result is 42."}, map[string]any{"role": "user", "content": "Now reply with only the result number. Do not call any tools."})
		chat["tool_choice"] = "none"
		follow, err := protocol.PrepareRequest(protocol.Chat, external, chat, "")
		if err != nil {
			t.Fatal(err)
		}
		invoke(follow)
		// Verify aliases do not leak through real SSE events, including native
		// Responses events used by the outer namespace/custom restoration layer.
		payload["model"] = "opencode/" + model
		payload["stream"] = true
		streamed := invoke(payload)
		_, aliases := aliasClientTools(payload, external, false, false)
		for _, line := range strings.Split(streamed.Body.String(), "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var event map[string]any
			if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) == nil && restoreToolNames(event, aliases) {
				t.Fatal("structured tool name alias leaked")
			}
		}
		if !strings.Contains(streamed.Body.String(), `"name":"read"`) {
			t.Fatal("client tool name missing")
		}
	}
	// A tiny generated PNG avoids using any user attachment in upstream tests.
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, color.RGBA{R: 255, A: 255})
		}
	}
	var pngBytes bytes.Buffer
	if err := png.Encode(&pngBytes, img); err != nil {
		t.Fatal(err)
	}
	picture := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": base64.StdEncoding.EncodeToString(pngBytes.Bytes())}}
	for _, external := range []protocol.Protocol{protocol.Anthropic, protocol.Responses} {
		input := map[string]any{"model": "opencode/" + model, "max_tokens": 256, "stream": false, "messages": []any{
			map[string]any{"role": "user", "content": "Use the screenshot tool and identify the dominant color. Answer with one English color word."},
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "shot1", "name": "screenshot", "input": map[string]any{}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "shot1", "content": []any{picture}}}},
		}, "tools": []any{map[string]any{"name": "screenshot", "description": "Return an image", "input_schema": map[string]any{"type": "object", "properties": map[string]any{}}}}}
		if external == protocol.Responses {
			var err error
			input, err = protocol.PrepareRequest(protocol.Anthropic, protocol.Responses, input, "")
			if err != nil {
				t.Fatal(err)
			}
		}
		proto := provider.ProtocolAnthropic
		if external == protocol.Responses {
			proto = provider.ProtocolResponses
		}
		ctx, cancel := context.WithTimeout(context.Background(), 70*time.Second)
		w := httptest.NewRecorder()
		start := time.Now()
		report, err := rt.Serve(ctx, provider.ServeRequest{Protocol: proto, Payload: input, Writer: w})
		cancel()
		t.Logf("tool-image protocol=%s HTTP=%d duration=%s outcome=%s", external, w.Code, time.Since(start).Round(time.Millisecond), report.Status)
		if err != nil || w.Code != 200 || !strings.Contains(strings.ToLower(w.Body.String()), "red") {
			t.Fatalf("tool image not recognized: %s", w.Body.String())
		}
	}
}

type catalogTestTransport func(*http.Request) (*http.Response, error)

type flushCheckedReader struct {
	first bool
	w     *httptest.ResponseRecorder
}

func (r *flushCheckedReader) Read(p []byte) (int, error) {
	if !r.first {
		r.first = true
		return copy(p, []byte("event: progress\ndata: {\"step\":1}\n\n")), nil
	}
	if !r.w.Flushed {
		return 0, errors.New("first SSE event was buffered")
	}
	return 0, io.EOF
}

func TestSystemOneFlushesBeforeReadingNextEvent(t *testing.T) {
	w := httptest.NewRecorder()
	proxy := &proxyTransport{client: &http.Client{Transport: catalogTestTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(&flushCheckedReader{w: w}), Request: req}, nil
	})}}
	proxy.healthy.Store(true)
	transports := &transportPool{items: []*proxyTransport{proxy}}
	nodes, err := newNodePool([]string{"fixture"}, transports, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.Retry.MaxAttempts = 1
	cfg.Performance.AttemptTimeoutSeconds = 5
	rt := &Runtime{cfg: cfg, transports: transports, zenNodes: nodes, goNodes: &nodePool{}, catalog: NewCatalog(TierZen, nil)}
	rt.catalog.ReplaceWithCapabilities([]string{"decision"}, nil, map[Tier]map[string]protocol.Protocol{TierZen: {"decision": protocol.SystemOne}}, nil, nil)
	report, err := rt.serveSystemOne(context.Background(), w, protocol.SystemOne, map[string]any{"model": "decision", "stream": true}, Route{ID: "decision", Tier: TierZen, Protocol: protocol.SystemOne}, RequestIDs{})
	if err != nil || report.Status != "ok" || !w.Flushed || !strings.Contains(w.Body.String(), `"step":1`) {
		t.Fatal(report, err, w.Body.String())
	}
}

func (f catalogTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestCatalogCancellationPreservesHealth(t *testing.T) {
	for _, anonymous := range []bool{false, true} {
		for _, before := range []bool{false, true} {
			t.Run(fmt.Sprintf("anonymous=%v/before=%v", anonymous, before), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				proxy := &proxyTransport{client: &http.Client{Transport: catalogTestTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					cancel()
					return nil, req.Context().Err()
				})}}
				proxy.healthy.Store(true)
				transports := &transportPool{items: []*proxyTransport{proxy}}
				nodes, err := newNodePool([]string{"one", "two"}, transports, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				anon := newAnonymousPool(true, transports, time.Minute)
				rt := &Runtime{cfg: Config{Retry: RetryConfig{MaxAttempts: 3}}, transports: transports, zenNodes: nodes, goNodes: &nodePool{}, anonymous: anon}
				if before {
					cancel()
				}
				if anonymous {
					rt.refreshAnonymousTier(ctx, "https://upstream.invalid")
				} else {
					rt.refreshTier(ctx, "https://upstream.invalid", nodes)
				}
				want := 1
				if before {
					want = 0
				}
				if calls != want || !proxy.healthy.Load() || proxy.checking.Load() {
					t.Fatalf("calls=%d healthy=%v checking=%v", calls, proxy.healthy.Load(), proxy.checking.Load())
				}
				for _, node := range nodes.nodes {
					if node.failures.Load() != 0 || node.cooldownUntil.Load() != 0 || nodes.Proxy(node) != proxy {
						t.Fatal("cancellation penalized/rebound key")
					}
				}
				for _, node := range anon.nodes {
					if node.failures.Load() != 0 || node.cooldownUntil.Load() != 0 {
						t.Fatal("cancellation penalized anonymous node")
					}
				}
			})
		}
	}
}

func TestDNSFailurePreservesProxyAndNodeHealth(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout=%v", timeout), func(t *testing.T) {
			proxy := &proxyTransport{}
			proxy.healthy.Store(true)
			transports := &transportPool{items: []*proxyTransport{proxy}}
			nodes, err := newNodePool([]string{"test-key"}, transports, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			anon := &anonymousNode{proxy: proxy}
			rt := &Runtime{transports: transports, zenNodes: nodes, goNodes: &nodePool{}, anonymous: &anonymousPool{cooldown: time.Minute}}
			dnsErr := fmt.Errorf("request: %w", &net.DNSError{Err: "DNS failed", Name: "upstream.invalid", IsTimeout: timeout})
			if isProxyFailure(dnsErr) {
				t.Fatal("DNS failure classified as broken proxy")
			}
			rt.observeKeyResult(context.Background(), nodes, nodes.nodes[0], proxy, "https://example.invalid", nil, dnsErr)
			rt.observeAnonymousResult(context.Background(), anon, "https://example.invalid", nil, dnsErr)
			if !proxy.healthy.Load() || proxy.checking.Load() {
				t.Fatal("DNS failure mutated proxy health or launched a probe")
			}
			if nodes.nodes[0].failures.Load() != 0 || nodes.nodes[0].cooldownUntil.Load() != 0 || anon.failures.Load() != 0 || anon.cooldownUntil.Load() != 0 {
				t.Fatal("DNS failure penalized a key or anonymous node")
			}
			if nodes.Proxy(nodes.nodes[0]) != proxy {
				t.Fatal("DNS failure rebound the key")
			}
		})
	}
}

func TestNonDNSFailuresRemainPenalized(t *testing.T) {
	if !isProxyFailure(context.DeadlineExceeded) {
		t.Fatal("connection deadline must remain a proxy failure")
	}
	p := &anonymousPool{cooldown: time.Minute}
	n := &anonymousNode{}
	p.MarkFailure(n, nil, fmt.Errorf("connection reset"))
	if n.failures.Load() != 1 || n.cooldownUntil.Load() <= time.Now().UnixNano() {
		t.Fatal("ordinary failure no longer cools the node")
	}
}

func TestMergedMetadataRetainsTierLimitsAndNativeAuthority(t *testing.T) {
	models, err := decodeModelsDev([]byte(`{"opencode":{"models":{"same":{"cost":{"input":1,"output":2,"cache_read":0,"cache_write":1.25},"limit":{"context":100,"output":20}},"zen-only":{"cost":{"input":0,"output":0}}}},"opencode-go":{"id":"opencode-go","models":{"same":{"cost":{"input":0,"output":0},"limit":{"context":200,"input":160,"output":40}},"go-only":{"cost":{"input":0,"output":0},"limit":{"output":30}}}}}`))
	if err != nil || len(models) != 3 {
		t.Fatal(models, err)
	}
	store := &PricingStore{models: models, updatedAt: time.Now()}
	if store.Decide("same").Allowed || store.Decide("go-only").Allowed || !store.Decide("zen-only").Allowed {
		t.Fatal("Go pricing leaked to anonymous Zen")
	}
	c := NewCatalog(TierZen, nil)
	c.SetPricingStore(store)
	c.modelMeta = map[Tier]map[string]Metadata{TierZen: {"same": {ContextWindow: 90}}, TierGo: {}}
	zen, goMD := c.MetadataForTier("same", TierZen), c.MetadataForTier("same", TierGo)
	if zen.ContextWindow != 90 || zen.MaxOutput != 20 || goMD.ContextWindow != 200 || goMD.MaxInput != 160 || goMD.MaxOutput != 40 || goMD.ToolCall || goMD.Reasoning {
		t.Fatal(zen, goMD)
	}
	price, _ := store.Price("same")
	if price.CacheRead == nil || *price.CacheRead != 0 || price.CacheWrite == nil || *price.CacheWrite != 1.25 || models["zen-only"].CacheRead != nil {
		t.Fatal("cache pricing must retain actual zero and missing values separately")
	}
	price.Limits[TierGo] = ModelLimits{}
	if c.MetadataForTier("same", TierGo).MaxOutput != 40 {
		t.Fatal("metadata map aliases store")
	}
}

func TestDiscoveryUsesPublishedEffortsAndTierPrices(t *testing.T) {
	var raw capabilityModel
	if err := json.Unmarshal([]byte(`{"reasoning":true,"reasoning_options":[{"type":"toggle"},{"type":"effort","values":["low","high","high"]},{"type":"budget_tokens","min":1024}],"cost":{"input":0,"output":2,"cache_read":0,"cache_write":-1}}`), &raw); err != nil {
		t.Fatal(err)
	}
	md := raw.metadata()
	c := NewCatalog(TierZen, nil)
	c.ReplaceWithCapabilities(nil, nil, nil, nil, map[Tier]map[string]Metadata{TierZen: {"m": md}})
	md.ReasoningOptions[1].Values[0] = "corrupted"
	*md.Cost.Output = 999
	got := c.MetadataForTier("m", TierZen)
	if got.ReasoningOptions[1].Values[0] != "low" || *got.Cost.Output != 2 {
		t.Fatal("catalog retained caller references")
	}
	*got.Cost.Output = 7
	if *c.MetadataForTier("m", TierZen).Cost.Output != 2 {
		t.Fatal("returned metadata aliases cache")
	}
	rt := &Runtime{cfg: defaultConfig()}
	extra := map[string]any{}
	rt.addDiscoveryMetadata(extra, "m", c.MetadataForTier("m", TierZen), TierZen)
	if strings.Join(extra["reasoning_efforts"].([]string), ",") != "low,high" {
		t.Fatal("invented reasoning levels", extra)
	}
	cost := extra["cost"].(map[string]float64)
	if len(cost) != 4 || cost["input"] != 0 || cost["output"] != 2 || cost["cacheRead"] != 0 {
		t.Fatal("missing/zero/invalid reference prices confused", cost)
	}
	for _, options := range [][]ReasoningOption{nil, {{Type: "toggle"}}, {{Type: "budget_tokens"}}} {
		extra = map[string]any{}
		rt.addDiscoveryMetadata(extra, "m", Metadata{Reasoning: true, ReasoningOptions: options}, TierGo)
		if extra["reasoning_efforts"] != nil || extra["compat"] != nil {
			t.Fatal("non-effort controls became effort choices")
		}
	}
	rt.cfg.Reasoning.Effort = "high"
	extra = map[string]any{}
	rt.addDiscoveryMetadata(extra, "m", c.MetadataForTier("m", TierZen), TierZen)
	if strings.Join(extra["reasoning_efforts"].([]string), ",") != "high" || extra["default_reasoning_effort"] != "high" {
		t.Fatal("forced effort advertised ineffective choices", extra)
	}
	rt.cfg.Reasoning.Effort = "max"
	extra = map[string]any{}
	rt.addDiscoveryMetadata(extra, "m", c.MetadataForTier("m", TierZen), TierZen)
	if extra["reasoning_efforts"] != nil {
		t.Fatal("unverified override became model capability")
	}
	if got := c.MetadataForTier("m", TierGo); got.Cost != nil || len(got.ReasoningOptions) > 0 {
		t.Fatal("Zen capabilities leaked to Go")
	}
}

func TestStaleConfigEditorDoesNotWritePersistedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.json")
	original := []byte("existing configuration")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry()
	old := &Runtime{path: path}
	if err := registry.Register(old); err != nil {
		t.Fatal(err)
	}
	old.BindRegistry(registry)
	next := &Runtime{path: path}
	next.BindRegistry(registry)
	if err := registry.Replace(old, next); err != nil {
		t.Fatal(err)
	}
	if err := old.SaveConfigDoc(map[string]any{"anonymous": true}); err == nil {
		t.Fatal("stale editor accepted")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(original) {
		t.Fatal("stale editor overwrote configuration before failing")
	}
}

func TestExplicitCatalogRefreshRetainsFailedTierAndClearsEmptyTier(t *testing.T) {
	var phase atomic.Int32
	proxy := &proxyTransport{client: &http.Client{Transport: catalogTestTransport(func(req *http.Request) (*http.Response, error) {
		body := `{"opencode":{"npm":"@ai-sdk/openai","models":{"new":{"id":"new"}}}}`
		status := 200
		if req.URL.Host == "tier.invalid" {
			switch phase.Load() {
			case 0:
				body = `{"data":[{"id":"new"}]}`
			case 1:
				body = `{"data":[]}`
			case 2:
				status = 503
				body = `{}`
			default:
				body = `{}`
			}
		}
		if strings.HasSuffix(req.URL.Path, ".mdx") {
			status = 503
			body = `{}`
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}}
	proxy.healthy.Store(true)
	transports := &transportPool{items: []*proxyTransport{proxy}}
	nodes, err := newNodePool([]string{"key"}, transports, 0)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCatalog(TierZen, nil)
	c.ReplaceWithCapabilities([]string{"old"}, []string{"go-old"}, nil, nil, nil)
	rt := &Runtime{cfg: Config{Upstream: UpstreamConfig{Zen: "https://tier.invalid"}, Retry: RetryConfig{MaxAttempts: 1}}, transports: transports, zenNodes: nodes, goNodes: &nodePool{}, anonymous: &anonymousPool{}, catalog: c, ready: true}
	if err := rt.RefreshModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !c.zen["new"] || c.zen["old"] || !c.goModels["go-old"] {
		t.Fatal("successful replacement or unconfigured tier changed")
	}
	phase.Store(2)
	if err := rt.RefreshModels(context.Background()); err == nil || !c.zen["new"] || !c.stale {
		t.Fatal("failed refresh discarded snapshot", err)
	}
	cursor := nodes.Cursor()
	nodes.MarkSuccess(cursor.Next())
	phase.Store(1)
	if err := rt.RefreshModels(context.Background()); err != nil || len(c.zen) != 0 || c.stale {
		t.Fatal("valid empty response did not revoke old models", err)
	}
	phase.Store(3)
	if err := rt.RefreshModels(context.Background()); err == nil {
		t.Fatal("missing data accepted as authoritative empty")
	}
	lifecycle, cancel := context.WithCancel(context.Background())
	rt.lifecycleCtx = lifecycle
	cancel()
	if err := rt.RefreshModels(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal("closed runtime started refresh", err)
	}
}

func TestPortalKeyFilteringDoesNotConsumeUpstreamAttempts(t *testing.T) {
	calls := 0
	proxy := &proxyTransport{client: &http.Client{Transport: catalogTestTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.Header.Get("Authorization") != "Bearer own" {
			t.Fatal("foreign key dispatched")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}")), Header: make(http.Header)}, nil
	})}}
	proxy.healthy.Store(true)
	transports := &transportPool{items: []*proxyTransport{proxy}}
	nodes, e := newNodePool([]string{"foreign-one", "foreign-two", "own"}, transports, time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	rt := &Runtime{cfg: Config{Retry: RetryConfig{MaxAttempts: 1}, Upstream: UpstreamConfig{Zen: "https://upstream.invalid"}}, zenNodes: nodes, catalog: NewCatalog(TierZen, nil)}
	uid := PortalKeyUID("own")
	ctx := provider.WithInvocation(context.Background(), provider.Invocation{Caller: provider.Caller{UserID: 1, AccountScope: map[string]bool{uid: true}}, Check: func(_ context.Context, a provider.AccountRef, m string) error {
		if a.LocalID != uid || m != "opencode/paid-model" {
			t.Fatal("incorrect dispatch identity", a, m)
		}
		return nil
	}})
	route := Route{ID: "paid-model", Tier: TierZen, Protocol: protocol.Chat}
	bodies := map[Tier][]byte{TierZen: []byte(`{"model":"paid-model"}`)}
	accountUID := ""
	resp, e, attempts := rt.doKeyUpstream(ctx, route, fixtureRouteBodies(bodies), RequestIDs{AccountUID: &accountUID}, 0)
	if e != nil || resp == nil || calls != 1 || attempts != 1 || accountUID != uid {
		t.Fatal("authorized key starved by scope filtering", e, calls, attempts, accountUID)
	}
	resp.Body.Close()
	nodes.next.Store(0)
	calls = 0
	revoked := provider.WithInvocation(context.Background(), provider.Invocation{Caller: provider.Caller{UserID: 1, AccountScope: map[string]bool{uid: true}}, Check: func(context.Context, provider.AccountRef, string) error {
		return provider.NewAPIError(403, "revoked", "forbidden")
	}})
	if resp, e, _ = rt.doKeyUpstream(revoked, route, fixtureRouteBodies(bodies), RequestIDs{}, 0); resp != nil || e == nil || calls != 0 {
		t.Fatal("revoked credential dispatched", resp, e, calls)
	}
	if got := toStringSlice([]string{"kept-key"}); len(got) != 1 || got[0] != "kept-key" {
		t.Fatal("native import clears credentials")
	}
	rt.cfg.KeyModels = map[string][]string{uid: {"allowed-model"}}
	if rt.keySupportsModel(uid, "paid-model") || !rt.keySupportsModel(uid, "allowed-model") {
		t.Fatal("credential catalog ignored")
	}
}

func TestSingleAllowedKeyRetainsTransientRetryBudget(t *testing.T) {
	calls := 0
	proxy := &proxyTransport{client: &http.Client{Transport: catalogTestTransport(func(req *http.Request) (*http.Response, error) {
		// Connectivity probes are GETs, not model attempts. Keep their result
		// separate from the POST retry counter, including asynchronous probes.
		if req.Method == http.MethodGet {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
		}
		calls++
		status := 503
		if calls == 2 {
			status = 200
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}}
	proxy.healthy.Store(true)
	transports := &transportPool{items: []*proxyTransport{proxy}}
	nodes, e := newNodePool([]string{"one"}, transports, time.Minute)
	if e != nil {
		t.Fatal(e)
	}
	rt := &Runtime{cfg: Config{Retry: RetryConfig{MaxAttempts: 2}, Upstream: UpstreamConfig{Zen: "https://upstream.invalid"}}, zenNodes: nodes, transports: transports, catalog: NewCatalog(TierZen, nil)}
	resp, e, attempts := rt.doKeyUpstream(context.Background(), Route{ID: "paid-model", Tier: TierZen, Protocol: protocol.Chat}, fixtureRouteBodies(map[Tier][]byte{TierZen: []byte(`{}`)}), RequestIDs{}, 0)
	if e != nil || resp == nil || resp.StatusCode != 200 || attempts != 2 || calls != 2 {
		t.Fatal("single-key transient retries changed", e, attempts, calls)
	}
	resp.Body.Close()
}

func TestSSEHeadFailureUsesExistingRetryBudget(t *testing.T) {
	for _, kind := range []string{"server_error", "policy_rejection", "after_output", "empty"} {
		calls := 0
		prelude := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"
		answer := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"
		proxy := &proxyTransport{client: &http.Client{Transport: catalogTestTransport(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodGet {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
			}
			calls++
			wire := prelude + answer
			if calls == 1 {
				wire = prelude + "data: {\"error\":{\"type\":\"" + kind + "\",\"message\":\"rejected\"}}\n\n"
				if kind == "after_output" {
					wire = prelude + answer + "data: {\"error\":{\"type\":\"server_error\"}}\n\n"
				}
				if kind == "empty" {
					wire = prelude
				}
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}, nil
		})}}
		proxy.healthy.Store(true)
		transports := &transportPool{items: []*proxyTransport{proxy}}
		nodes, e := newNodePool([]string{"one"}, transports, time.Minute)
		if e != nil {
			t.Fatal(e)
		}
		rt := &Runtime{cfg: Config{Retry: RetryConfig{MaxAttempts: 2}, Upstream: UpstreamConfig{Zen: "https://upstream.invalid"}}, zenNodes: nodes, transports: transports, catalog: NewCatalog(TierZen, nil)}
		resp, e, attempts := rt.doKeyUpstream(context.Background(), Route{ID: "model", Tier: TierZen, Protocol: protocol.Chat}, fixtureRouteBodies(map[Tier][]byte{TierZen: []byte(`{}`)}), RequestIDs{}, 0)
		if e != nil || resp == nil {
			t.Fatal(kind, e)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		want := 1
		if kind == "server_error" || kind == "empty" {
			want = 2
		}
		if calls != want || attempts != want {
			t.Fatal(kind, "wrong retry count", calls, attempts)
		}
		if kind == "policy_rejection" {
			if resp.StatusCode != 400 {
				t.Fatal("policy rejection retried", resp.StatusCode)
			}
		} else if !strings.HasPrefix(string(body), prelude+answer) {
			t.Fatal(kind, "prefix lost", string(body))
		}
	}
}

func TestSSEHeadCancellationClosesReader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	defer writer.Close()
	entered := make(chan struct{})
	client := &http.Client{Transport: catalogTestTransport(func(req *http.Request) (*http.Response, error) {
		close(entered)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: reader}, nil
	})}
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://upstream.invalid/v1/chat/completions", strings.NewReader(`{}`))
	done := make(chan error, 1)
	go func() {
		resp, err := doInferenceAttempt(client, req, protocol.Chat)
		if resp != nil {
			resp.Body.Close()
		}
		done <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("prelude reader survived client cancellation")
	}
}

func BenchmarkFreeShapeEncoding(b *testing.B) {
	model := "fixture-free"
	route := Route{ID: model, Tier: TierZen, Protocol: protocol.Chat}
	rt := &Runtime{cfg: defaultConfig(), catalog: NewCatalog(TierZen, nil)}
	input := map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": strings.Repeat("long context ", 80000)}}}
	before := func() ([]byte, error) {
		payload, err := protocol.PrepareRequest(protocol.Chat, protocol.Chat, input, rt.cfg.Upstream.Zen)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		var decoded map[string]any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			return nil, err
		}
		shapeFreePayload(decoded, protocol.Chat)
		return json.Marshal(decoded)
	}
	after := func() ([]byte, error) {
		body, _, err := rt.prepareRouteBody(protocol.Chat, route, input, true, TierZen)
		return body, err
	}
	a, err := before()
	if err != nil {
		b.Fatal(err)
	}
	z, err := after()
	if err != nil || !bytes.Equal(a, z) || input["stream"] != nil {
		b.Fatal("free shaping wire contract changed", err)
	}
	for _, tc := range []struct {
		name   string
		encode func() ([]byte, error)
	}{{"before", before}, {"after", after}} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := tc.encode(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestFreeTierRecoveryPreservesShapedToolBody(t *testing.T) {
	model := "fixture-free"
	rt := &Runtime{cfg: defaultConfig(), catalog: NewCatalog(TierZen, nil)}
	route := Route{ID: model, Tier: TierZen, Protocol: protocol.Responses, KeyTiers: []Tier{TierZen, TierGo}}
	input := map[string]any{"model": model, "previous_response_id": "expired", "input": []any{
		map[string]any{"type": "reasoning", "id": "rs_expired", "encrypted_content": "opaque"},
		map[string]any{"type": "message", "role": "user", "content": "continue"},
	}}
	bodies, limits, err := rt.prepareRouteBodiesWithLimits(protocol.Responses, route, input, true)
	if err != nil {
		t.Fatal(err)
	}
	recovered, changed := recoverRouteBodies(route, bodies)
	if !changed {
		t.Fatal("expected recovery for both tiers")
	}
	if _, ready := limits[TierGo]; ready {
		t.Fatal("fallback tier prepared before first use")
	}
	for _, tier := range []Tier{TierZen, TierGo} {
		var payload map[string]any
		body, err := recovered(tier)
		if err != nil {
			t.Fatal(err)
		}
		if again, err := recovered(tier); err != nil || len(again) == 0 || len(body) == 0 || &again[0] != &body[0] {
			t.Fatal("encoded body was not reused", tier, err)
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatal(err)
		}
		tools, _ := payload["tools"].([]any)
		items, _ := payload["input"].([]any)
		if payload["stream"] != true || payload["previous_response_id"] != "expired" || len(tools) != 2 || len(items) != 1 {
			t.Fatal(tier, payload)
		}
		if item, ok := items[0].(map[string]any); !ok || item["role"] != "user" {
			t.Fatal(tier, items)
		}
	}
	if input["stream"] != nil || input["previous_response_id"] != "expired" {
		t.Fatal("caller payload mutated", input)
	}
}

func fixtureRouteBodies(bodies map[Tier][]byte) routeBody {
	return func(tier Tier) ([]byte, error) { return bodies[tier], nil }
}

func TestStaleReasoningRecoveryKeepsServerConversation(t *testing.T) {
	for _, body := range []string{
		`{"previous_response_id":"resp_prior","input":"continue"}`,
		`{"previous_response_id":"resp_prior","input":[{"type":"message","role":"user","content":"continue"}]}`,
	} {
		out, changed := stripStaleReasoningBody([]byte(body))
		if changed || string(out) != body {
			t.Fatal("continuation-only request was reset", string(out))
		}
	}
	body := []byte(`{"previous_response_id":"resp_prior","input":[{"type":"reasoning","id":"expired"},{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":"result"}]}`)
	out, changed := stripStaleReasoningBody(body)
	if !changed {
		t.Fatal("stale reasoning was not repaired")
	}
	var payload map[string]any
	if err := json.Unmarshal(out, &payload); err != nil {
		t.Fatal(err)
	}
	items := payload["input"].([]any)
	if payload["previous_response_id"] != "resp_prior" || len(items) != 2 || items[0].(map[string]any)["call_id"] != "c1" || items[1].(map[string]any)["output"] != "result" {
		t.Fatal("conversation or tools lost", payload)
	}
	again, changed := stripStaleReasoningBody(out)
	if changed || string(again) != string(out) {
		t.Fatal("recovery is not idempotent")
	}
}
