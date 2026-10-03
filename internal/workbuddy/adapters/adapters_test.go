package adapters

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

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
	legacy := c.Finish()
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
