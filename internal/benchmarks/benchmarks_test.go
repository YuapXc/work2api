package benchmarks

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// fakeSettings feeds a fixed key so configured refresh paths can run offline.
type fakeSettings struct{ key string }

func (f fakeSettings) GetSettings() (map[string]string, error) {
	return map[string]string{"aa_api_key": f.key}, nil
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestRefreshPublishesOnlyCompleteCatalog(t *testing.T) {
	s := New(fakeSettings{key: "test-key"})
	failSecondPage := false
	var pages []string
	s.client.Transport = transportFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Path != "/api/v2/language/models/free" || req.Header.Get("x-api-key") != "test-key" {
			t.Fatalf("unexpected AA request to %s", req.URL)
		}
		page := req.URL.Query().Get("page")
		pages = append(pages, page)
		status := http.StatusOK
		var body string
		switch page {
		case "1":
			body = `{"pagination":{"page":1,"has_more":true},"data":[{"id":"glm-53","name":"GLM-5.3","slug":"glm-5-3","evaluations":{"artificial_analysis_intelligence_index":31.7}}]}`
		case "2":
			body = `{"pagination":{"page":2,"has_more":false},"data":[{"id":"ds-flash-max","name":"DeepSeek V4.1 Flash (Max)","slug":"deepseek-v4-1-flash","evaluations":{"artificial_analysis_intelligence_index":39.5,"artificial_analysis_coding_index":null},"performance":{"median_output_tokens_per_second":174,"median_time_to_first_answer_token_seconds":null},"pricing":{"price_1m_input_tokens":0,"price_1m_output_tokens":1.2}}]}`
			if failSecondPage {
				status = http.StatusTooManyRequests
			}
		default:
			t.Fatalf("unexpected page %q", page)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	if err := s.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	checkSecondPage := func() {
		t.Helper()
		got := s.Catalog().Resolve(Target{ID: "workbuddy/deepseek-v4.1-flash", Provider: "workbuddy", Efforts: []string{"max"}, DefaultEffort: "max"}, Binding{})
		if got == nil || got.Intelligence == nil || *got.Intelligence != 39.5 || got.Coding != nil {
			t.Fatalf("unexpected model evaluation: %+v", got)
		}
		if got.AAURL != "https://artificialanalysis.ai/models/deepseek-v4-1-flash" ||
			got.OutputTokensPerSecond == nil || *got.OutputTokensPerSecond != 174 || got.FirstAnswerSeconds != nil ||
			got.InputPerMillion == nil || *got.InputPerMillion != 0 || got.OutputPerMillion == nil || *got.OutputPerMillion != 1.2 {
			t.Fatalf("unexpected AA model URL or measurements: %+v", got.Result)
		}
	}
	checkSecondPage()
	failSecondPage = true
	if err := s.Refresh(t.Context()); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("expected a refresh error for page two, got %v", err)
	}
	checkSecondPage()
	if got := strings.Join(pages, ","); got != "1,2,1,2" {
		t.Fatalf("unexpected AA pages: %s", got)
	}
}

func TestResolveModelVersionAndEffort(t *testing.T) {
	rows := []aaModel{
		{ID: "base", Name: "DeepSeek V4.1 Flash (Max)", Slug: "deepseek-v4-1-flash"},
		{ID: "max", Name: "DeepSeek V4.1 Flash 1017 (Max)", Slug: "deepseek-v4-1-flash-1017"},
		{ID: "high", Name: "DeepSeek V4.1 Flash 1017 (High)", Slug: "deepseek-v4-1-flash-1017-high"},
		{ID: "off", Name: "DeepSeek V4.1 Flash 1017 (Non-reasoning)", Slug: "deepseek-v4-1-flash-1017-non-reasoning"},
	}
	for i, value := range []float64{39.5, 32.6, 28.6} {
		rows[i].Evaluations.Intelligence = &value
	}
	c := newCatalog(rows)
	target := Target{ID: "workbuddy/deepseek-v4.1-flash", Provider: "workbuddy", Name: "DeepSeek V4.1 Flash 1017",
		DefaultEffort: "high", Efforts: []string{"high", "max", "off"}}
	got := c.Resolve(target, Binding{})
	if got == nil || got.Result == nil || *got.Intelligence != 28.6 || len(got.Variants) != 3 ||
		got.Variants[2].Effort != "off" || got.Variants[2].Intelligence != nil {
		t.Fatalf("wrong revision or thinking effort: %+v", got)
	}
	if got.AAURL != modelURL+"deepseek-v4-1-flash-1017-high" || got.Variants[1].AAURL != modelURL+"deepseek-v4-1-flash-1017" {
		t.Fatalf("reasoning variants must link to their individual pages: %+v", got.Variants)
	}
	target.DefaultEffort = ""
	if got := c.Resolve(target, Binding{}); got == nil || got.Result != nil || len(got.Variants) != 3 {
		t.Fatalf("unknown default must leave all scores in details: %+v", got)
	}
	target.ReportedNames = []string{"DeepSeek V4.1 Flash 1017", "DeepSeek V4.1 Flash 1020"}
	if got := c.Resolve(target, Binding{}); got != nil {
		t.Fatalf("mixed versions must suppress the evaluation: %+v", got)
	}
}

func TestBindingRequiresReviewOnIdentityChange(t *testing.T) {
	rows := []aaModel{
		{ID: "high", Name: "Example 1017 (High)", Slug: "example-1017-high"},
		{ID: "max", Name: "Example 1017 (Max)", Slug: "example-1017"},
		{ID: "other", Name: "Example 1020 (High)", Slug: "example-1020-high"},
	}
	c := newCatalog(rows)
	target := Target{ID: "workbuddy/example", Provider: "workbuddy", Name: "Example", Efforts: []string{"high", "max"}}
	if c.Resolve(target, Binding{}) != nil {
		t.Fatal("the bare alias must not pick a numbered revision")
	}
	binding := Binding{EntryID: "high", Identity: target.Identity()}
	if err := c.ValidateBinding(target, binding); err != nil || c.Resolve(target, binding) == nil {
		t.Fatalf("explicit version binding rejected: %v", err)
	}
	for _, bad := range []string{"max", "other"} {
		binding.Efforts = map[string]string{"high": bad}
		if c.ValidateBinding(target, binding) == nil {
			t.Fatalf("expected rejection of wrong variant %s", bad)
		}
	}
	binding.Efforts = nil
	target.Name = "Example 1020"
	if c.BindingState(target, binding) != "review" || c.Resolve(target, binding) != nil {
		t.Fatal("binding must be rechecked after a reported version change")
	}
}

func TestExactNameAndChannelSuffixMatching(t *testing.T) {
	rows := []aaModel{
		{ID: "qwen", Name: "Qwen3.8 Max", Slug: "qwen3-8-max"},
		{ID: "mimo", Name: "MiMo-V2.5", Slug: "mimo-v2-5"},
		{ID: "kimi", Name: "Kimi K2", Slug: "kimi-k2"},
	}
	c := newCatalog(rows)
	for _, tc := range []struct {
		target Target
		want   string
	}{
		{Target{ID: "qoder/qmodel_38max", Provider: "qoder", Name: "Qwen3.8-Max"}, "qwen"},
		{Target{ID: "opencode/mimo-v2.5-free", Provider: "opencode"}, "mimo"},
		{Target{ID: "workbuddy/kimi-k2.8-preview", Provider: "workbuddy"}, ""},
	} {
		got, _ := c.automatic(tc.target)
		if (got == nil && tc.want != "") || (got != nil && got.ID != tc.want) {
			t.Fatalf("unexpected auto-match for %s: %+v", tc.target.ID, got)
		}
	}
}
