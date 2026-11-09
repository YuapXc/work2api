package adapters

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"work2api/internal/core/protocol/chatstream"
	"work2api/internal/workbuddy/reasoning"

	"work2api/internal/workbuddy/upstream"
)

func TestDeepseekHistoryReplayAcrossAdapters(t *testing.T) {
	for _, model := range []string{"deepseek-v4.1-flash", "other-model"} {
		for _, protocol := range []string{"messages", "responses"} {
			var input map[string]any
			convert := AnthropicRequestToChat
			if protocol == "messages" {
				input = map[string]any{"model": model, "messages": []any{
					map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": "first", "signature": "opaque-signature"}, map[string]any{"type": "redacted_thinking", "data": "opaque"}, map[string]any{"type": "tool_use", "id": "c1", "name": "report", "input": map[string]any{"value": 1}}}},
					map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "c1", "content": "one"}}},
					map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "thinking", "thinking": "second"}, map[string]any{"type": "text", "text": "answer"}, map[string]any{"type": "tool_use", "id": "c2", "name": "report", "input": map[string]any{"value": 2}}}},
					map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "c2", "content": "two"}}},
				}}
			} else {
				convert = ResponsesRequestToChat
				input = map[string]any{"model": model, "input": []any{
					map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "first"}}, "encrypted_content": "opaque"},
					map[string]any{"type": "function_call", "call_id": "c1", "name": "report", "arguments": `{"value":1}`},
					map[string]any{"type": "function_call_output", "call_id": "c1", "output": "one"},
					map[string]any{"type": "reasoning", "content": []any{map[string]any{"type": "reasoning_text", "text": "second"}}},
					map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "answer"}}},
					map[string]any{"type": "function_call", "call_id": "c2", "name": "report", "arguments": `{"value":2}`},
					map[string]any{"type": "function_call_output", "call_id": "c2", "output": "two"},
					map[string]any{"type": "reasoning", "encrypted_content": "unreadable"},
				}}
			}
			before, _ := json.Marshal(input)
			chat, err := convert(input)
			if err != nil {
				t.Fatal(err)
			}
			chat = reasoning.BackfillReasoningContent(chat)
			messages := chat["messages"].([]any)
			if len(messages) != 4 {
				t.Fatal("thinking split from tools or phantom message", protocol, messages)
			}
			for i, want := range []string{"first", "second"} {
				message := messages[i*2].(map[string]any)
				if model == "deepseek-v4.1-flash" && message["reasoning"] != want {
					t.Fatal("thinking lost or assigned to wrong round", protocol, message)
				}
				if model == "other-model" && message["reasoning"] != nil {
					t.Fatal("unverified model changed")
				}
				call := message["tool_calls"].([]any)[0].(map[string]any)
				if call["id"] != []string{"c1", "c2"}[i] || call["function"].(map[string]any)["arguments"] != []string{`{"value":1}`, `{"value":2}`}[i] {
					t.Fatal("tool contract changed")
				}
			}
			wire, _ := json.Marshal(chat)
			if strings.Contains(string(wire), "opaque") || strings.Contains(string(wire), "unreadable") {
				t.Fatal("opaque payload converted to plaintext")
			}
			after, _ := json.Marshal(input)
			if string(before) != string(after) {
				t.Fatal("source mutated")
			}
		}
	}
}

func TestUpstreamRefusalSurvivesAdapters(t *testing.T) {
	for _, first := range []bool{true, false} {
		responses := chatstream.NewResponsesStreamConverter("m")
		messages := chatstream.NewAnthropicStreamConverter("m")
		lines := []string{chatChunk(map[string]any{"refusal": "不能执行该请求。"}), chatChunk(map[string]any{"content": "说明"})}
		if !first {
			lines[0], lines[1] = lines[1], lines[0]
		}
		var events strings.Builder
		for _, line := range lines {
			events.WriteString(responses.FeedLine(line))
			messages.FeedLine(line)
		}
		events.WriteString(responses.Finish())
		if !strings.Contains(events.String(), `"type":"response.refusal.delta"`) || !strings.Contains(events.String(), `"type":"response.refusal.done"`) {
			t.Fatal("refusal lifecycle lost", events.String())
		}
		output := responses.GetNonstreamResponse()["output"].([]any)[0].(map[string]any)["content"].([]any)
		idx := 1
		if first {
			idx = 0
		}
		if output[idx].(map[string]any)["type"] != "refusal" || output[idx].(map[string]any)["refusal"] != "不能执行该请求。" {
			t.Fatal("refusal block/order lost", output)
		}
		if !strings.Contains(messages.GetNonstreamResponse()["content"].([]any)[0].(map[string]any)["text"].(string), "不能执行该请求。") {
			t.Fatal("Messages refusal explanation lost")
		}
	}
	collected, err := upstream.CollectStream("m", func(yield upstream.LineFunc) error {
		if err := yield(chatChunk(map[string]any{"refusal": "denied"})); err != nil {
			return err
		}
		return yield(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`)
	})
	if err != nil || collected["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)["refusal"] != "denied" {
		t.Fatal("Chat nonstream refusal lost", collected, err)
	}
}

func TestUpstreamErrorFinishIsNotSuccess(t *testing.T) {
	for _, reason := range []string{"error", "failed", "failure", "network_error", "server_error"} {
		_, err := upstream.CollectStream("m", func(yield upstream.LineFunc) error {
			if err := yield(chatChunk(map[string]any{"content": "partial"})); err != nil {
				return err
			}
			return yield(`data: {"choices":[{"delta":{},"finish_reason":"` + reason + `"}]}`)
		})
		if err == nil {
			t.Fatal("upstream failure reported as success", reason)
		}
	}
}

func TestAnthropicClientCapabilitiesSurviveConversion(t *testing.T) {
	system := "You are Claude Code.\n<skills_instructions>\n### Available skills\nfind-skills: discover skills at skill://find-skills/SKILL.md\n</skills_instructions>\nAvailable agent types for the Agent tool:\nreviewer: review CLAUDE.md\n## MCP Server Instructions\nUse mcp__docs__search precisely."
	body := map[string]any{
		"system": []any{map[string]any{"type": "text", "text": system}, map[string]any{"type": "text", "text": "Project constraint: preserve user files."}},
		"tools": []any{
			map[string]any{"name": "Skill", "description": "Invoke find-skills only when relevant", "input_schema": map[string]any{"type": "object"}},
			map[string]any{"name": "Agent", "description": "Delegate to reviewer", "input_schema": map[string]any{"type": "object"}},
		},
		"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "<system-reminder>Plan mode: read only.</system-reminder>"}}}},
	}
	before, _ := json.Marshal(body)
	chat, err := AnthropicRequestToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	if got := chat["messages"].([]any)[0].(map[string]any)["content"]; got != system+"\nProject constraint: preserve user files." {
		t.Fatalf("system instructions changed: %v", got)
	}
	if got := chat["messages"].([]any)[1].(map[string]any)["content"]; got != "<system-reminder>Plan mode: read only.</system-reminder>" {
		t.Fatal("user reminder changed", got)
	}
	for i, raw := range chat["tools"].([]any) {
		fn := raw.(map[string]any)["function"].(map[string]any)
		original := body["tools"].([]any)[i].(map[string]any)
		if fn["name"] != original["name"] || fn["description"] != original["description"] || !reflect.DeepEqual(fn["parameters"], original["input_schema"]) {
			t.Fatal("tool metadata changed", fn)
		}
	}
	after, _ := json.Marshal(body)
	if string(before) != string(after) {
		t.Fatal("converter mutated request")
	}
}

func TestAnthropicDeferredClientToolDiscovery(t *testing.T) {
	tool := func(name string, deferred bool) any {
		return map[string]any{"name": name, "description": "Use " + name, "input_schema": map[string]any{"type": "object"}, "defer_loading": deferred}
	}
	for _, tc := range []struct {
		name     string
		messages []any
		choice   any
		want     []string
		wantErr  bool
	}{
		{name: "initial", want: []string{"ToolSearch"}},
		{name: "discovered", messages: []any{
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "search-1", "name": "ToolSearch", "input": map[string]any{"query": "docs"}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "search-1", "content": []any{map[string]any{"type": "text", "text": "Found:"}, map[string]any{"type": "tool_reference", "tool_name": "mcp__docs__search"}}}}},
		}, want: []string{"ToolSearch", "mcp__docs__search"}},
		{name: "previously invoked", messages: []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "call-1", "name": "mcp__docs__search", "input": map[string]any{}}}}}, want: []string{"ToolSearch", "mcp__docs__search"}},
		{name: "explicit choice", choice: map[string]any{"type": "tool", "name": "mcp__docs__search"}, want: []string{"ToolSearch", "mcp__docs__search"}},
		{name: "legacy named choice", choice: "mcp__docs__search", want: []string{"ToolSearch", "mcp__docs__search"}},
		{name: "unknown reference", messages: []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "search-1", "content": []any{map[string]any{"type": "tool_reference", "tool_name": "not_defined"}}}}}}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]any{"tools": []any{tool("ToolSearch", false), tool("mcp__docs__search", true), tool("mcp__private__write", true)}, "messages": tc.messages}
			if tc.choice != nil {
				body["tool_choice"] = tc.choice
			}
			before, _ := json.Marshal(body)
			chat, err := AnthropicRequestToChat(body)
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v", err)
			}
			if err != nil {
				return
			}
			var names []string
			for _, raw := range chat["tools"].([]any) {
				names = append(names, raw.(map[string]any)["function"].(map[string]any)["name"].(string))
			}
			if !reflect.DeepEqual(names, tc.want) {
				t.Fatalf("loaded tools = %v, want %v", names, tc.want)
			}
			if tc.name == "discovered" {
				result := chat["messages"].([]any)[1].(map[string]any)
				if result["role"] != "tool" || result["tool_call_id"] != "search-1" || !strings.Contains(result["content"].(string), `"tool_name":"mcp__docs__search"`) || !strings.HasPrefix(result["content"].(string), "Found:") {
					t.Fatal("discovery result lost", result)
				}
			}
			after, _ := json.Marshal(body)
			if string(before) != string(after) {
				t.Fatal("request mutated")
			}
		})
	}
}

func TestAnthropicParallelControlAndToolErrors(t *testing.T) {
	for _, typ := range []string{"auto", "any", "tool", "none"} {
		for _, disabled := range []bool{false, true} {
			chat, err := AnthropicRequestToChat(map[string]any{"tool_choice": map[string]any{"type": typ, "name": "read", "disable_parallel_tool_use": disabled}})
			if err != nil || chat["parallel_tool_calls"] != !disabled {
				t.Fatal(typ, disabled, chat, err)
			}
		}
	}
	chat, err := AnthropicRequestToChat(map[string]any{"tool_choice": map[string]any{"type": "auto"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := chat["parallel_tool_calls"]; exists {
		t.Fatal("absent flag acquired a value")
	}
	for _, content := range []any{"Permission denied", []any{map[string]any{"type": "text", "text": "Permission denied"}, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AA=="}}}} {
		chat, err = AnthropicRequestToChat(map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "call-1", "is_error": true, "content": content}, map[string]any{"type": "text", "text": "continue"}}}}})
		if err != nil {
			t.Fatal(err)
		}
		messages := chat["messages"].([]any)
		result := messages[0].(map[string]any)
		encoded, _ := json.Marshal(result["content"])
		if result["role"] != "tool" || result["tool_call_id"] != "call-1" || !strings.Contains(string(encoded), "is_error=true") || !strings.Contains(string(encoded), "Permission denied") || messages[1].(map[string]any)["content"] != "continue" {
			t.Fatal(messages)
		}
		if _, image := content.([]any); image && !strings.Contains(string(encoded), "data:image/png;base64,AA==") {
			t.Fatal("error image lost")
		}
	}
	for _, body := range []map[string]any{
		{"tool_choice": map[string]any{"type": "auto", "disable_parallel_tool_use": "true"}},
		{"tools": []any{map[string]any{"type": "tool_search_tool_regex_20251119", "name": "search"}}},
		{"tools": []any{map[string]any{"name": "search", "defer_loading": "true"}}},
		{"tools": []any{map[string]any{"name": "search", "defer_loading": true}}},
		{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "document"}}}}},
	} {
		if _, err := AnthropicRequestToChat(body); err == nil {
			t.Fatal("unsupported/malformed capability silently accepted", body)
		}
	}
}

func chatChunk(delta map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
	})
	return "data: " + string(b) + "\n\n"
}

// TestAnthropicStreamThinking verifies reasoning_content deltas from the
// upstream become thinking blocks (thinking_delta) ahead of the text block,
// with blocks closed and reopened when the model interleaves.

// TestAnthropicNonstreamThinking checks the non-streaming response carries a
// thinking block before the text block.

// TestAnthropicStreamNoThinking keeps the plain-text stream untouched when the
// upstream never sends reasoning_content.

// TestResponsesStreamReasoning verifies reasoning_content becomes a reasoning
// output item with a summary_text part.

// TestAnthropicRequestBudgetToEffort locks the thinking.budget_tokens →
// reasoning_effort ladder and the output_config.effort precedence.
func TestAnthropicRequestBudgetToEffort(t *testing.T) {
	cases := []struct {
		budget int
		want   string
	}{
		{0, "high"}, {1024, "low"}, {4096, "medium"},
		{8192, "high"}, {16384, "xhigh"}, {32000, "xhigh"}, {32768, "max"},
	}
	for _, tc := range cases {
		if got := budgetToEffort(tc.budget); got != tc.want {
			t.Errorf("budgetToEffort(%d) = %q, want %q", tc.budget, got, tc.want)
		}
	}

	body := map[string]any{
		"model":    "glm-5.3-flash",
		"thinking": map[string]any{"type": "enabled", "budget_tokens": 32000},
	}
	chat, err := AnthropicRequestToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	if chat["reasoning_effort"] != "xhigh" {
		t.Fatalf("budget 32000 → %v, want xhigh", chat["reasoning_effort"])
	}

	// explicit output_config.effort wins over the budget
	body = map[string]any{
		"model":         "glm-5.3-flash",
		"thinking":      map[string]any{"type": "enabled", "budget_tokens": 1024},
		"output_config": map[string]any{"effort": "max"},
	}
	chat, err = AnthropicRequestToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	if chat["reasoning_effort"] != "max" {
		t.Fatalf("explicit effort → %v, want max", chat["reasoning_effort"])
	}

	// thinking disabled injects nothing
	body = map[string]any{
		"model":    "glm-5.3-flash",
		"thinking": map[string]any{"type": "disabled"},
	}
	chat, err = AnthropicRequestToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := chat["reasoning_effort"]; has {
		t.Fatalf("disabled thinking must not set reasoning_effort: %v", chat["reasoning_effort"])
	}

	// no thinking at all injects nothing
	chat, err = AnthropicRequestToChat(map[string]any{"model": "m", "max_tokens": 100})
	if err != nil {
		t.Fatal(err)
	}
	if _, has := chat["reasoning_effort"]; has {
		t.Fatalf("no thinking must not set reasoning_effort")
	}
}

func TestResponsesNativeOptionsAdaptToChat(t *testing.T) {
	body := map[string]any{"input": "hello", "reasoning": map[string]any{"effort": "high"}, "text": map[string]any{"format": map[string]any{"type": "json_schema", "name": "answer", "strict": true, "schema": map[string]any{"type": "object"}}}, "tool_choice": map[string]any{"type": "function", "name": "lookup"}, "parallel_tool_calls": false}
	chat, err := ResponsesRequestToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	if chat["reasoning_effort"] != "high" || chat["parallel_tool_calls"] != false {
		t.Fatal("native options lost", chat)
	}
	choice := chat["tool_choice"].(map[string]any)
	if choice["function"].(map[string]any)["name"] != "lookup" {
		t.Fatal("tool choice lost")
	}
	schema := chat["response_format"].(map[string]any)["json_schema"].(map[string]any)
	if schema["name"] != "answer" || schema["strict"] != true || schema["schema"] == nil {
		t.Fatal("structured output schema lost", schema)
	}
}

func TestResponsesAdapterRequiresPortableHistory(t *testing.T) {
	input := map[string]any{"model": "deepseek-v4.1-flash", "previous_response_id": "resp_prior", "input": []any{map[string]any{"role": "user", "content": "next"}}}
	before, _ := json.Marshal(input)
	if _, err := ResponsesRequestToChat(input); err == nil || !strings.Contains(err.Error(), "previous_response_id") {
		t.Fatal("server continuation silently became a new conversation", err)
	}
	after, _ := json.Marshal(input)
	if string(before) != string(after) {
		t.Fatal("mutated rejected request")
	}
	delete(input, "previous_response_id")
	if _, err := ResponsesRequestToChat(input); err != nil {
		t.Fatal("portable input rejected", err)
	}
}
