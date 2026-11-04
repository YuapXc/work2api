package protocol

import (
	"encoding/json"
	"testing"
	"work2api/internal/jsonutil"
)

func TestBlankClientEffortDoesNotBlockConfiguredDefault(t *testing.T) {
	for _, p := range []Protocol{Chat, Responses} {
		for _, blank := range []string{"", "  "} {
			body := map[string]any{"reasoning_effort": blank, "reasoning": map[string]any{"effort": blank}}
			ForcedEffort(p, body, "high")
			value := body["reasoning_effort"]
			if p == Responses {
				value = jsonutil.AnyAt(body, "reasoning", "effort")
			}
			if value != "high" {
				t.Fatalf("%s blank effort blocked default: %v", p, body)
			}
		}
	}
}

func TestThinkingControlTypesAreNotEffortLevels(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		budget int
		effort string
	}{
		{"enabled", 2048, "low"}, {"enabled", 8192, "high"}, {"enabled", 32768, "max"},
	} {
		thinking, output := anthropicThinking(map[string]any{"type": tc.kind, "budget_tokens": tc.budget})
		if output["effort"] != tc.effort || thinking.(map[string]any)["budget_tokens"] != float64(tc.budget) {
			t.Fatalf("budget or effort lost: %v %v", thinking, output)
		}
	}
	thinking, output := anthropicThinking(map[string]any{"type": "adaptive"})
	if thinking.(map[string]any)["type"] != "adaptive" || output != nil {
		t.Fatal("adaptive became invalid effort", thinking, output)
	}
	if got := responsesReasoning(map[string]any{"type": "disabled"}); got != nil {
		t.Fatal("Anthropic disabled object leaked to Responses", got)
	}
	for _, kind := range []string{"enabled", "adaptive"} {
		got := responsesReasoning(map[string]any{"type": kind, "budget_tokens": 2048}).(map[string]any)
		if got["effort"] != "low" || got["type"] != nil {
			t.Fatal("invalid Responses reasoning", got)
		}
	}
}

func TestBooleanReasoningControlsPreserveNativeIntent(t *testing.T) {
	for _, tc := range []struct {
		raw                string
		enabled, specified bool
	}{
		{`{}`, false, false}, {`{"reasoning_effort":null}`, false, false},
		{`{"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}}`, true, true},
		{`{"thinking":{"type":"disabled"},"output_config":{"effort":"high"}}`, false, true},
		{`{"thinking":{"type":"enabled","budget_tokens":2048}}`, true, true},
		{`{"reasoning":{"effort":"high"}}`, true, true},
		{`{"reasoning_effort":"off"}`, false, true},
		{`{"output_config":{"effort":"max"}}`, true, true},
		{`{"reasoning_effort":"unknown"}`, false, false},
	} {
		var input map[string]any
		if err := json.Unmarshal([]byte(tc.raw), &input); err != nil {
			t.Fatal(err)
		}
		enabled, specified := ReasoningEnabled(input)
		if enabled != tc.enabled || specified != tc.specified {
			t.Fatal(tc.raw, enabled, specified)
		}
	}
}
