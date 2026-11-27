package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

func TestEnterpriseAgentRequestsUseConsoleGateway(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "get_model") || strings.Contains(r.URL.Path, "get_config") {
			w.Write([]byte(`{"config_info_list":[]}`))
			return
		}
		w.Write([]byte(`{"config_info_list":[]}`))
	}))
	defer srv.Close()
	c := New()
	c.EnterpriseHost = srv.URL
	c.AgentHost = "http://127.0.0.1:1"
	a := &auth.Auth{AccessToken: "fixture", Variant: "cn", EnterpriseConsole: true}
	if _, err := c.FetchModels(a); err != nil {
		t.Fatal("enterprise catalog did not use console gateway", err)
	}
	rc, status, _, _, err := c.ChatPreparedStreamWithHeaders(a, []byte(`{}`), a.UsesEnterpriseConsole())
	if err != nil || status != 200 {
		t.Fatal("enterprise chat did not use console gateway", status, err)
	}
	if rc != nil {
		rc.Close()
	}
}

func TestEnterpriseCatalogControlsWireIdentityAndLimits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"config_info_list":[
		{"config_name":"personal","display_config":{"display_name":"Personal"}},
		{"config_name":"seat","config_source":"2","is_custom_model":true,"display_name":"Seat","multimodal":false,"display_config":{"multimodal":true},"model_detail_list":[{"model_name":"variant","max_tokens":128},{"model_name":"native__dev","provider":"vendor","max_tokens":8192,"prompt_max_tokens":90000}]},
		{"config_name":"disabled","config_source":2,"status":false}, {"config_name":"off","config_source":2,"config_switch":false},
		{"config_name":"internal","config_source":2,"is_invisible_to_user":true},{"config_name":"embedding","config_source":2,"usage":"embed"},
		{"config_name":"local","config_source":3,"display_config":{"display_name":"local"}}]}`)
	}))
	defer server.Close()
	c := New()
	c.AgentHost, c.EnterpriseHost = server.URL, server.URL
	for _, enterprise := range []bool{false, true} {
		models, err := c.FetchModels(&auth.Auth{AccessToken: "fixture", Variant: "cn", EnterpriseConsole: enterprise})
		if err != nil {
			t.Fatal(err)
		}
		if len(models) == 0 || models[0].ID != "personal" || models[0].Enterprise != nil {
			t.Fatal(models)
		}
		if !enterprise {
			if len(models) != 1 {
				t.Fatal("personal catalog includes seat", models)
			}
			continue
		}
		if len(models) != 2 || models[1].ID != "seat" || models[1].Name != "Seat" || models[1].MaxTokens != 8192 {
			t.Fatal(models)
		}
		for _, tc := range []struct {
			request string
			want    int
		}{{`{}`, 8192}, {`{"max_tokens":256}`, 256}, {`{"max_tokens":32,"max_completion_tokens":40000}`, 8192}} {
			raw, limits := PrepareBodyResolvedWithLimits([]byte(tc.request), "cn", "seat", models[1])
			var body map[string]any
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			hyper, _ := body["enterprise_custom_hyper_params"].(map[string]any)
			if body["model_name"] != "native__dev" || body["provider"] != "vendor" || body["config_source"] != 2.0 || body["is_custom_model"] != true || limits["max_tokens"] != tc.want || body["max_tokens"] != float64(tc.want) || hyper["max_tokens"] != body["max_tokens"] || hyper["prompt_max_tokens"] != 90000.0 || hyper["multimodal"] != false {
				t.Fatal(body, limits)
			}
		}
		raw, _ := PrepareBodyResolvedWithLimits([]byte(`{"provider":"client","config_source":2,"enterprise_custom_hyper_params":{"max_tokens":7}}`), "cn", "personal", models[0])
		if strings.Contains(string(raw), "provider") || strings.Contains(string(raw), "config_source") || strings.Contains(string(raw), "hyper_params") {
			t.Fatal(string(raw))
		}
	}
}

func TestPreparedEnterpriseRequestKeepsItsEndpoint(t *testing.T) {
	consoleCalls, consumerCalls := 0, 0
	console := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		consoleCalls++
		body, _ := io.ReadAll(req.Body)
		if !strings.Contains(string(body), `"config_source":2`) {
			t.Error("enterprise parameters missing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: done\ndata: {}\n\n")
	}))
	defer console.Close()
	consumer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		consumerCalls++
		w.WriteHeader(400)
	}))
	defer consumer.Close()
	client := New()
	client.EnterpriseHost, client.AgentHost = console.URL, consumer.URL
	a := &auth.Auth{Variant: "cn", AccessToken: "fixture", EnterpriseConsole: true}
	body, _ := PrepareBodyResolvedWithLimits([]byte(`{}`), "cn", "seat", ModelInfo{ID: "seat", ConsoleRoute: true, Enterprise: &EnterpriseModel{ModelName: "native"}, MaxTokens: 128})
	if changed, err := a.FallbackToConsumer(); !changed || err != nil {
		t.Fatal(changed, err)
	}
	rc, status, _, _, err := client.ChatPreparedStreamWithHeaders(a, body, true)
	if rc != nil {
		rc.Close()
	}
	if err != nil || status != 200 || consoleCalls != 1 || consumerCalls != 0 {
		t.Fatal("prepared request changed billing endpoint", consoleCalls, consumerCalls, status, err)
	}
}

func TestEnterpriseDirectory4245Fallback(t *testing.T) {
	for _, status := range []int{200, 400} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					w.WriteHeader(status)
					io.WriteString(w, `{"code":4245}`)
					return
				}
				io.WriteString(w, `{"config_info_list":[]}`)
			}))
			defer server.Close()
			c := New()
			c.EnterpriseHost = server.URL
			c.AgentHost = server.URL
			a := &auth.Auth{AccessToken: "fixture", Variant: "cn", EnterpriseConsole: true}
			if _, err := c.FetchModels(a); err != nil || calls != 2 || a.UsesEnterpriseConsole() {
				t.Fatal("directory fallback", calls, err)
			}
			if _, err := c.FetchModels(a); err != nil || calls != 3 {
				t.Fatal("repeated console request", calls, err)
			}
		})
	}
}

func TestModelPlanRejectionAndMaxModeCatalog(t *testing.T) {
	for _, raw := range []string{`{"code":1005,"extra":{"plan":2}}`, `{"code":1005,"extra":"{\"plan\":0}"}`} {
		if Classify(400, raw) != ErrModelPlan || Classify(401, raw) != ErrSessionDead {
			t.Fatal(raw)
		}
		event, err := ParseSOLOLine("error", raw)
		if err != nil || (&SOLOStreamError{Code: event.ErrorCode, PlanPresent: event.PlanPresent}).Kind() != ErrModelPlan {
			t.Fatal(event, err)
		}
	}
	if Classify(400, `{"code":1005,"extra":{"plan":null}}`) != ErrPlanLimit {
		t.Fatal("unreported plan invented")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"config_info_list":[{"config_name":"wide","display_config":{"display_name":"Wide","max_mode":true},"context_window_tokens":{"dev":116000,"max":1000000},"model_detail_list":[{"max_tokens":64000}]}]}`)
	}))
	defer srv.Close()
	client := New()
	client.AgentHost = srv.URL
	models, err := client.FetchModels(&auth.Auth{Variant: "cn", AccessToken: "fixture"})
	if err != nil || len(models) != 1 {
		t.Fatal(models, err)
	}
	for _, enabled := range []bool{false, true} {
		m := models[0].WithMaxMode(enabled)
		want := int64(116000)
		if enabled {
			want = 1000000
		}
		if m.EffectiveContextWindow() != want {
			t.Fatal(m)
		}
		raw, _ := PrepareBodyResolvedWithLimits([]byte(`{"messages":[]}`), "cn", "wide", m)
		var body map[string]any
		json.Unmarshal(raw, &body)
		if (body["is_max_mode"] == true) != enabled {
			t.Fatal(string(raw))
		}
	}
	cfg := directConfig("custom_openai_compatible", []byte(`{"status":true,"auth_type":"0","ak":"private-key","base_url":"https://model.example/v1/chat/completions"}`))
	if cfg == nil {
		t.Fatal("valid direct catalog discarded")
	}
	secretModel := ModelInfo{Enterprise: &EnterpriseModel{ModelName: "custom", Direct: cfg}}
	raw, _ := json.Marshal(secretModel)
	if strings.Contains(string(raw), "private-key") || strings.Contains(string(raw), "model.example") {
		t.Fatal("direct secret escaped catalog", string(raw))
	}
	for _, raw := range []string{`{"status":false,"auth_type":0,"ak":"key","base_url":"https://model.example/chat/completions"}`, `{"status":true,"auth_type":1,"ak":"key","base_url":"https://model.example/chat/completions"}`, `{"status":true,"auth_type":0,"ak":"key","base_url":"https://user:pass@model.example/chat/completions"}`} {
		if directConfig("custom_openai_compatible", []byte(raw)) != nil {
			t.Fatal(raw)
		}
	}
}

func TestEnterpriseTwinIDsPreservePhysicalConfig(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		preset := `{"config_name":"kimi","config_source":1,"display_config":{"display_name":"Preset"},"model_detail_list":[{"model_name":"preset__dev"}]}`
		tenant := `{"config_name":"kimi","config_source":2,"provider":"aliyuncs","display_name":"Tenant","model_detail_list":[{"model_name":"tenant","max_tokens":4096}]}`
		if reverse {
			preset, tenant = tenant, preset
		}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"config_info_list":[%s,%s,{"config_name":"AI/model","config_source":2,"provider":"vendor","display_name":"Other","model_detail_list":[{"model_name":"other"}]}]}`, preset, tenant)
		}))
		c := New()
		c.EnterpriseHost = server.URL
		models, err := c.FetchModels(&auth.Auth{AccessToken: "fixture", Variant: "cn", EnterpriseConsole: true})
		server.Close()
		if err != nil || len(models) != 3 {
			t.Fatal(models, err)
		}
		var twin, other ModelInfo
		for _, m := range models {
			if m.ID == "aliyuncs//kimi" {
				twin = m
			}
			if m.ID == "AI/model" {
				other = m
			}
		}
		if twin.Enterprise == nil || !other.MatchesID("vendor//AI/model") {
			t.Fatal("enterprise model hidden or renamed", models)
		}
		raw, _ := PrepareBodyResolvedWithLimits([]byte(`{}`), "cn", twin.ID, twin)
		var wire map[string]any
		json.Unmarshal(raw, &wire)
		if wire["config_name"] != "kimi" || wire["model"] != "kimi" || wire["config_source"] != float64(2) || wire["model_name"] != "tenant" {
			t.Fatal(string(raw))
		}
	}
	if Classify(400, `{"code":4026}`) != ErrContextWindow || (&SOLOStreamError{Code: 4026}).Kind() != ErrContextWindow {
		t.Fatal("4026 classification lost")
	}
}
