package chatstream

import (
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"work2api/internal/core/provider"
)

func TestMessagesStreamingCaptureDoesNotTruncateWireOrNonstream(t *testing.T) {
	text := strings.Repeat("正文", 20)
	thinking := strings.Repeat("思考", 20)
	args := `{"value":"` + text + `"}`
	chunk := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{
		"content": text, "reasoning_content": thinking,
		"tool_calls": []any{map[string]any{"index": 0, "id": "call_1", "function": map[string]any{"name": "test", "arguments": args}}},
	}}}}
	for _, p := range []provider.LogCapturePolicy{{Enabled: true, Limit: 7}, {Enabled: false, Limit: 7}, {Enabled: true, Limit: 0}} {
		c := NewAnthropicStreamConverter("m")
		c.SetStreamingLogPolicy(p)
		wire := c.FeedChunk(chunk)
		for _, s := range []string{text, thinking, args} {
			encoded, _ := json.Marshal(s)
			if !strings.Contains(wire, string(encoded)) {
				t.Fatal("full wire delta missing")
			}
		}
		if !p.Enabled {
			if c.TextContent() != "" || c.Reasoning() != "" || c.toolLogBytes != 0 {
				t.Fatal("disabled capture retained content")
			}
		} else if p.Limit > 0 {
			if len(c.TextContent()) > p.Limit || len(c.Reasoning()) > p.Limit || c.toolLogBytes > p.Limit {
				t.Fatal("capture exceeded policy")
			}
		} else if c.TextContent() != text || c.Reasoning() != thinking || c.toolUses[0].args.String() != args {
			t.Fatal("unlimited setting lost content")
		}
		full := NewAnthropicStreamConverter("m")
		full.SetStreamingLogPolicy(p)
		full.SetNonstream()
		full.FeedChunk(chunk)
		if full.TextContent() != text || full.Reasoning() != thinking || full.toolUses[0].args.String() != args {
			t.Fatal("nonstream content truncated")
		}
		content := full.GetNonstreamResponse()["content"].([]any)
		if len(content) != 3 || content[2].(map[string]any)["input"].(map[string]any)["value"] != text {
			t.Fatal("nonstream tool input changed")
		}
	}
}

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
	if _, known := c.GetNonstreamResponse()["usage"].(map[string]any)["output_tokens_details"]; known {
		t.Fatal("unknown reasoning usage must omit the optional object for Codex compatibility")
	}
	if strings.Contains(c.Finish(), `"output_tokens_details"`) {
		t.Fatal("stream completion contains an invalid empty reasoning usage object")
	}
	c.FeedLine(`data: {"usage":{"completion_tokens_details":{"reasoning_tokens":0}}}`)
	if c.GetNonstreamResponse()["usage"].(map[string]any)["output_tokens_details"].(map[string]any)["reasoning_tokens"] != 0 {
		t.Fatal("explicit upstream zero reasoning tokens must be retained")
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

func chatChunk(delta map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}},
	})
	return "data: " + string(b) + "\n\n"
}
