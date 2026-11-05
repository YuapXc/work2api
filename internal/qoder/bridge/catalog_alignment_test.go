package bridge

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestContextTiersUseDeterministicDefaults(t *testing.T) {
	var raw map[string]any
	json.Unmarshal([]byte(`{"assistant":[{"key":"m","enable":true,"context_config":{"1M":{"token_count":1000000},"200K":{"token_count":200000,"is_default":true},"400K":{"token_count":400000}}}]}`), &raw)
	for i := 0; i < 30; i++ {
		ms := parseQoderModels(raw)
		if len(ms) != 1 || ms[0].ContextWindow != 200000 || len(ms[0].ContextTiers) != 3 || ms[0].ContextTiers[2].Tokens != 1000000 {
			t.Fatal(ms)
		}
	}
}
func TestResourceFailureRequiresStructuredEvidence(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{{`{"error":{"subCode":"RESOURCE_LIMIT","isQueued":true}}`, "upstream_queued"}, {`{"subCode":"RESOURCE_LIMIT","serviceAvailable":false}`, "resource_limit"}, {`{"code":10605}`, ""}, {`RESOURCE_LIMIT isQueued`, ""}} {
		if got := resourceFailure(tc.raw, 0); got != tc.want {
			t.Fatal(got, tc.want)
		}
	}
}

func TestResourceFailureStatusConsistentAcrossTransports(t *testing.T) {
	for _, tc := range []struct {
		raw    string
		status int
	}{
		{`{"subCode":"RESOURCE_LIMIT","isQueued":true}`, 429},
		{`{"subCode":"RESOURCE_LIMIT","serviceAvailable":false}`, 403},
	} {
		for _, err := range []error{NewUpstreamError(500, tc.raw), NewStreamBusinessError(tc.raw)} {
			if got := ErrorStatus(err); got != tc.status {
				t.Fatalf("status = %d, want %d", got, tc.status)
			}
		}
	}
}

func TestThinkingCatalogUsesAuthoritativeShape(t *testing.T) {
	for _, tc := range []struct {
		raw    string
		levels []string
		off    bool
	}{
		{`{"thinking_config":{"enabled":{"efforts":{"max":{},"low":{}}},"disabled":{}},"reasoning_efforts":["medium"]}`, []string{"low", "max"}, true},
		{`{"thinking_config":{"enabled":{"efforts":["xhigh","minimal","xhigh",4]},"disabled":false}}`, []string{"minimal", "xhigh"}, false},
		{`{"thinking_config":{},"reasoning_efforts":["max"]}`, nil, false},
		{`{"thinking_config":null,"reasoning_efforts":["medium"]}`, []string{"medium"}, false},
	} {
		var model map[string]any
		if err := json.Unmarshal([]byte(tc.raw), &model); err != nil {
			t.Fatal(err)
		}
		got := readThinkingConfig(model)
		if !slices.Equal(got.efforts, tc.levels) || got.canDisable != tc.off {
			t.Fatal(tc.raw, got)
		}
	}
}
