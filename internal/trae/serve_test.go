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
	"syscall"
	"testing"
	"time"

	"work2api/internal/core/protocol"
	"work2api/internal/core/provider"
	"work2api/internal/store"
	"work2api/internal/streamwatch"
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
	r.accounts["test"].Cooldown = time.Now().Add(time.Hour)
	r.accounts["test"].CooldownKind = upstream.ErrSoftRate
	data = r.AdminData(context.Background())
	if data.Accounts[0]["cooldown_kind"] != "soft_rate" || data.Accounts[0]["cooldown_until"] == nil || data.Resources[0].Status != "cooling" {
		t.Fatal("lost cooldown evidence", data.Accounts[0])
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
	response := `{"config_info_list":[{"config_name":"vision","display_config":{"display_name":"Vision","multimodal":true,"model_capability":"reasoning_model","tool_response_multimodal":true},"reasoning_effort_config":{"support_thinking":false,"options":["light","extra_high","high","extra_high",{"unknown":true}],"default_level":"high"},"context_window_tokens":{"dev":200000},"model_detail_list":[{"max_tokens":8192},{"max_tokens":4096}]},{"config_name":"text","display_config":{"display_name":"Text","multimodal":false}},{"config_name":"unknown","display_config":{"display_name":"Unknown"}}]}`
	response = strings.Replace(response, `[{"max_tokens":8192},{"max_tokens":4096}]`, `[{"max_tokens":8192,"model_entitlement_usage_cost":1},{"max_tokens":4096,"model_entitlement_usage_cost":0},{"model_entitlement_usage_cost":{"unknown":1}}]`, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { io.WriteString(w, response) }))
	defer server.Close()
	client := upstream.New()
	client.AgentHost = server.URL
	models, err := client.FetchModels(&auth.Auth{Variant: "cn", AccessToken: "fixture"})
	if err != nil || len(models) != 3 {
		t.Fatalf("models=%v err=%v", models, err)
	}
	if strings.Join(models[0].ReasoningOptions, ",") != "light,extra_high,high" || models[0].DefaultReasoning != "high" {
		t.Fatal("dynamic options lost", models[0])
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

func TestCredentialRefreshDoesNotBlockOtherAccountsAndPersists(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	r := fixtureRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		io.WriteString(w, `{"Result":{"Token":"rotated","RefreshToken":"rotated-refresh","TokenExpireDuration":3600}}`)
	})
	register := func(uid string, expires int64) *auth.Auth {
		a := &auth.Auth{UID: uid, Variant: "cn", AccessToken: "fixture", RefreshToken: "refresh", ExpiresAt: expires, FilePath: filepath.Join(r.dir, accountID(uid)+"-cn.json")}
		r.accounts[accountID(uid)] = &accountState{Auth: a, Enabled: true, Identities: map[string]*auth.Auth{"cn": a}}
		return a
	}
	expired := register("expired", time.Now().Add(-time.Minute).Unix())
	fresh := register("fresh", time.Now().Add(time.Hour).Unix())
	done := make(chan error, 1)
	go func() { done <- r.ensure(context.Background(), expired) }()
	<-entered
	adminDone := make(chan struct{})
	go func() { r.AdminData(context.Background()); close(adminDone) }()
	select {
	case <-adminDone:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("admin data waited for credential network refresh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	other := make(chan error, 1)
	go func() { other <- r.ensure(ctx, fresh) }()
	select {
	case err := <-other:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		close(release)
		t.Fatal("fresh account waited for unrelated refresh")
	}
	waitCtx, waitCancel := context.WithCancel(context.Background())
	waitCancel()
	if err := r.ensure(waitCtx, expired); !errors.Is(err, context.Canceled) {
		close(release)
		t.Fatal("waiter cancellation ignored", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("rotation retried", calls.Load())
	}
	raw, err := os.ReadFile(expired.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := auth.Parse(raw)
	if err != nil || loaded.JWT() != "rotated" || loaded.RefreshTokenValue() != "rotated-refresh" {
		t.Fatal("rotated chain not saved", err)
	}
	if err := r.ensure(context.Background(), expired); err != nil || calls.Load() != 1 {
		t.Fatal("fresh token unnecessarily refreshed", err, calls.Load())
	}
}

func TestCredentialRotationSaveRetryAndRevocation(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(fmt.Sprintf("revoke=%v", revoke), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			r := fixtureRuntime(t, func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				close(entered)
				<-release
				io.WriteString(w, `{"Result":{"Token":"rotated","RefreshToken":"rotated-refresh","TokenExpireDuration":3600}}`)
			})
			a := &auth.Auth{UID: "save-test", Variant: "cn", AccessToken: "fixture", RefreshToken: "refresh", ExpiresAt: 1, FilePath: filepath.Join(r.dir, "missing", "credentials.json")}
			id := accountID(a.UID)
			r.accounts[id] = &accountState{Auth: a, Enabled: true, Identities: map[string]*auth.Auth{"cn": a}}
			done := make(chan error, 1)
			go func() { done <- r.ensure(context.Background(), a) }()
			<-entered
			if revoke {
				if err := r.DeleteAccount(id); err != nil {
					close(release)
					t.Fatal(err)
				}
			}
			close(release)
			if err := <-done; err == nil {
				t.Fatal("failed publication reported success")
			}
			if revoke {
				if _, err := os.Stat(a.FilePath); !os.IsNotExist(err) {
					t.Fatal("removed credential resurrected")
				}
				return
			}
			a.Lock()
			a.FilePath = filepath.Join(r.dir, "saved.json")
			a.Unlock()
			if err := r.ensure(context.Background(), a); err != nil {
				t.Fatal("fresh unsaved token did not retry persistence", err)
			}
			if calls.Load() != 1 {
				t.Fatal("saving required another upstream rotation", calls.Load())
			}
			if _, err := os.Stat(a.FilePath); err != nil {
				t.Fatal(err)
			}
		})
	}
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
	id := accountID("fixture")
	identity := &auth.Auth{UID: "fixture", Variant: "cn", AccessToken: "fixture"}
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
	identity := &auth.Auth{UID: "enabled", Variant: "cn", AccessToken: "fixture"}
	a := &accountState{Auth: identity, Enabled: true, Identities: map[string]*auth.Auth{"cn": identity}}
	r.accounts[accountID("enabled")] = a
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
	for _, scenario := range []string{"http-rate", "sse-input", "after-output", "retry-success", "revoked", "dial-failover", "ambiguous-send"} {
		t.Run(scenario, func(t *testing.T) {
			var calls atomic.Int32
			r := fixtureRuntime(t, func(w http.ResponseWriter, req *http.Request) {
				n := calls.Add(1)
				switch scenario {
				case "dial-failover":
					io.WriteString(w, "event: output\ndata: {\"response\":\"OK\"}\n\nevent: done\ndata: {}\n\n")
				case "ambiguous-send":
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
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
			if scenario != "retry-success" && scenario != "dial-failover" && scenario != "ambiguous-send" {
				delete(r.accounts, accountID("two"))
			}
			var dials atomic.Int32
			if scenario == "dial-failover" {
				dial := r.transport.DialContext
				r.transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
					if dials.Add(1) == 1 {
						return nil, &net.OpError{Op: "dial", Net: network, Err: syscall.ECONNREFUSED}
					}
					return dial(ctx, network, addr)
				}
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
			case "dial-failover":
				if err != nil || dials.Load() != 2 || calls.Load() != 1 || report.Status != "ok" {
					t.Fatalf("confirmed pre-send failure did not fall back safely: %v %+v", err, report)
				}
			case "ambiguous-send":
				if err == nil || calls.Load() != 1 {
					t.Fatalf("possibly submitted request was replayed: %v calls=%d", err, calls.Load())
				}
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
// produced. An upstream may omit cache or reasoning counters, so the detail
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
func TestPreparedTraeLimitsMatchWireAndBoundMultilineEvents(t *testing.T) {
	input := []byte(`{"model":"m","max_tokens":77,"messages":[{"role":"user","content":"hello"}]}`)
	wire, limits := upstream.PrepareBodyResolvedWithLimits(input, "solo", "m")
	var body map[string]any
	if json.Unmarshal(wire, &body) != nil || limits["max_tokens"] != 77 || body["max_tokens"] != float64(77) {
		t.Fatal("limits differ from wire", limits)
	}
	if string(input) != `{"model":"m","max_tokens":77,"messages":[{"role":"user","content":"hello"}]}` {
		t.Fatal("caller body mutated")
	}
	line := "data: " + strings.Repeat("x", 1024) + "\n"
	events := newEvents(strings.NewReader(strings.Repeat(line, streamwatch.DefaultMaxEventBytes/len(line)+1)))
	if _, err := events.next(); !errors.Is(err, streamwatch.ErrSSEEventTooLarge) {
		t.Fatal("multiline event unbounded", err)
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

func TestFirstEventDiagnosticsKeepCategoriesWithoutRawContent(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{errTraeEmptyReply, "empty_reply"}, {io.ErrUnexpectedEOF, "unexpected_eof"}, {context.Canceled, "cancelled"},
		{context.DeadlineExceeded, "timeout"}, {&json.SyntaxError{}, "invalid_json"}, {&json.UnmarshalTypeError{}, "invalid_event_shape"},
		{errors.New("private upstream request body"), "stream_read_error"},
	} {
		if got := firstEventFailure(tc.err); got != tc.want {
			t.Fatal(got, tc.want)
		}
	}
}

func TestTraeNullAssistantNormalizationStaysAtBoundary(t *testing.T) {
	input := []byte(`{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"echo","arguments":"{}"}}]}]}`)
	wire := upstream.PrepareBodyResolved(input, "solo", "m")
	var body map[string]any
	if err := json.Unmarshal(wire, &body); err != nil {
		t.Fatal(err)
	}
	message := body["messages"].([]any)[0].(map[string]any)
	blocks, ok := message["content"].([]any)
	if !ok || len(blocks) != 1 || blocks[0].(map[string]any)["text"] != "" {
		t.Fatal("null assistant not normalized", message)
	}
	if !bytes.Contains(wire, []byte(`"function_call"`)) || !bytes.Contains(input, []byte(`"content":null`)) {
		t.Fatal("tool history/input changed", string(wire))
	}
}

func TestCatalogPricingAndOfferParsing(t *testing.T) {
	contact := map[string]any{"consumption_rate": map[string]any{"enable": true, "data": map[string]any{"rate": 0.06}}, "activity_discount": map[string]any{"enable": true, "data": map[string]any{"current": map[string]any{"discount_type": "off_peak", "before_consumption_rate": 0.12, "consumption_rate": 0.06}, "off_peak": map[string]any{"time_zone": "Asia/Shanghai", "time_windows": []any{map[string]any{"weekdays": []int{6, 7}, "start_minute": 0, "end_minute": 1440}, map[string]any{"weekdays": []int{8}, "start_minute": -1, "end_minute": 0}}}}}}
	current := map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(current) }))
	defer server.Close()
	client := upstream.New()
	client.AgentHost = server.URL
	defer client.HTTP.CloseIdleConnections()
	for _, encoded := range []bool{false, true} {
		var value any = contact
		display := any(map[string]any{"display_name": "Pricing", "builder": map[string]any{}})
		if encoded {
			b, _ := json.Marshal(contact)
			value = string(b)
			b, _ = json.Marshal(display)
			display = string(b)
		}
		current = map[string]any{"config_info_list": []any{map[string]any{"config_name": "pricing", "display_config": display, "display_contact_config": value}, map[string]any{"config_name": "hidden", "display_config": map[string]any{"display_name": "Hidden", "is_invisible_to_user": true}}, map[string]any{"config_name": "custom", "display_config": map[string]any{"display_name": "Custom", "is_custom_model": true}}, map[string]any{"config_name": "builder", "display_config": map[string]any{"display_name": "Builder", "builder": map[string]any{"id": "fixture"}}}, map[string]any{"config_name": "unknown", "display_config": map[string]any{"display_name": "Unknown"}}}}
		models, err := client.FetchModels(&auth.Auth{Variant: "cn"})
		if err != nil || len(models) != 2 {
			t.Fatalf("filtered models=%d err=%v", len(models), err)
		}
		q := models[0].Pricing[0]
		if q.Rate == nil || *q.Rate != 0.06 || q.Product != "cn" || len(q.Promotions) != 1 {
			t.Fatal("lost rate or applied discount twice", q)
		}
		p := q.Promotions[0]
		if p.Label != "错峰优惠" || p.Timezone != "Asia/Shanghai" || len(p.Weekly) != 1 || p.Weekly[0].End != "24:00" || len(p.Weekly[0].Weekdays) != 2 {
			t.Fatal("invalid weekly offer", p)
		}
		if models[1].Pricing[0].Rate != nil {
			t.Fatal("unknown rate became zero")
		}
	}
	for _, rate := range []any{-1, nil, "unknown", true} {
		contact["consumption_rate"] = map[string]any{"enable": true, "data": map[string]any{"rate": rate}}
		current = map[string]any{"config_info_list": []any{map[string]any{"config_name": "m", "display_config": map[string]any{"display_name": "M"}, "display_contact_config": contact}}}
		models, err := client.FetchModels(&auth.Auth{Variant: "solo"})
		if err != nil || len(models) != 1 || models[0].Pricing[0].Rate != nil {
			t.Fatal("invalid optional pricing affected model or became a price")
		}
	}
}

func TestPricingScopesProductsAndStaleSnapshots(t *testing.T) {
	rateA, rateB := 0.06, 1.5
	offer := upstream.ModelPromotion{Label: "错峰优惠", Region: "cn", Timezone: "Asia/Shanghai"}
	q := upstream.ModelPrice{Product: "cn", Rate: &rateA, Promotions: []upstream.ModelPromotion{offer}}
	model := upstream.ModelInfo{ID: "m", Name: "M", Pricing: []upstream.ModelPrice{q}}
	r := &Runtime{accounts: map[string]*accountState{
		"allowed": {Auth: &auth.Auth{Variant: "cn"}, Enabled: true, Models: []upstream.ModelInfo{model}, CatalogTimes: map[string]time.Time{"cn": time.Now()}},
		"private": {Auth: &auth.Auth{Variant: "solo"}, Enabled: true, Models: []upstream.ModelInfo{{ID: "m", Pricing: []upstream.ModelPrice{{Product: "solo", Rate: &rateB}}}}, CatalogTimes: map[string]time.Time{"solo": time.Now()}},
	}}
	scoped := r.ModelMetadataForAccounts("trae/m", map[string]bool{"allowed": true})
	values := scoped["price_factors"].([]upstream.ModelPrice)
	if len(values) != 1 || *values[0].Rate != 0.06 {
		t.Fatal("private price leaked into scoped catalog")
	}
	empty := r.ModelMetadataForAccounts("trae/m", nil)
	if len(empty["price_factors"].([]upstream.ModelPrice)) != 0 {
		t.Fatal("missing scope disclosed pricing")
	}
	merged := mergeModelInfo(model, r.accounts["private"].Models[0])
	if len(merged.Pricing) != 2 || len(model.Pricing) != 1 {
		t.Fatal("merge lost product price or mutated source")
	}
	raw, err := json.Marshal(catalogSnapshot{Version: 1, Models: []upstream.ModelInfo{merged}})
	if err != nil {
		t.Fatal(err)
	}
	var restored catalogSnapshot
	if json.Unmarshal(raw, &restored) != nil || len(restored.Models[0].Pricing) != 2 {
		t.Fatal("snapshot lost per-product pricing")
	}
	partial := pricingMetadata([]upstream.ModelPrice{q, {Product: "cn"}})
	if !partial["promotions"].([]upstream.ModelPromotion)[0].Partial {
		t.Fatal("partial offer advertised for every account")
	}
	r.accounts["allowed"].CatalogErrors = map[string]bool{"cn": true}
	stale := r.ModelMetadataForAccounts("trae/m", map[string]bool{"allowed": true})
	if stale["pricing_stale"] != true || len(stale["promotions"].([]upstream.ModelPromotion)) != 0 || model.Pricing[0].Stale {
		t.Fatal("stale offer shown or source mutated")
	}
}

func TestLiveTraePricing(t *testing.T) {
	if os.Getenv("WORK2API_LIVE_TRAE_PRICING") != "1" {
		t.Skip("explicit read-only catalog pricing verification")
	}
	accounts, err := auth.LoadDir(filepath.Join("..", "..", "data", "trae"))
	if err != nil {
		t.Fatal("cannot load local credentials")
	}
	client := upstream.New()
	client.HTTP.Timeout = 30 * time.Second
	defer client.HTTP.CloseIdleConnections()
	seen := map[string]bool{}
	for _, a := range accounts {
		if seen[a.Variant] {
			continue
		}
		models, err := client.FetchModels(a)
		if err != nil {
			t.Logf("product=%s catalog unavailable", a.Variant)
			continue
		}
		known, offers := 0, 0
		for _, m := range models {
			for _, q := range m.Pricing {
				if q.Rate != nil {
					known++
				}
				offers += len(q.Promotions)
				if m.ID == "deepseek-v4.1-flash" || m.ID == "kimi-k3" || m.ID == "qwen3.8-max" {
					if q.Rate == nil {
						t.Fatalf("product=%s public model=%s missing rate", a.Variant, m.ID)
					}
					t.Logf("product=%s model=%s rate=%g promotions=%d", a.Variant, m.ID, *q.Rate, len(q.Promotions))
					for _, p := range q.Promotions {
						t.Logf("product=%s label=%s timezone=%s weekly_windows=%d", a.Variant, p.Label, p.Timezone, len(p.Weekly))
					}
				}
			}
		}
		if known == 0 {
			t.Fatal("no upstream rates decoded")
		}
		seen[a.Variant] = true
		t.Logf("product=%s visible_models=%d known_rates=%d offers=%d", a.Variant, len(models), known, offers)
	}
	if !seen["cn"] || !seen["solo"] {
		t.Fatal("IDE and Work/SOLO were not both verified")
	}
}

func TestMaintenanceAuthFailureStopsCheckinForDisabledAccount(t *testing.T) {
	var calls atomic.Int32
	r := fixtureRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		w.WriteHeader(401)
		io.WriteString(w, `{"message":"expired"}`)
	})
	a := &auth.Auth{UID: "maintenance", Variant: "cn", AccessToken: "fixture", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour).Unix(), FilePath: filepath.Join(r.dir, "maintenance.json")}
	state := &accountState{Auth: a, Enabled: false, Identities: map[string]*auth.Auth{"cn": a}}
	r.accounts[accountID(a.UID)] = state
	for i := 0; i < 3; i++ {
		if _, err := r.AdminCheckin(context.Background()); err != nil {
			t.Fatal(err)
		}
		_ = r.refreshQuota(context.Background(), accountID(a.UID), true)
	}
	if calls.Load() != 1 {
		t.Fatalf("failed auth still triggered downstream maintenance: %d", calls.Load())
	}
	if state.Enabled {
		t.Fatal("maintenance changed administrator dispatch switch")
	}
}

func TestQuotaUnauthorizedPreservesRecoverySignal(t *testing.T) {
	r := fixtureRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(401)
		io.WriteString(w, `{"message":"private upstream detail"}`)
	})
	a := &auth.Auth{UID: "quota401", Variant: "cn", AccessToken: "fixture", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	id := accountID(a.UID)
	r.accounts[id] = &accountState{Auth: a, Enabled: true, Identities: map[string]*auth.Auth{"cn": a}}
	err := r.refreshQuota(context.Background(), id, true)
	var remote *upstream.Error
	if !errors.As(err, &remote) || remote.Status != 401 || strings.Contains(err.Error(), "private upstream detail") {
		t.Fatal("quota error lost type or leaked remote message", err)
	}
	state := r.authGuard().State(id+":cn:auth", a.MaintenanceIdentity())
	if state.Kind != "authorization_unverified" || state.NeedsLogin {
		t.Fatal("quota rejection did not request revalidation", state)
	}
}

func TestDisabledTraeAccountStillChecksStatus(t *testing.T) {
	var calls atomic.Int32
	r := fixtureRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		io.WriteString(w, `{"code":0,"data":{"enable":false}}`)
	})
	a := &auth.Auth{UID: "disabled-maintenance", Variant: "cn", AccessToken: "fixture", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	state := &accountState{Auth: a, Enabled: false, Identities: map[string]*auth.Auth{"cn": a}}
	r.accounts[accountID(a.UID)] = state
	if _, err := r.AdminCheckin(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("disabled account skipped maintenance or repeated auth: %d", calls.Load())
	}
	if state.Enabled {
		t.Fatal("maintenance reenabled account")
	}
}

func TestConfirmedTraeExpiryExcludesOnlyFailedProduct(t *testing.T) {
	r := fixtureRuntime(t, nil)
	cn := &auth.Auth{UID: "same-user", Variant: "cn", AccessToken: "cn"}
	solo := &auth.Auth{UID: "same-user", Variant: "solo", AccessToken: "solo"}
	a := &accountState{Auth: cn, Enabled: true, Identities: map[string]*auth.Auth{"cn": cn, "solo": solo}, Catalogs: map[string][]upstream.ModelInfo{"cn": {{ID: "m"}}, "solo": {{ID: "m"}}}}
	id := accountID(cn.UID)
	r.accounts[id] = a
	g := r.authGuard()
	fp := cn.MaintenanceIdentity()
	g.State(id+":cn:auth", fp)
	g.Observe(id+":cn:auth", fp, "authorization_expired", true)
	r.mu.Lock()
	picked, _ := a.modelIdentityFor("m", false, r.identityAvailableLocked)
	r.mu.Unlock()
	if picked != solo {
		t.Fatal("failed primary identity blocked or displaced valid alternate")
	}
	fp = solo.MaintenanceIdentity()
	g.State(id+":solo:auth", fp)
	g.Observe(id+":solo:auth", fp, "authorization_expired", true)
	req := provider.ServeRequest{}
	if _, err := r.selectAccountFor(req, "m", "sticky", nil, nil, false); err == nil {
		t.Fatal("all-invalid account was still selected")
	}
	if err := r.ensure(context.Background(), cn); !errors.Is(err, provider.ErrMaintenanceLogin) {
		t.Fatal("known-invalid inference attempted refresh", err)
	}
	if !a.Enabled {
		t.Fatal("auth failure changed manual enablement")
	}
}

func TestReadOnlyCheckPreservesDisabledAccountAndScope(t *testing.T) {
	var gets, posts atomic.Int32
	r := fixtureRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Method == "GET" {
			gets.Add(1)
		} else {
			posts.Add(1)
		}
		io.WriteString(w, `{"code":0,"data":{"models":[]}}`)
	})
	a := &auth.Auth{UID: "check-disabled", Variant: "cn", AccessToken: "fixture", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	state := &accountState{Auth: a, Enabled: false, Identities: map[string]*auth.Auth{"cn": a}, Catalogs: map[string][]upstream.ModelInfo{}, CatalogTimes: map[string]time.Time{"cn": time.Now()}, CatalogErrors: map[string]bool{}}
	id := accountID(a.UID)
	r.accounts[id] = state
	result := r.CheckAccount(context.Background(), id, false)
	if result.Status != "valid" || gets.Load() != 0 || posts.Load() != 0 || state.Enabled {
		t.Fatal("fresh read-only evidence was not reused or switch changed", result)
	}
	fp := a.MaintenanceIdentity()
	g := r.authGuard()
	g.State(id+":cn:auth", fp)
	g.Observe(id+":cn:auth", fp, "authorization_expired", true)
	result = r.CheckAccount(context.Background(), id, true)
	if result.Status != "invalid" || gets.Load() != 0 || posts.Load() != 0 {
		t.Fatal("known expiry bypassed maintenance gate", result)
	}
}

func TestQuotaPublicationDoesNotWaitForCredentialRotation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	r := fixtureRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		close(entered)
		<-release
		io.WriteString(w, `{"is_credits_billing":true,"user_entitlement_pack_list":[]}`)
	})
	a := &auth.Auth{UID: "quota-publish", Variant: "cn", AccessToken: "fixture", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	id := accountID(a.UID)
	state := &accountState{Auth: a, Enabled: true, Identities: map[string]*auth.Auth{"cn": a}}
	r.accounts[id] = state
	done := make(chan error, 1)
	go func() { done <- r.refreshQuotaRaw(context.Background(), id, true) }()
	<-entered
	a.Lock()
	close(release)
	select {
	case err := <-done:
		a.Unlock()
		if err == nil {
			t.Fatal("published quota while identity unavailable")
		}
	case <-time.After(time.Second):
		a.Unlock()
		<-done
		t.Fatal("quota publication waited for network credential lock")
	}
	if !state.QuotaFailed {
		t.Fatal("singleflight followers would see stale success")
	}
	r.Models(context.Background())
}

func TestReasoningCatalogMergePreservesNativeOptions(t *testing.T) {
	entry := provider.CatalogModel{Extra: map[string]any{}}
	applyModelCapabilities(&entry, upstream.ModelInfo{ReasoningOptions: []string{"light", "extra_high"}, DefaultReasoning: "light"})
	mergeCatalogCapabilities(&entry, upstream.ModelInfo{ReasoningOptions: []string{"high", "max"}, DefaultReasoning: "high"})
	reasoning := entry.Extra["reasoning"].(map[string]any)
	efforts := reasoning["supportedEfforts"].([]string)
	if strings.Join(efforts, ",") != "low,xhigh,high,max" || reasoning["defaultEffort"] != nil {
		t.Fatalf("merged efforts/default: %v", reasoning)
	}
	if got := entry.Extra["reasoning_options"].([]string); strings.Join(got, ",") != "light,extra_high,high,max" {
		t.Fatal("lost native options", got)
	}
}

func TestEnterpriseAuthFlagSurvivesValidationAndSave(t *testing.T) {
	a, err := auth.Parse([]byte(`{"accessToken":"fixture","variant":"cn","host":"https://console.enterprise.trae.cn"}`))
	if err != nil || !a.EnterpriseConsole {
		t.Fatal("enterprise host not recognized", err)
	}
	if err = validateAuth(a); err != nil || a.APIHost != upstream.OAuthHost || !a.EnterpriseConsole {
		t.Fatal("host metadata changed network target", err)
	}
	a.FilePath = filepath.Join(t.TempDir(), "auth.json")
	if err = a.SaveAtomic(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(a.FilePath)
	if err != nil {
		t.Fatal(err)
	}
	next, err := auth.Parse(raw)
	if err != nil || !next.EnterpriseConsole {
		t.Fatal("enterprise identity lost after restart", err)
	}
}

func TestEnterpriseQuotaPublicationKeepsCurrencyAndFailureScope(t *testing.T) {
	var fail atomic.Bool
	r := fixtureRuntime(t, func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "get_personal_quota"):
			io.WriteString(w, `{"code":0,"user_model_quota":{"user_id":"me","seat_pool_currency_quota":100}}`)
		case strings.HasSuffix(req.URL.Path, "get_user_model_usage"):
			if fail.Load() {
				w.WriteHeader(503)
				return
			}
			io.WriteString(w, `{"user_usage_list":[{"user_id":"me","in_seat_detail":{"chat_usage_details":[{"total_cost_currency":25}]}}]}`)
		case strings.HasSuffix(req.URL.Path, "get_personal_core_data"):
			io.WriteString(w, `{"Data":{"ResetTime":1800000000000}}`)
		default:
			io.WriteString(w, `{"is_credits_billing":true,"user_entitlement_pack_list":[{"display_desc":"Consumer","entitlement_base_info":{"quota":{"credits_limit":4000}},"usage":{"credits_amount":4}}]}`)
		}
	})
	a := &auth.Auth{UID: "me", Variant: "cn", AccessToken: "fixture", ExpiresAt: time.Now().Add(time.Hour).Unix(), EnterpriseConsole: true}
	id := accountID(a.UID)
	state := &accountState{Auth: a, Enabled: true, Identities: map[string]*auth.Auth{"cn": a}}
	r.accounts[id] = state
	if err := r.refreshQuota(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	if state.Remaining == nil || *state.Remaining != 3996 || state.QuotaUnit != "credits" || state.Expiry != nil || len(state.CreditPackages) != 2 || state.CreditPackages[1]["display_only"] != true {
		t.Fatal("enterprise quota mixed with credits", state)
	}
	data := r.AdminData(context.Background())
	if data.Accounts[0]["quota_unit"] != "credits" || data.Budgets[0].Remaining == nil || *data.Budgets[0].Remaining != 3996 {
		t.Fatal("display-only credits subtracted", data)
	}
	fail.Store(true)
	if err := r.refreshQuota(context.Background(), id, true); err != nil {
		t.Fatal("reference quota blocked consumer credits", err)
	}
	if state.QuotaFailed || state.Remaining == nil || *state.Remaining != 3996 {
		t.Fatal("failed read destroyed snapshot", state)
	}
	if guard := r.authGuard().State(id+":cn:auth", a.MaintenanceIdentity()); guard.NeedsLogin {
		t.Fatal("seat read failure invalidated sign-in", guard)
	}
}

func TestMaintenanceIdentitySeparatesEnterpriseScope(t *testing.T) {
	a := &auth.Auth{AccessToken: "fixture", UID: "me", Variant: "cn"}
	before := a.MaintenanceIdentity()
	a.EnterpriseConsole = true
	if before == a.MaintenanceIdentity() {
		t.Fatal("enterprise scope reused personal maintenance state")
	}
}

func TestContextTransportKeepsRequestDeadlineAndOuterCancel(t *testing.T) {
	for _, outerCancel := range []bool{false, true} {
		outer, cancelOuter := context.WithCancel(context.Background())
		inner, cancelInner := context.WithTimeout(context.Background(), 20*time.Millisecond)
		req, _ := http.NewRequestWithContext(inner, "GET", "https://example.test", nil)
		transport := contextTransport{ctx: outer, base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if outerCancel {
				cancelOuter()
			}
			select {
			case <-r.Context().Done():
				return nil, r.Context().Err()
			case <-time.After(time.Second):
				t.Error("request deadline or parent cancellation was lost")
				return nil, errors.New("timeout")
			}
		})}
		_, err := transport.RoundTrip(req)
		if err == nil {
			t.Fatal("cancellation ignored")
		}
		cancelInner()
		cancelOuter()
	}
}

func TestEnterpriseCallbackHostIsFixed(t *testing.T) {
	for _, host := range []string{"https://console.enterprise.trae.cn", "https://attacker.test", "https://console.enterprise.trae.cn.attacker.test", "https://user@console.enterprise.trae.cn"} {
		r := &Runtime{logins: map[string]*loginState{"trace": {Callback: "http://127.0.0.1:1234/authorize", Expires: time.Now().Add(time.Minute)}}}
		query := url.Values{"state": {"trace"}, "code": {"fixture"}, "userInfo": {`{"APIHost":"` + host + `"}`}}
		if err := r.captureCallback("trace", "http://127.0.0.1:1234/authorize?"+query.Encode(), 0); err != nil {
			t.Fatal(err)
		}
		if r.logins["trace"].EnterpriseConsole != (host == "https://console.enterprise.trae.cn") {
			t.Fatal("unrecognized enterprise target", host)
		}
	}
}

func TestBareTextToolCallsBecomeCallsAndTruncationFails(t *testing.T) {
	for _, truncate := range []bool{false, true} {
		value := `{"tool_calls":[{"name":"unlisted","arguments":{"path":"file"}}]}`
		if truncate {
			value = `{"tool_calls":[`
		}
		first := &upstream.SOLOEvent{Event: "output", Response: value}
		events := newEvents(strings.NewReader("event: done\ndata: {}\n\n"))
		var output bytes.Buffer
		err := normalizeStream(&output, events, first, "trae/test", nil)
		if truncate {
			if err == nil {
				t.Fatal("partial tool sent as answer")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		result, _, _, err := protocol.CollapseChatStreamAs(context.Background(), strings.NewReader(output.String()), protocol.Chat, "test", nil)
		if err != nil || !bytes.Contains(result, []byte(`"finish_reason":"tool_calls"`)) || !bytes.Contains(result, []byte(`"name":"unlisted"`)) {
			t.Fatal("text tool lost", string(result), err)
		}
	}
}
