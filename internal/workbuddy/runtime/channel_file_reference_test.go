package runtime

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

const (
	dsRejectedPreamble  = "Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the workspace root"
	dsRewrittenPreamble = "Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve against the workspace root"
	dsFileParagraph     = dsRejectedPreamble + "; absolute paths identify files or directories on the host. A trailing slash marks a directory: list it when its contents matter."
)

// The rejected window is refused wherever it sits in the system text, including
// in the system prompts DSH keeps in history after a prompt update, so every
// system/developer message is rewritten.
func TestNeutralizeChannelFingerprintsRewritesEverySystemMessage(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": dsFileParagraph}}},
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "content": "working"},
			map[string]any{"role": "system", "content": "Prompt updated.\n\n" + dsFileParagraph},
			map[string]any{"role": "developer", "content": dsFileParagraph},
		},
	}
	got, changed := NeutralizeChannelFingerprints(body)
	if !changed {
		t.Fatal("rejected file-reference paragraph was not rewritten")
	}
	messages := got["messages"].([]any)
	for _, index := range []int{0, 3, 4} {
		raw, _ := json.Marshal(messages[index])
		if strings.Contains(string(raw), dsRejectedPreamble) {
			t.Fatalf("message %d still carries the rejected sentence", index)
		}
		if !strings.Contains(string(raw), dsRewrittenPreamble) {
			t.Fatalf("message %d lost the rewritten sentence", index)
		}
	}
	for _, index := range []int{1, 2} {
		if !reflect.DeepEqual(body["messages"].([]any)[index], messages[index]) {
			t.Fatalf("message %d is conversation content and must stay byte-identical", index)
		}
	}
	// Everything after the matched window rides along untouched.
	first := messages[0].(map[string]any)["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.HasSuffix(first, "; absolute paths identify files or directories on the host. A trailing slash marks a directory: list it when its contents matter.") {
		t.Fatal("rewrote more than the rejected window")
	}
}

// A user, assistant or tool message that quotes the sentence is conversation
// content, not a client declaration.
func TestNeutralizeChannelFingerprintsLeavesConversationContent(t *testing.T) {
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "You are a coding agent."},
			map[string]any{"role": "user", "content": dsFileParagraph},
			map[string]any{"role": "assistant", "content": dsFileParagraph},
			map[string]any{"role": "tool", "tool_call_id": "call1", "content": dsFileParagraph},
		},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "read", "description": dsFileParagraph}}},
	}
	before, _ := json.Marshal(body)
	got, changed := NeutralizeChannelFingerprints(body)
	if changed {
		t.Fatal("rewrote a request that only quotes the paragraph")
	}
	if !reflect.DeepEqual(got, body) {
		t.Fatal("returned a different body for an unchanged request")
	}
	after, _ := json.Marshal(body)
	if string(before) != string(after) {
		t.Fatal("mutated the caller's body")
	}
}

func TestNeutralizeChannelFingerprintsIsIdempotentAndNarrow(t *testing.T) {
	body := map[string]any{"messages": []any{map[string]any{"role": "system", "content": dsFileParagraph}}}
	once, changed := NeutralizeChannelFingerprints(body)
	if !changed {
		t.Fatal("first pass missed the paragraph")
	}
	if _, again := NeutralizeChannelFingerprints(once); again {
		t.Fatal("second pass rewrote the body again")
	}
	for _, text := range []string{
		"Relative paths resolve from the workspace root; absolute paths identify files or directories on the host.",
		"Tokens prefixed with @ are paths the user explicitly referenced. Relative paths resolve from the project root.",
		"Tokens prefixed with @ are paths the user explicitly referenced.\n\nRelative paths resolve from the workspace root.",
		"You are a coding agent powered by the deepseek-v4.1-flash model.",
	} {
		narrow := map[string]any{"messages": []any{map[string]any{"role": "system", "content": text}}}
		if _, changed := NeutralizeChannelFingerprints(narrow); changed {
			t.Fatalf("rewrote text outside the rejected window: %q", text)
		}
	}
}

// Both content shapes reach the gateway: a plain string and an array of text
// blocks. A rewrite that only handles one silently misses half the requests.
func TestNeutralizeChannelFingerprintsHandlesBothContentShapes(t *testing.T) {
	stringBody := map[string]any{"messages": []any{map[string]any{"role": "system", "content": "Intro.\n\n" + dsFileParagraph}}}
	got, changed := NeutralizeChannelFingerprints(stringBody)
	if !changed {
		t.Fatal("string-form system content was not rewritten")
	}
	text := got["messages"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.HasPrefix(text, "Intro.\n\n"+dsRewrittenPreamble) {
		t.Fatal("string content did not receive the rewrite in place")
	}

	blockBody := map[string]any{"messages": []any{map[string]any{"role": "system", "content": []any{
		map[string]any{"type": "text", "text": dsFileParagraph, "cache_control": map[string]any{"type": "ephemeral"}},
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": "AA=="}},
	}}}}
	got, changed = NeutralizeChannelFingerprints(blockBody)
	if !changed {
		t.Fatal("block-form system content was not rewritten")
	}
	blocks := got["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if blocks[0].(map[string]any)["cache_control"] == nil {
		t.Fatal("dropped sibling keys of the rewritten text block")
	}
	if !reflect.DeepEqual(blocks[1], blockBody["messages"].([]any)[0].(map[string]any)["content"].([]any)[1]) {
		t.Fatal("rewrote a non-text block")
	}
}

func TestNeutralizeChannelFingerprintsWithoutMessages(t *testing.T) {
	for _, body := range []map[string]any{{}, {"messages": []any{}}, {"messages": "not-a-list"}} {
		if _, changed := NeutralizeChannelFingerprints(body); changed {
			t.Fatalf("reported a change for %#v", body)
		}
	}
}
