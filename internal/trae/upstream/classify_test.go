package upstream

import (
	"encoding/json"
	"testing"
)

func TestClassifyLongContext5xxAsInputTooLarge(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
	}{
		{502, `{"error":"input too large"}`},
		{500, `{"message":"context length exceeds limit"}`},
		{503, `{"message":"上下文过长，请压缩后重试"}`},
		{413, `too large`},
	} {
		if got := Classify(tc.status, tc.body); got != ErrInputTooLarge {
			t.Fatalf("status=%d body=%q: got %v, want ErrInputTooLarge", tc.status, tc.body, got)
		}
	}
	if got := Classify(502, `{"error":"bad gateway"}`); got != ErrServer {
		t.Fatalf("plain 502: got %v, want ErrServer", got)
	}
}

func TestClassifyPreservesExplicitStatusAndParameterErrors(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   ErrKind
	}{
		{429, `{"message":"token limit per minute exceeded"}`, ErrSoftRate},
		{429, `{"message":"context window rate limit"}`, ErrSoftRate},
		{401, `{"message":"maximum context token invalid"}`, ErrSessionDead},
		{503, `{"message":"queue exceeds capacity"}`, ErrServer},
		{400, `{"message":"max_tokens must be positive"}`, ErrClient},
		{400, `{"message":"queue wait too long"}`, ErrClient},
		{404, `{"message":"context length model not found"}`, ErrNotFound},
		{402, `{"message":"token limit reached"}`, ErrPlanLimit},
		{500, `{"message":"maximum context length exceeded"}`, ErrInputTooLarge},
	}
	for _, tc := range cases {
		if got := Classify(tc.status, tc.body); got != tc.want {
			t.Fatalf("%d %s: %v != %v", tc.status, tc.body, got, tc.want)
		}
	}
	for _, code := range []int64{401, 429} {
		e := &SOLOStreamError{Code: code, Msg: "context length exceeded"}
		want := ErrSessionDead
		if code == 429 {
			want = ErrSoftRate
		}
		if e.Kind() != want {
			t.Fatal("stream status lost", e.Kind())
		}
	}
}

func TestDynamicReasoningNativeMapping(t *testing.T) {
	model := ModelInfo{ReasoningOptions: []string{"light", "high", "extra_high"}, DefaultReasoning: "high"}
	for _, tc := range []struct{ effort, want string }{{"low", "light"}, {"high", "high"}, {"xhigh", "extra_high"}, {"max", "extra_high"}, {"medium", ""}, {"unknown", ""}, {"", ""}} {
		raw := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
		var obj map[string]any
		_ = json.Unmarshal(raw, &obj)
		if tc.effort != "" {
			obj["reasoning_effort"] = tc.effort
		}
		raw, _ = json.Marshal(obj)
		result, _ := PrepareBodyResolvedWithLimits(raw, "cn", "m", model)
		var body map[string]any
		_ = json.Unmarshal(result, &body)
		got, _ := body["reasoning_effort"].(string)
		if got != tc.want {
			t.Fatalf("%s -> %s want %s, body=%s", tc.effort, got, tc.want, result)
		}
	}
	model.ReasoningOptions = append(model.ReasoningOptions, "max")
	if got, ok := model.NativeEffort("max"); !ok || got != "max" {
		t.Fatal("native max overwritten", got, ok)
	}
}
