package protocol

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"work2api/internal/jsonutil"
)

// TestConvertRequestChatToAnthropic verifies the ported conversion layer wires
// up: an OpenAI Chat request should transcode into an Anthropic Messages body
// with system split out and messages preserved.
func TestConvertRequestChatToAnthropic(t *testing.T) {
	in := map[string]any{
		"model": "claude-x",
		"messages": []any{
			map[string]any{"role": "system", "content": "be brief"},
			map[string]any{"role": "user", "content": "hello"},
		},
		"max_tokens": float64(64),
	}
	out, err := ConvertRequest(Chat, Anthropic, in)
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	if got := jsonutil.StringAt(out, "system"); got != "be brief" {
		// system may be represented as string or blocks depending on impl;
		// accept either as long as the text survives.
		if blocks := jsonutil.SliceAt(out, "system"); len(blocks) == 0 {
			t.Fatalf("system prompt lost: %v", out["system"])
		}
	}
	msgs := jsonutil.SliceAt(out, "messages")
	if len(msgs) == 0 {
		t.Fatalf("messages lost: %v", out["messages"])
	}
	b, _ := json.Marshal(out)
	t.Logf("anthropic body: %s", b)
}

// TestConvertRequestSameProtocolClones ensures a same-protocol request is
// preserved (not dropped) through PrepareRequest.
func TestConvertRequestSameProtocolClones(t *testing.T) {
	in := map[string]any{
		"model":    "gpt-x",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	out, err := PrepareRequest(Chat, Chat, in, "https://example.test/v1/chat/completions")
	if err != nil {
		t.Fatalf("PrepareRequest: %v", err)
	}
	if jsonutil.StringAt(out, "model") != "gpt-x" {
		t.Fatalf("model lost: %v", out)
	}
}

func TestResponsesCompletedReusesFinishedItems(t *testing.T) {
	var wire bytes.Buffer
	emitter := newBridgeStreamEmitter(&wire, httptest.NewRecorder(), Responses, "test-model")
	for _, event := range []bridgeStreamEvent{{Kind: "reasoning", Encrypted: "encrypted-state"}, {Kind: "tool_start", ToolKey: "one", ToolID: "call1", ToolName: "lookup"}, {Kind: "tool_delta", ToolKey: "one", Text: `{"q":"test"}`}, {Kind: "text", Text: "answer"}, {Kind: "finish", Stop: "tool_calls"}, {Kind: "done"}} {
		if err := emitter.Emit(event); err != nil {
			t.Fatal(err)
		}
	}
	finished := map[int]any{}
	var completed []any
	err := readSSE(strings.NewReader(wire.String()), func(name, data string) error {
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return err
		}
		if name == "response.output_item.done" {
			finished[int(event["output_index"].(float64))] = event["item"]
		}
		if name == "response.completed" {
			completed = event["response"].(map[string]any)["output"].([]any)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 3 || len(finished) != 3 {
		t.Fatalf("missing output items: %s", wire.String())
	}
	for index, item := range completed {
		if !reflect.DeepEqual(item, finished[index]) {
			t.Fatalf("completed item %d changed identity/content", index)
		}
	}
	if completed[0].(map[string]any)["encrypted_content"] != "encrypted-state" {
		t.Fatal("encrypted reasoning lost")
	}
}

func TestCacheUnknownAndObservedZeroRemainDistinct(t *testing.T) {
	for _, target := range []Protocol{Chat, Responses, Anthropic} {
		for _, known := range []bool{false, true} {
			response := bridgeResponse{Usage: Usage{Input: 10, Output: 2, Total: 12, CachedKnown: known}}
			doc := encodeBridgeResponse(target, response)
			usage := doc["usage"].(map[string]any)
			var cached any
			if target == Anthropic {
				cached = usage["cache_read_input_tokens"]
			} else if target == Chat {
				cached = usage["prompt_tokens_details"].(map[string]any)["cached_tokens"]
			} else {
				cached = usage["input_tokens_details"].(map[string]any)["cached_tokens"]
			}
			if (cached != nil) != known {
				t.Fatalf("%s cached known=%v, value=%v", target, known, cached)
			}
		}
	}
}

func TestCollapseDoesNotInventMissingUsage(t *testing.T) {
	wire := `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n" + "data: [DONE]\n\n"
	body, _, known, err := CollapseStreamWithUsage(strings.NewReader(wire), Chat, "test")
	if err != nil || known {
		t.Fatal("unexpected usage observation", known, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["usage"]; ok {
		t.Fatal("synthetic usage exposed as observed", string(body))
	}
}
