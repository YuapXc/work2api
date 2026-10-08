package adapters

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

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
func TestAnthropicStreamThinking(t *testing.T) {
	c := NewAnthropicStreamConverter("glm-5.3-flash")
	var out strings.Builder
	for _, line := range []string{
		chatChunk(map[string]any{"role": "assistant", "reasoning_content": "先想"}),
		chatChunk(map[string]any{"reasoning_content": "一下"}),
		chatChunk(map[string]any{"content": "答案"}),
		chatChunk(map[string]any{"reasoning_content": "再想"}),
		chatChunk(map[string]any{"content": "9.9"}),
	} {
		out.WriteString(c.FeedLine(line))
	}
	out.WriteString(c.Finish())
	s := out.String()

	if !strings.Contains(s, `"type":"thinking","thinking":""`) &&
		!strings.Contains(s, `"thinking":"","type":"thinking"`) {
		t.Fatalf("missing thinking content_block_start:\n%s", s)
	}
	if !strings.Contains(s, `"type":"thinking_delta","thinking":"先想"`) &&
		!strings.Contains(s, `"thinking":"先想"`) {
		t.Fatalf("missing thinking_delta:\n%s", s)
	}
	if !strings.Contains(s, `"text":"9.9","type":"text_delta"`) {
		t.Fatalf("missing text_delta:\n%s", s)
	}
	// thinking block must be opened before the text block
	if strings.Index(s, `"type":"thinking"`) > strings.Index(s, `"type":"text_delta"`) {
		t.Fatalf("thinking block should precede text:\n%s", s)
	}
	// interleaving: after the first text, a new thinking delta must stop text and
	// reopen thinking; then text reopens as a new block index
	if strings.Count(s, `content_block_stop`) < 3 {
		t.Fatalf("expected blocks to be closed/reopened on interleaving:\n%s", s)
	}
	// usage-log accessor
	if c.Reasoning() != "先想一下再想" {
		t.Fatalf("Reasoning() = %q", c.Reasoning())
	}
}

// TestAnthropicNonstreamThinking checks the non-streaming response carries a
// thinking block before the text block.
func TestAnthropicNonstreamThinking(t *testing.T) {
	c := NewAnthropicStreamConverter("glm-5.3-flash")
	_ = c.FeedLine(chatChunk(map[string]any{"reasoning_content": "思考过程"}))
	_ = c.FeedLine(chatChunk(map[string]any{"content": "正文"}))
	resp := c.GetNonstreamResponse()
	content, ok := resp["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("content blocks = %#v", resp["content"])
	}
	thinking, _ := content[0].(map[string]any)
	text, _ := content[1].(map[string]any)
	if thinking["type"] != "thinking" || thinking["thinking"] != "思考过程" {
		t.Fatalf("thinking block = %#v", thinking)
	}
	if text["type"] != "text" || text["text"] != "正文" {
		t.Fatalf("text block = %#v", text)
	}
}

// TestAnthropicStreamNoThinking keeps the plain-text stream untouched when the
// upstream never sends reasoning_content.
func TestAnthropicStreamNoThinking(t *testing.T) {
	c := NewAnthropicStreamConverter("m")
	var out strings.Builder
	out.WriteString(c.FeedLine(chatChunk(map[string]any{"content": "hello"})))
	out.WriteString(c.Finish())
	s := out.String()
	if strings.Contains(s, "thinking") {
		t.Fatalf("unexpected thinking blocks:\n%s", s)
	}
}

// TestResponsesStreamReasoning verifies reasoning_content becomes a reasoning
// output item with a summary_text part.
func TestResponsesStreamReasoning(t *testing.T) {
	c := NewResponsesStreamConverter("glm-5.3-flash")
	var out strings.Builder
	out.WriteString(c.FeedLine(chatChunk(map[string]any{"reasoning_content": "想"})))
	out.WriteString(c.FeedLine(chatChunk(map[string]any{"content": "答"})))
	out.WriteString(c.Finish())

	resp := c.GetNonstreamResponse()
	output, ok := resp["output"].([]any)
	if !ok || len(output) != 2 {
		t.Fatalf("output items = %#v", resp["output"])
	}
	rs, _ := output[0].(map[string]any)
	if rs["type"] != "reasoning" {
		t.Fatalf("first item = %#v", rs)
	}
	summary, _ := rs["summary"].([]any)
	if len(summary) != 1 || summary[0].(map[string]any)["text"] != "想" {
		t.Fatalf("summary = %#v", summary)
	}
	if c.Reasoning() != "想" {
		t.Fatalf("Reasoning() = %q", c.Reasoning())
	}
}

func TestResponsesOutputIdentityOrderAndIncompleteStatus(t *testing.T) {
	for _, finish := range []string{"stop", "length", "content_filter"} {
		for _, reasoningFirst := range []bool{true, false} {
			c := NewResponsesStreamConverter("m")
			var stream strings.Builder
			tool := map[string]any{"tool_calls": []any{map[string]any{"index": 3, "id": "call-3", "function": map[string]any{"name": "run", "arguments": "{\"x\":"}}}}
			if reasoningFirst {
				stream.WriteString(c.FeedLine(chatChunk(map[string]any{"reasoning_content": "think"})))
			} else {
				stream.WriteString(c.FeedLine(chatChunk(tool)))
			}
			stream.WriteString(c.FeedLine(chatChunk(map[string]any{"content": "answer"})))
			if reasoningFirst {
				stream.WriteString(c.FeedLine(chatChunk(tool)))
			} else {
				stream.WriteString(c.FeedLine(chatChunk(map[string]any{"reasoning_content": "think"})))
			}
			stream.WriteString(c.FeedLine(`data: {"choices":[{"delta":{},"finish_reason":"` + finish + `"}],"usage":{"prompt_tokens":10,"completion_tokens":7,"completion_tokens_details":{"reasoning_tokens":3}}}`))
			stream.WriteString(c.Finish())
			response := c.GetNonstreamResponse()
			output := response["output"].([]any)
			seq := 0
			added := map[string]bool{}
			for _, line := range strings.Split(stream.String(), "\n") {
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				var event map[string]any
				if err := json.Unmarshal([]byte(line[6:]), &event); err != nil {
					t.Fatal(err)
				}
				if event["sequence_number"] != float64(seq) {
					t.Fatal("non-monotonic event sequence", event)
				}
				seq++
				if index, ok := event["output_index"].(float64); ok {
					item := output[int(index)].(map[string]any)
					if id, ok := event["item_id"].(string); ok && (id != item["id"] || !added[id]) {
						t.Fatal("delta does not reference announced final item", event, item)
					}
					if announced, ok := event["item"].(map[string]any); ok {
						if announced["id"] != item["id"] {
							t.Fatal("item ID/index changed", event, item)
						}
						if event["type"] == "response.output_item.added" {
							added[announced["id"].(string)] = true
						}
					}
				}
			}
			status := "completed"
			if finish != "stop" {
				status = "incomplete"
				reason := "max_output_tokens"
				if finish == "content_filter" {
					reason = finish
				}
				if response["incomplete_details"].(map[string]any)["reason"] != reason {
					t.Fatal(response)
				}
			}
			if response["status"] != status || !strings.Contains(stream.String(), `"type":"response.`+status+`"`) {
				t.Fatal("incorrect terminal status", response)
			}
			usage := response["usage"].(map[string]any)
			if usage["output_tokens_details"].(map[string]any)["reasoning_tokens"] != 3 {
				t.Fatal("reasoning usage lost", usage)
			}
		}
	}
	c := NewResponsesStreamConverter("m")
	c.FeedLine(`data: {"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	if _, known := c.GetNonstreamResponse()["usage"].(map[string]any)["output_tokens_details"].(map[string]any)["reasoning_tokens"]; known {
		t.Fatal("unknown reasoning tokens invented")
	}
}

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

func TestResponsesFinishEventsPreserveCompletePayloadAndOrder(t *testing.T) {
	c := NewResponsesStreamConverter("m")
	c.FeedLine(chatChunk(map[string]any{"content": "完整文本<>&"}))
	c.FeedLine(chatChunk(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call-1", "function": map[string]any{"name": "tool", "arguments": `{"value":"完整参数"}`}}}}))
	startSequence := c.sequence
	legacy := c.Finish()
	c.sequence = startSequence // Compare two encoders at the same point in the stream.
	var want []map[string]any
	for _, line := range strings.Split(legacy, "\n") {
		if strings.HasPrefix(line, "data: ") {
			var e map[string]any
			if err := json.Unmarshal([]byte(line[6:]), &e); err != nil {
				t.Fatal(err)
			}
			want = append(want, e)
		}
	}
	var got []map[string]any
	if err := c.FinishEvents(func(event map[string]any) error {
		b, _ := json.Marshal(event)
		var e map[string]any
		json.Unmarshal(b, &e)
		got = append(got, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatal("incremental finishing changed protocol")
	}
	if got[0]["text"] != "完整文本<>&" || got[len(got)-1]["type"] != "response.completed" {
		t.Fatal(got)
	}
	stopped := errors.New("disconnected")
	calls := 0
	if err := c.FinishEvents(func(map[string]any) error { calls++; return stopped }); !errors.Is(err, stopped) || calls != 1 {
		t.Fatal("continued after disconnect", err, calls)
	}
}
func TestNonstreamConvertersSuppressSSEWithoutLosingContent(t *testing.T) {
	for _, c := range []interface {
		SetNonstream()
		FeedLine(string) string
		TextContent() string
		GetNonstreamResponse() map[string]any
	}{NewResponsesStreamConverter("m"), NewAnthropicStreamConverter("m")} {
		c.SetNonstream()
		if s := c.FeedLine(chatChunk(map[string]any{"content": "保留正文", "tool_calls": []any{map[string]any{"index": 0, "id": "call-1", "function": map[string]any{"name": "tool", "arguments": `{"x":1}`}}}})); s != "" {
			t.Fatal("serialized unused SSE")
		}
		if c.TextContent() != "保留正文" {
			t.Fatal("text lost")
		}
		b, _ := json.Marshal(c.GetNonstreamResponse())
		if !strings.Contains(string(b), "tool") || !strings.Contains(string(b), "保留正文") {
			t.Fatal(string(b))
		}
	}
}

func BenchmarkResponsesFinishCopies(b *testing.B) {
	c := NewResponsesStreamConverter("m")
	c.FeedLine(chatChunk(map[string]any{"content": strings.Repeat("x", 8<<20)}))
	b.Run("combined-SSE", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = c.Finish()
		}
	})
	b.Run("incremental-JSON", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = c.FinishEvents(func(event map[string]any) error { return json.NewEncoder(io.Discard).Encode(event) })
		}
	})
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
