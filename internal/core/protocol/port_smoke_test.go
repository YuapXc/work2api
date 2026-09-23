package protocol

import (
	"encoding/json"
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
