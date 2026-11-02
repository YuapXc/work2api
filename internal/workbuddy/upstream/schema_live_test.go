package upstream_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
