package trae

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"work2api/internal/core/protocol"
	"work2api/internal/core/provider"
	"work2api/internal/store"
	"work2api/internal/trae/auth"
	"work2api/internal/trae/upstream"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// Metadata-only opt-in probe: prints field paths, never credential/prompt values.
func TestLiveTraeCatalogMetadata(t *testing.T) {
	path := os.Getenv("WORK2API_TEST_TRAE_CREDENTIAL")
	if path == "" {
		t.Skip("real upstream calls require explicit opt-in")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("cannot read test credential")
	}
	a, err := auth.Parse(raw)
	if err != nil {
		t.Fatal("invalid test credential")
	}
	client := upstream.New()
	base := client.HTTP.Transport.(*http.Transport)
	defer base.CloseIdleConnections()
	client.HTTP.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(req)
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		resp.Body = io.NopCloser(bytes.NewReader(data))
		var doc map[string]any
		if json.Unmarshal(data, &doc) == nil {
			paths := map[string]bool{}
			var walk func(any, string, int)
			walk = func(v any, path string, depth int) {
				if depth > 6 {
					return
				}
				switch val := v.(type) {
				case map[string]any:
					for key, item := range val {
						paths[path+key] = true
						walk(item, path+key+".", depth+1)
					}
				case []any:
					for _, item := range val {
						walk(item, path+"[].", depth+1)
					}
				}
			}
			walk(doc, "", 0)
			keys := []string{}
			for key := range paths {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			t.Logf("%s %s field paths: %v", a.Variant, req.URL.Path, keys)
			if configs, ok := doc["config_info_list"].([]any); ok {
				for _, item := range configs {
					cfg, _ := item.(map[string]any)
					display, _ := cfg["display_config"].(map[string]any)
					meta := map[string]any{"id": cfg["config_name"], "multimodal": display["multimodal"], "model_capability": display["model_capability"], "tool_response_multimodal": display["tool_response_multimodal"], "reasoning_effort_config": cfg["reasoning_effort_config"]}
					meta["fee_model_level"] = display["fee_model_level"]
					meta["commercial_info"] = display["commercial_info"]
					if details, ok := cfg["model_detail_list"].([]any); ok {
						costs := []any{}
						for _, detail := range details {
							d, _ := detail.(map[string]any)
							costs = append(costs, d["model_entitlement_usage_cost"])
						}
						meta["entitlement_usage_cost"] = costs
					}
					encoded, _ := json.Marshal(meta)
					t.Logf("public model metadata: %s", encoded)
				}
			}
		}
		return resp, nil
	})
	if _, err := client.FetchModels(a); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("WORK2API_TEST_TRAE_BILLING_METADATA") == "1" {
		if _, err := client.UserEntUsage(a); err != nil {
			t.Log("quota metadata unavailable")
		}
		if _, err := client.PayStatus(a); err != nil {
			t.Log("plan metadata unavailable")
		}
	}
}

func TestLiveTraeImage(t *testing.T) {
	credentialPath := os.Getenv("WORK2API_TEST_TRAE_CREDENTIAL")
	if credentialPath == "" {
		t.Skip("real upstream calls require explicit opt-in")
	}
	model := os.Getenv("WORK2API_TEST_TRAE_MODEL")
	if model == "" {
		t.Fatal("explicit test model required")
	}
	raw, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal("cannot read test credential")
	}
	dir := t.TempDir()
	db, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r, err := New(dir, db)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if _, err = r.AddAccountByToken(ctx, string(raw), nil); err != nil {
		t.Fatal(err)
	}
	picture := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			picture.SetRGBA(x, y, color.RGBA{R: 17, G: 210, B: 45, A: 255})
		}
	}
	var pngData bytes.Buffer
	if err = png.Encode(&pngData, picture); err != nil {
		t.Fatal(err)
	}
	chat := map[string]any{"model": model, "max_tokens": float64(256), "messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "What is the dominant color in the attached image? Reply with one English color word."}, map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngData.Bytes())}}}}}}
	target := protocol.Anthropic
	if os.Getenv("WORK2API_TEST_TRAE_IMAGE_PROTOCOL") == "responses" {
		target = protocol.Responses
	}
	body, err := protocol.ConvertRequest(protocol.Chat, target, chat)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	report, err := r.Serve(ctx, provider.ServeRequest{Protocol: provider.Protocol(target), Payload: body, Writer: w})
	if err != nil || report.Status != "ok" {
		t.Fatalf("image request failed: status=%s err=%v", report.Status, err)
	}
	reply, err := protocol.ConvertResponse(target, protocol.Chat, w.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Choices []struct{ Message struct{ Content string } }
	}
	if err = json.Unmarshal(reply, &document); err != nil || len(document.Choices) != 1 {
		t.Fatal("invalid image response")
	}
	if !strings.Contains(strings.ToLower(document.Choices[0].Message.Content), "green") {
		t.Fatalf("image not recognized: %q", document.Choices[0].Message.Content)
	}
	t.Logf("%s image recognized correctly; input=%d output=%d", target, report.InputTokens, report.OutputTokens)
}

func TestNativeCallbackCredentialShapes(t *testing.T) {
	for _, query := range []url.Values{
		{"refreshToken": {"test-refresh"}},
		{"userJwt": {`{"RefreshToken":"test-refresh"}`}},
		{"credential": {`{"userJwt":{"RefreshToken":"test-refresh"}}`}},
		{"credential": {url.Values{"userJwt": {`{"RefreshToken":"test-refresh"}`}}.Encode()}},
	} {
		if callbackRefreshToken(query) != "test-refresh" {
			t.Fatal("native callback credential shape rejected")
		}
	}
}

// Opt-in only: copies a locally supplied credential to the test's temporary
// state. Never edits the desktop credential or uses the running gateway DB.
func TestLiveTraeProtocols(t *testing.T) {
	credentialPath := os.Getenv("WORK2API_TEST_TRAE_CREDENTIAL")
	if credentialPath == "" {
		t.Skip("real upstream calls require explicit opt-in")
	}
	model := os.Getenv("WORK2API_TEST_TRAE_MODEL")
	if model == "" {
		t.Fatal("explicit test model required")
	}
	raw, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal("cannot read test credential")
	}
	dir := t.TempDir()
	db, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r, err := New(dir, db)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if _, err = r.AddAccountByToken(ctx, string(raw), nil); err != nil {
		t.Fatal(err)
	}
	call := func(target protocol.Protocol, chat map[string]any) map[string]any {
		t.Helper()
		body, err := protocol.ConvertRequest(protocol.Chat, target, chat)
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		report, err := r.Serve(ctx, provider.ServeRequest{Protocol: provider.Protocol(target), Payload: body, Writer: w})
		if err != nil || report.Status != "ok" {
			t.Fatalf("%s call status=%s err=%v", target, report.Status, err)
		}
		response, err := protocol.ConvertResponse(target, protocol.Chat, w.Body.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		if err = json.Unmarshal(response, &document); err != nil {
			t.Fatal(err)
		}
		t.Logf("%s completed, input=%d output=%d usage_known=%v", target, report.InputTokens, report.OutputTokens, report.TokensKnown != nil && *report.TokensKnown)
		return document
	}
	for _, target := range []protocol.Protocol{protocol.Chat, protocol.Anthropic, protocol.Responses} {
		call(target, map[string]any{"model": model, "max_tokens": float64(128), "messages": []any{map[string]any{"role": "user", "content": "Reply with only OK."}}})
	}
	streamResponse := httptest.NewRecorder()
	streamReport, streamErr := r.Serve(ctx, provider.ServeRequest{Protocol: provider.ProtocolChat, Writer: streamResponse, Payload: map[string]any{"model": model, "stream": true, "max_tokens": float64(128), "messages": []any{map[string]any{"role": "user", "content": "Reply with only OK."}}}})
	if streamErr != nil || streamReport.Status != "ok" || !strings.Contains(streamResponse.Body.String(), "[DONE]") {
		t.Fatalf("live SSE not completed: status=%s err=%v", streamReport.Status, streamErr)
	}
	if _, _, _, err := protocol.CollapseStreamWithUsage(bytes.NewReader(streamResponse.Body.Bytes()), protocol.Chat, model); err != nil {
		t.Fatal("live SSE cannot be decoded", err)
	}
	t.Log("chat SSE completed with native terminal event")
	request := map[string]any{"model": model, "max_tokens": float64(256), "messages": []any{map[string]any{"role": "user", "content": "Call lookup with query hello; do not answer directly."}}, "tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "description": "Retrieve the given test value", "parameters": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}, "required": []any{"query"}}}}}, "tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}}
	reply := call(protocol.Chat, request)
	choices, _ := reply["choices"].([]any)
	if len(choices) != 1 {
		t.Fatal("missing response choice")
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	calls, _ := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatal("upstream did not honor the forced tool request")
	}
	tool, _ := calls[0].(map[string]any)
	if text(tool["id"]) == "" {
		t.Fatal("missing tool call ID")
	}
	request["messages"] = append(request["messages"].([]any), message, map[string]any{"role": "tool", "tool_call_id": tool["id"], "content": "lookup succeeded: hello"})
	request["tool_choice"] = "none"
	call(protocol.Anthropic, request)
}

func TestAdminProjectionIncludesModelsAndStatus(t *testing.T) {
	r := &Runtime{accounts: map[string]*accountState{"test": {Auth: &auth.Auth{Variant: "cn"}, Enabled: true, Models: []upstream.ModelInfo{{ID: "CaseSensitive-ID", Name: "Test model"}}}}}
	data := r.AdminData(context.Background())
	if len(data.Models) != 1 || data.Models[0]["id"] != "trae/casesensitive-id" || data.Status["model_count"] != 1 || data.Status["account_count"] != 1 || data.Status["healthy_count"] != 1 {
		t.Fatalf("incomplete admin projection: models=%v status=%v", data.Models, data.Status)
	}
}

func TestImageCapabilityDoesNotPromoteUnknownModels(t *testing.T) {
	r := &Runtime{accounts: map[string]*accountState{"test": {Auth: &auth.Auth{Variant: "cn"}, Enabled: true, Models: []upstream.ModelInfo{{ID: "deepseek-v4.1-flash"}, {ID: "glm-5.3-flash"}}}}}
	data := r.AdminData(context.Background())
	for _, model := range data.Models {
		if model["vision"] != nil || model["vision_verified"] != false {
			t.Fatal("unknown model advertised as confirmed text or image")
		}
	}
}

func TestCatalogUsesUpstreamMetadata(t *testing.T) {
	response := `{"config_info_list":[{"config_name":"vision","display_config":{"display_name":"Vision","multimodal":true,"model_capability":"reasoning_model","tool_response_multimodal":true},"reasoning_effort_config":{"support_thinking":false},"context_window_tokens":{"dev":200000},"model_detail_list":[{"max_tokens":8192},{"max_tokens":4096}]},{"config_name":"text","display_config":{"display_name":"Text","multimodal":false}},{"config_name":"unknown","display_config":{"display_name":"Unknown"}}]}`
	response = strings.Replace(response, `[{"max_tokens":8192},{"max_tokens":4096}]`, `[{"max_tokens":8192,"model_entitlement_usage_cost":1},{"max_tokens":4096,"model_entitlement_usage_cost":0},{"model_entitlement_usage_cost":{"unknown":1}}]`, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { io.WriteString(w, response) }))
	defer server.Close()
	client := upstream.New()
	client.AgentHost = server.URL
	models, err := client.FetchModels(&auth.Auth{Variant: "cn", AccessToken: "fixture"})
	if err != nil || len(models) != 3 {
		t.Fatalf("models=%v err=%v", models, err)
	}
	r := &Runtime{accounts: map[string]*accountState{"a": {Auth: &auth.Auth{Variant: "cn"}, Enabled: true, Models: models}}}
	data := r.AdminData(context.Background())
	byID := map[string]map[string]any{}
	for _, model := range data.Models {
		byID[text(model["id"])] = model
	}
	if byID["trae/vision"]["vision"] != true || byID["trae/vision"]["thinking_control"] != false || byID["trae/vision"]["max_output_tokens"] != 4096 || byID["trae/text"]["vision"] != false || byID["trae/unknown"]["vision"] != nil {
		t.Fatalf("incorrect discovery projection: %v", byID)
	}
	costs, ok := byID["trae/vision"]["entitlement_usage_cost"].([]float64)
	if !ok || len(costs) != 2 || costs[0] != 1 || costs[1] != 0 {
		t.Fatal("catalog entitlement values lost or unknown structure invented", costs)
	}
	merged := mergeModelInfo(models[0], upstream.ModelInfo{EntitlementCosts: []float64{2}})
	if len(merged.EntitlementCosts) != 3 || len(models[0].EntitlementCosts) != 2 {
		t.Fatal("product cost metadata merge lost values or mutated original")
	}
	response = `{"config_info_list":[]}`
	if models, err = client.FetchModels(&auth.Auth{Variant: "cn"}); err != nil || len(models) != 0 {
		t.Fatal("authoritative empty catalog rejected", err)
	}
	response = `{}`
	if _, err = client.FetchModels(&auth.Auth{Variant: "cn"}); err == nil {
		t.Fatal("missing catalog treated as authoritative empty")
	}
}

func fixtureRuntime(t *testing.T, handler http.HandlerFunc) *Runtime {
	t.Helper()
	dir := t.TempDir()
	db, err := store.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	r, err := New(dir, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	if handler != nil {
		server := httptest.NewTLSServer(handler)
		t.Cleanup(server.Close)
		r.transport = server.Client().Transport.(*http.Transport)
		r.transport.TLSClientConfig.ServerName = "example.com"
		r.transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
		}
	}
	return r
}

func TestCatalogSnapshotAndProductPreference(t *testing.T) {
	r := fixtureRuntime(t, nil)
	id := accountID("fixture-user")
	a := &accountState{Enabled: true, Identities: map[string]*auth.Auth{}, Catalogs: map[string][]upstream.ModelInfo{}}
	r.accounts[id] = a
	for _, product := range []string{"cn", "solo"} {
		identity := &auth.Auth{UID: "fixture-user", Nickname: "Fixture", Variant: product, AccessToken: "fixture", FilePath: filepath.Join(r.dir, id+"-"+product+".json")}
		if err := identity.SaveAtomic(); err != nil {
			t.Fatal(err)
		}
		a.Identities[product] = identity
		a.Auth = identity
		if err := r.commitCatalog(id, product, identity, []upstream.ModelInfo{{ID: product, Name: product}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.db.UpsertProviderAccount(store.ProviderAccount{UID: id, Provider: "trae", Profile: "solo", Auth: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	reloaded, err := New(filepath.Dir(r.dir), r.db)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	if state := reloaded.accounts[id]; state.Auth.Variant != "solo" || len(state.Models) != 2 {
		t.Fatal("product preference or snapshots lost on restart")
	}
	if err = reloaded.commitCatalog(id, "cn", reloaded.accounts[id].Identities["cn"], []upstream.ModelInfo{}); err != nil {
		t.Fatal(err)
	}
	again, err := New(filepath.Dir(r.dir), r.db)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if len(again.accounts[id].Models) != 1 || again.accounts[id].Models[0].ID != "solo" {
		t.Fatal("empty product catalog resurrected old models")
	}
	if err = again.DeleteAccount(id); err != nil {
		t.Fatal(err)
	}
	for _, product := range []string{"cn", "solo"} {
		if _, err := os.Stat(r.catalogPath(id, product)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("deleted snapshot survived")
		}
	}
}

func TestImageRoutingAndAffinityEviction(t *testing.T) {
	yes, no := true, false
	cn, solo := &auth.Auth{Variant: "cn"}, &auth.Auth{Variant: "solo"}
	a := &accountState{Auth: cn, Identities: map[string]*auth.Auth{"cn": cn, "solo": solo}, Catalogs: map[string][]upstream.ModelInfo{"cn": {{ID: "m", Multimodal: &no}}, "solo": {{ID: "m", Multimodal: &yes}}}}
	if identity, _ := a.modelIdentityFor("m", true); identity != solo {
		t.Fatal("image selected unsupported preferred product")
	}
	if identity, _ := a.modelIdentityFor("m", false); identity != cn {
		t.Fatal("text product preference changed")
	}
	r := &Runtime{}
	for i := 0; i < 256; i++ {
		r.rememberAffinity(fmt.Sprint(i), "account")
	}
	r.rememberAffinity("255", "account")
	r.rememberAffinity("new", "account")
	if len(r.affinities) != 256 || r.affinities["255"] != "account" || r.affinities["new"] != "account" || r.affinities["0"] != "" {
		t.Fatal("affinity cache cleared or active session evicted")
	}
}

func TestQuotaRecoveryCoalescesAndPreservesOtherCooldowns(t *testing.T) {
	var packs []upstream.EntitlementPack
	if err := json.Unmarshal([]byte(`[{"is_hide":true,"entitlement_base_info":{"quota":{"credits_limit":100}}},{"status":3,"entitlement_base_info":{"quota":{"credits_limit":100}}}]`), &packs); err != nil {
		t.Fatal(err)
	}
	if pool := upstream.CreditsPoolUsage(packs, true); !pool.Known || pool.Remain != 0 || upstream.SelectActivePack(packs, true) != nil {
		t.Fatal("hidden/cancelled outer pack state must exclude obsolete credits")
	}
	var reads atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	r := fixtureRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		if reads.Add(1) == 1 {
			close(started)
			<-release
		}
		io.WriteString(w, `{"is_credits_billing":true,"user_entitlement_pack_list":[{"display_desc":"Credits","entitlement_base_info":{"product_type":1,"end_time":1793462400,"quota":{"credits_limit":100}},"usage":{"credits_amount":99.75}}]}`)
	})
	id := "fixture"
	identity := &auth.Auth{Variant: "cn", AccessToken: "fixture"}
	a := &accountState{Auth: identity, Enabled: true, Identities: map[string]*auth.Auth{"cn": identity}, Cooldown: time.Now().Add(time.Hour), CooldownKind: upstream.ErrPlanLimit}
	r.accounts[id] = a
	var wait sync.WaitGroup
	errorsOut := make(chan error, 8)
	wait.Add(1)
	go func() { defer wait.Done(); errorsOut <- r.refreshQuota(context.Background(), id, false) }()
	<-started
	for i := 0; i < 7; i++ {
		wait.Add(1)
		go func() { defer wait.Done(); errorsOut <- r.refreshQuota(context.Background(), id, false) }()
	}
	close(release)
	wait.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	if reads.Load() != 1 || a.Remaining == nil || *a.Remaining != 0.25 || !a.Cooldown.IsZero() || len(a.CreditPackages) != 1 || a.Expiry == nil {
		t.Fatal("recovery or coalescing failed")
	}
	if remain, ok := a.CreditPackages[0]["remain"].(*float64); !ok || remain == nil || *remain != 0.25 {
		t.Fatal("credit package must retain fractional credit balance")
	}
	a.CooldownKind, a.Cooldown = upstream.ErrSoftRate, time.Now().Add(time.Hour)
	if err := r.refreshQuota(context.Background(), id, true); err != nil || a.Cooldown.IsZero() {
		t.Fatal("quota refresh incorrectly cleared rate-limit cooldown")
	}
}

func TestWarmLoadsQuotaWithoutPeriodicRefresh(t *testing.T) {
	var reads atomic.Int32
	r := fixtureRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != upstream.EpEntUsage {
			http.Error(w, "catalog unavailable", http.StatusServiceUnavailable)
			return
		}
		reads.Add(1)
		io.WriteString(w, `{"is_credits_billing":true,"user_entitlement_pack_list":[{"entitlement_base_info":{"quota":{"credits_limit":100}},"usage":{"credits_amount":25}}]}`)
	})
	identity := &auth.Auth{Variant: "cn", AccessToken: "fixture"}
	a := &accountState{Auth: identity, Enabled: true, Identities: map[string]*auth.Auth{"cn": identity}}
	r.accounts["enabled"] = a
	r.accounts["disabled"] = &accountState{Auth: identity, Enabled: false}
	r.Warm(context.Background())
	if reads.Load() != 1 || a.Remaining == nil || *a.Remaining != 75 || a.QuotaAt.IsZero() || a.QuotaFailed || r.accounts["disabled"].Remaining != nil {
		t.Fatal("startup failed to hydrate enabled account quota independently of catalog")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Warm(ctx)
	if reads.Load() != 1 {
		t.Fatal("cancelled startup queried upstream")
	}
}

func TestHTTPAndStreamRejectionsRemainRequestLevel(t *testing.T) {
	for _, body := range []string{`{"code": 4001}`, `{"biz_code":4001}`} {
		if upstream.Classify(400, body) != upstream.ErrModelUnavailable {
			t.Fatal("spaced business code not classified")
		}
	}
	err := eventError(&upstream.SOLOEvent{ErrorCode: 4001, ErrorMessage: "context window exceeded"})
	var rejected *upstream.SOLOStreamError
	if !errors.As(err, &rejected) || rejected.Kind() != upstream.ErrInputTooLarge {
		t.Fatal("stream message lost before classification")
	}
	r := &Runtime{accounts: map[string]*accountState{"a": {}}}
	r.cool("a", rejected.Kind())
	r.cool("a", upstream.ErrModelUnavailable)
	if r.accounts["a"].Failures != 0 || !r.accounts["a"].Cooldown.IsZero() {
		t.Fatal("request error punished healthy account")
	}
	if retryAfter("120", time.Now()) != 2*time.Minute || retryAfter("-1", time.Now()) != 0 || retryAfter("999999999999", time.Now()) != 24*time.Hour {
		t.Fatal("retry-after bounds incorrect")
	}
	if strings.Contains(rejectionError(rejected.Kind(), 400, 4001).Error(), "context window exceeded") {
		t.Fatal("raw upstream message echoed")
	}
}

func TestServeRejectionBoundaries(t *testing.T) {
	for _, scenario := range []string{"http-rate", "sse-input", "after-output", "retry-success", "revoked"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			r := fixtureRuntime(t, func(w http.ResponseWriter, req *http.Request) {
				n := calls.Add(1)
				switch scenario {
				case "http-rate":
					w.Header().Set("Retry-After", "120")
					w.WriteHeader(429)
					io.WriteString(w, `{"code":429}`)
				case "retry-success":
					if n == 1 {
						w.Header().Set("Retry-After", "120")
						w.WriteHeader(429)
						return
					}
					io.WriteString(w, "event: output\ndata: {\"response\":\"OK\"}\n\nevent: done\ndata: {}\n\n")
				case "sse-input":
					io.WriteString(w, "event: error\ndata: {\"code\":4001,\"msg\":\"context window exceeded: private content\"}\n\n")
				case "after-output":
					io.WriteString(w, "event: output\ndata: {\"response\":\"partial\"}\n\nevent: error\ndata: {\"code\":4008,\"message\":\"quota\"}\n\n")
				default:
					t.Error("revoked caller reached upstream")
				}
			})
			for _, uid := range []string{"one", "two"} {
				identity := &auth.Auth{UID: uid, Variant: "cn", AccessToken: "fixture"}
				r.accounts[accountID(uid)] = &accountState{Auth: identity, Enabled: true, Identities: map[string]*auth.Auth{"cn": identity}, Catalogs: map[string][]upstream.ModelInfo{"cn": {{ID: "m", Name: "Model"}}}}
			}
			if scenario != "retry-success" {
				delete(r.accounts, accountID("two"))
			}
			ctx := context.Background()
			if scenario == "revoked" {
				ctx = provider.WithInvocation(ctx, provider.Invocation{Check: func(context.Context, provider.AccountRef, string) error {
					return provider.NewAPIError(403, "revoked", "model_not_allowed")
				}})
			}
			writer := httptest.NewRecorder()
			report, err := r.Serve(ctx, provider.ServeRequest{Writer: writer, Protocol: provider.ProtocolChat, Payload: map[string]any{"model": "trae/m", "stream": true, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}})
			switch scenario {
			case "http-rate":
				if err == nil || calls.Load() != 1 || writer.Header().Get("Retry-After") != "120" || r.accounts[accountID("one")].CooldownKind != upstream.ErrSoftRate {
					t.Fatalf("rate boundary failed: %v", err)
				}
			case "sse-input":
				if err == nil || calls.Load() != 1 || r.accounts[accountID("one")].Failures != 0 || strings.Contains(err.Error(), "private content") {
					t.Fatalf("request boundary failed: %v", err)
				}
			case "after-output":
				if err == nil || calls.Load() != 1 || report.Status != "incomplete" {
					t.Fatalf("output replayed: %v %+v", err, report)
				}
			case "retry-success":
				if err != nil || calls.Load() != 2 || report.Status != "ok" || writer.Header().Get("Retry-After") != "" {
					t.Fatalf("bounded failover failed: %v %+v", err, report)
				}
			case "revoked":
				if err == nil || calls.Load() != 0 {
					t.Fatal("revocation bypassed")
				}
			}
		})
	}
}

// These fixtures exercise the native event boundary without an account or an
// upstream request. EOF must not turn a partial reply into a successful one.
func TestNativeStreamCompletion(t *testing.T) {
	for _, target := range []protocol.Protocol{protocol.Chat, protocol.Anthropic, protocol.Responses} {
		t.Run(string(target), func(t *testing.T) {
			input := "event: output\ndata: {\"reasoning_content\":\"think\"}\n\nevent: extra_info\ndata: {\"reasoning_content\":\"think more\"}\n\nevent: output\ndata: {\"response\":\"answer\"}\n\nevent: token_usage\ndata: {\"prompt_tokens\":12,\"completion_tokens\":7,\"total_tokens\":19}\n\nevent: done\ndata: {}\n\n"
			events := newEvents(strings.NewReader(input))
			first, err := events.next()
			if err != nil {
				t.Fatal(err)
			}
			var stream bytes.Buffer
			if err = normalizeStream(&stream, events, first, "trae/test", nil); err != nil {
				t.Fatal(err)
			}
			reply, usage, known, err := protocol.CollapseStreamWithUsage(&stream, protocol.Chat, "trae/test")
			if err != nil || !known || usage.Input != 12 || usage.Output != 7 || usage.FinishReason != "stop" {
				t.Fatalf("usage=%+v known=%v err=%v", usage, known, err)
			}
			if !bytes.Contains(reply, []byte(`"reasoning_content":"think more"`)) {
				t.Fatalf("reasoning duplicated or lost: %s", reply)
			}
			converted, err := protocol.ConvertResponse(protocol.Chat, target, reply)
			if err != nil || !json.Valid(converted) || !bytes.Contains(converted, []byte("answer")) {
				t.Fatalf("invalid %s response: %s (%v)", target, converted, err)
			}
		})
	}
}

// Codex aborts the turn with "failed to parse ResponseCompleted: missing field
// `cached_tokens`" when the Responses usage object carries an empty
// input_tokens_details, which is what an upstream without cache reporting
// produced. Trae never reports cache or reasoning counters, so the detail
// objects must be omitted rather than sent empty.
func TestResponsesUsageOmitsUnreportedDetails(t *testing.T) {
	input := "event: output\ndata: {\"response\":\"answer\"}\n\nevent: token_usage\ndata: {\"prompt_tokens\":12,\"completion_tokens\":7,\"total_tokens\":19}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n"
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			r := fixtureRuntime(t, func(w http.ResponseWriter, req *http.Request) {
				io.WriteString(w, input)
			})
			identity := &auth.Auth{UID: "fixture-user", Variant: "cn", AccessToken: "fixture"}
			r.accounts[accountID("fixture-user")] = &accountState{Auth: identity, Enabled: true, Identities: map[string]*auth.Auth{"cn": identity}, Catalogs: map[string][]upstream.ModelInfo{"cn": {{ID: "m", Name: "Model"}}}}
			w := httptest.NewRecorder()
			report, err := r.Serve(context.Background(), provider.ServeRequest{
				Writer:   w,
				Protocol: provider.ProtocolResponses,
				Payload:  map[string]any{"model": "trae/m", "stream": stream, "input": []any{map[string]any{"role": "user", "content": "hi"}}},
			})
			if err != nil || report.Status != "ok" {
				t.Fatalf("status=%s err=%v", report.Status, err)
			}
			for _, usage := range responsesUsages(t, w.Body.Bytes(), stream) {
				if _, present := usage["input_tokens_details"]; present {
					t.Fatalf("empty input_tokens_details breaks Codex: %v", usage)
				}
				if _, present := usage["output_tokens_details"]; present {
					t.Fatalf("unreported reasoning detail emitted: %v", usage)
				}
				if usage["input_tokens"] != float64(12) || usage["output_tokens"] != float64(7) || usage["total_tokens"] != float64(19) {
					t.Fatalf("token totals changed: %v", usage)
				}
			}
		})
	}
}

// responsesUsages collects every usage object carried by a Responses reply,
// streaming or not. Only the terminal events carry usage, so earlier stream
// events are skipped.
func responsesUsages(t *testing.T, body []byte, stream bool) []map[string]any {
	t.Helper()
	var usages []map[string]any
	collect := func(document map[string]any) {
		usage, ok := document["usage"].(map[string]any)
		if !ok {
			return
		}
		usages = append(usages, usage)
	}
	if !stream {
		var document map[string]any
		if err := json.Unmarshal(body, &document); err != nil {
			t.Fatal(err)
		}
		collect(document)
		if len(usages) == 0 {
			t.Fatalf("missing usage in %s", body)
		}
		return usages
	}
	for _, frame := range strings.Split(string(body), "\n\n") {
		data := ""
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if data == "" || data == "[DONE]" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatal(err)
		}
		if response, ok := event["response"].(map[string]any); ok {
			collect(response)
		}
	}
	if len(usages) == 0 {
		t.Fatalf("no usage-bearing events in %s", body)
	}
	return usages
}

// The bridge re-prefixes the upstream id per downstream protocol. A Chat-style
// "chatcmpl-" prefix minted upstream used to leak into Responses ids as
// "resp_chatcmpl-trae-...", so every protocol must see exactly one prefix.
func TestResponseIDsCarryOneProtocolPrefix(t *testing.T) {
	for _, target := range []protocol.Protocol{protocol.Chat, protocol.Anthropic, protocol.Responses} {
		t.Run(string(target), func(t *testing.T) {
			r := fixtureRuntime(t, func(w http.ResponseWriter, req *http.Request) {
				io.WriteString(w, "event: output\ndata: {\"response\":\"answer\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
			})
			identity := &auth.Auth{UID: "fixture-user", Variant: "cn", AccessToken: "fixture"}
			r.accounts[accountID("fixture-user")] = &accountState{Auth: identity, Enabled: true, Identities: map[string]*auth.Auth{"cn": identity}, Catalogs: map[string][]upstream.ModelInfo{"cn": {{ID: "m", Name: "Model"}}}}
			payload := map[string]any{"model": "trae/m", "stream": false}
			if target == protocol.Responses {
				payload["input"] = []any{map[string]any{"role": "user", "content": "hi"}}
			} else {
				payload["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
			}
			w := httptest.NewRecorder()
			report, err := r.Serve(context.Background(), provider.ServeRequest{Writer: w, Protocol: provider.Protocol(target), Payload: payload})
			if err != nil || report.Status != "ok" {
				t.Fatalf("status=%s err=%v", report.Status, err)
			}
			var document map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &document); err != nil {
				t.Fatal(err)
			}
			id, _ := document["id"].(string)
			want := map[protocol.Protocol]string{protocol.Chat: "chatcmpl_", protocol.Anthropic: "msg_", protocol.Responses: "resp_"}[target]
			if !strings.HasPrefix(id, want) {
				t.Fatalf("%s id %q lacks the %s prefix", target, id, want)
			}
			if strings.Count(id, "chatcmpl") != strings.Count(want, "chatcmpl") || strings.Count(id, "resp_") != strings.Count(want, "resp_") || strings.Count(id, "msg_") != strings.Count(want, "msg_") {
				t.Fatalf("%s id %q carries a foreign protocol prefix", target, id)
			}
		})
	}
}

// The upstream interleaves progress_notice heartbeats whose data payload is a
// bare JSON string instead of an object. Parsing used to fail hard on those
// frames, which aborted an otherwise healthy turn with a 502. The heartbeat
// must be skipped while the answer that follows it still reaches the client.
func TestNativeProgressNoticeHeartbeatIsSkipped(t *testing.T) {
	input := "event: progress_notice\ndata: \"Processing_1791806915\"\n\nevent: output\ndata: {\"response\":\"answer\"}\n\nevent: token_usage\ndata: {\"prompt_tokens\":9,\"completion_tokens\":2,\"total_tokens\":11}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n"
	events := newEvents(strings.NewReader(input))
	first, err := events.next()
	if err != nil {
		t.Fatalf("heartbeat aborted the stream: %v", err)
	}
	if first.Event != "progress_notice" || first.Response != "" {
		t.Fatalf("heartbeat parsed as output: %+v", first)
	}
	var stream bytes.Buffer
	if err = normalizeStream(&stream, events, first, "trae/test", nil); err != nil {
		t.Fatal(err)
	}
	reply, usage, known, err := protocol.CollapseStreamWithUsage(&stream, protocol.Chat, "trae/test")
	if err != nil || !known || usage.Input != 9 || usage.Output != 2 {
		t.Fatalf("usage=%+v known=%v err=%v", usage, known, err)
	}
	if !bytes.Contains(reply, []byte("answer")) {
		t.Fatalf("answer lost after heartbeat: %s", reply)
	}
}

// Corrupt JSON is still a hard failure; only well-formed non-object frames are
// skipped.
func TestNativeMalformedFrameStillFails(t *testing.T) {
	events := newEvents(strings.NewReader("event: progress_notice\ndata: {not-json}\n\n"))
	if _, err := events.next(); err == nil {
		t.Fatal("malformed frame accepted")
	}
}
func TestNativeStreamRequiresTerminalEvent(t *testing.T) {
	events := newEvents(strings.NewReader("event: output\ndata: {\"response\":\"partial\"}\n\n"))
	first, err := events.next()
	if err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	if err = normalizeStream(&stream, events, first, "trae/test", nil); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("partial response accepted: %v", err)
	}
	if strings.Contains(stream.String(), "[DONE]") {
		t.Fatal("fabricated completion")
	}
}

func TestNativeToolHistoryAndOutput(t *testing.T) {
	request := []byte(`{"model":"CaseSensitive-ID","messages":[{"role":"system","content":"skills metadata"},{"role":"assistant","tool_calls":[{"id":"call-a","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call-a","content":"result"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
	prepared := upstream.PrepareBodyResolved(request, "cn", "CaseSensitive-ID")
	var body map[string]any
	if err := json.Unmarshal(prepared, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "CaseSensitive-ID" || len(body["messages"].([]any)) != 3 || !bytes.Contains(prepared, []byte("skills metadata")) || !bytes.Contains(prepared, []byte("call-a")) {
		t.Fatalf("input history changed: %s", prepared)
	}
	events := newEvents(strings.NewReader("event: done\ndata: {}\n\n"))
	first := &upstream.SOLOEvent{Event: "output", ToolCalls: json.RawMessage(`[{"id":"call-a","function_call":{"name":"lookup","arguments":{"query":"value"}}}]`)}
	var stream bytes.Buffer
	if err := normalizeStream(&stream, events, first, "trae/test", nil); err != nil {
		t.Fatal(err)
	}
	reply, _, _, err := protocol.CollapseStreamWithUsage(&stream, protocol.Chat, "trae/test")
	if err != nil || !bytes.Contains(reply, []byte(`"finish_reason":"tool_calls"`)) || !bytes.Contains(reply, []byte("call-a")) {
		t.Fatalf("tool output lost: %s (%v)", reply, err)
	}
	for _, target := range []protocol.Protocol{protocol.Anthropic, protocol.Responses} {
		converted, err := protocol.ConvertResponse(protocol.Chat, target, reply)
		if err != nil || !bytes.Contains(converted, []byte("call-a")) || !bytes.Contains(converted, []byte("lookup")) {
			t.Fatalf("tool conversion failed: %s (%v)", converted, err)
		}
	}
}
