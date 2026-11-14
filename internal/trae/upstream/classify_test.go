package upstream

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"work2api/internal/trae/auth"
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

func TestEnterpriseSeatUsageUsesOnlyOwnSeat(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT fixture" {
			t.Error("missing identity")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "get_personal_quota"):
			w.Write([]byte(`{"code":0,"data":{"user_model_quota":{"user_id":"me","seat_pool_currency_quota":"100"}}}`))
		case strings.HasSuffix(r.URL.Path, "get_user_model_usage"):
			w.Write([]byte(`{"data":{"user_usage_list":[{"user_id":"other","in_seat_detail":{"chat_usage_details":[{"total_cost_currency":99}]}},{"user_id":"me","in_seat_detail":{"chat_usage_details":[{"total_cost_currency":"1.25"},{"total_cost_currency":2}]}}]}}`))
		case strings.HasSuffix(r.URL.Path, "get_personal_core_data"):
			w.Write([]byte(`{"Data":{"ResetTime":1800000000000}}`))
		}
	}))
	defer srv.Close()
	c := New()
	c.EnterpriseHost = srv.URL
	q, err := c.EnterpriseUsage(context.Background(), &auth.Auth{AccessToken: "fixture", Variant: "cn"})
	if err != nil || q.Used == nil || *q.Used != 3.25 || q.Limit == nil || *q.Limit != 100 || q.Reset == nil || *q.Reset != 1800000000 || q.Partial || calls != 3 {
		t.Fatal("incorrect seat usage", q, err, calls)
	}
	quota := map[string]any{"user_model_quota": map[string]any{"seat_pool_currency_quota": 100.0}}
	spent := map[string]any{"user_usage_list": []any{map[string]any{"user_id": "a"}, map[string]any{"user_id": "b"}}}
	q, err = parseEnterpriseQuota(quota, spent, nil)
	if err != nil || q.Used != nil {
		t.Fatal("ambiguous seats guessed", q, err)
	}
	q, err = parseEnterpriseQuota(quota, nil, nil)
	if err != nil || q.Used != nil {
		t.Fatal("missing spend became zero", q, err)
	}
	quota["user_model_quota"].(map[string]any)["seat_pool_currency_quota"] = 9999999999.0
	q, err = parseEnterpriseQuota(quota, nil, nil)
	if err != nil || !q.Unlimited || q.Limit != nil {
		t.Fatal("unlimited became budget", q, err)
	}
}

func TestEnterpriseAgentRequestsUseConsumerGateway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "get_model") || strings.Contains(r.URL.Path, "get_config") {
			w.Write([]byte(`{"config_info_list":[]}`))
			return
		}
		w.Write([]byte(`{"config_info_list":[]}`))
	}))
	defer srv.Close()
	c := New()
	c.AgentHost = srv.URL
	c.EnterpriseHost = "http://127.0.0.1:1"
	a := &auth.Auth{AccessToken: "fixture", Variant: "cn", EnterpriseConsole: true}
	if _, err := c.FetchModels(a); err != nil {
		t.Fatal("enterprise catalog did not use consumer gateway", err)
	}
	rc, status, _, _, err := c.ChatPreparedStreamWithHeaders(a, []byte(`{}`))
	if err != nil || status != 200 {
		t.Fatal("enterprise chat did not use consumer gateway", status, err)
	}
	if rc != nil {
		rc.Close()
	}
}
