package app

import (
	"strings"
	"testing"
)

func TestCountTokensCJKBaselineAndMissingFields(t *testing.T) {
	text := strings.Repeat("中", 100)
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": text}}}
	baseline := len(text)/4 + 8
	if got := estimateInputTokens(body); got != baseline {
		t.Fatalf("CJK regression: %d != %d", got, baseline)
	}
	body["system"] = strings.Repeat("s", 40)
	if got := estimateInputTokens(body); got != baseline+10 {
		t.Fatalf("system omitted: %d", got)
	}
	body["tools"] = []any{map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object", "description": strings.Repeat("d", 40)}}}
	withTools := estimateInputTokens(body)
	if withTools <= baseline+10 {
		t.Fatal("tool schema omitted")
	}
	body["messages"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "content": []any{map[string]any{"type": "text", "text": text}}}}}}
	if got := estimateInputTokens(body); got != withTools {
		t.Fatalf("nested tool result changed text count: %d != %d", got, withTools)
	}
	image := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "data": strings.Repeat("a", 10000)}}
	if estimateContentBytes(image) != 0 {
		t.Fatal("image base64 must not be counted as text")
	}
}
