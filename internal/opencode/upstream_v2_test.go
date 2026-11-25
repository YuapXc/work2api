package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"work2api/internal/core/protocol"
	"work2api/internal/core/provider"
	"work2api/internal/opencode/goauth"
)

func TestOfficialV2RoutesAndLegacyFallback(t *testing.T) {
	for _, p := range []protocol.Protocol{protocol.Chat, protocol.Anthropic, protocol.Responses} {
		t.Run(string(p), func(t *testing.T) {
			req, err := newUpstreamRequest(context.Background(), "https://opencode.ai/inference", p, []byte(`{"model":"m"}`), RequestIDs{Session: "s"}, "test-key")
			if err != nil {
				t.Fatal(err)
			}
			family := "openai"
			if p == protocol.Anthropic {
				family = "anthropic"
			}
			if req.URL.Path != "/inference/"+family+protocol.Path(p) {
				t.Fatal(req.URL.Path)
			}
			original := req.URL.Path
			calls := 0
			client := &http.Client{Transport: catalogTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				data, _ := io.ReadAll(r.Body)
				if string(data) != `{"model":"m"}` || r.Header.Get("x-opencode-session") != "s" {
					t.Fatal("fallback changed body or identity")
				}
				status := 404
				if calls == 2 {
					status = 200
					if r.URL.Path != "/inference"+protocol.Path(p) {
						t.Fatal(r.URL.Path)
					}
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
			})}
			resp, err := doCompatibleInferenceAttempt(client, req, p, time.Second)
			if err != nil || resp.StatusCode != 200 || calls != 2 {
				t.Fatalf("fallback calls=%d resp=%v err=%v", calls, resp, err)
			}
			resp.Body.Close()
			if req.URL.Path != original {
				t.Fatal("mutated original request")
			}
			custom, _ := newUpstreamRequest(context.Background(), "https://example.invalid/inference", p, nil, RequestIDs{}, "")
			if custom.URL.Path != "/inference"+protocol.Path(p) {
				t.Fatal("rewrote custom URL")
			}
		})
	}
}

func TestV2FallbackOnlyForUncommittedRouteMiss(t *testing.T) {
	for _, status := range []int{200, 400, 401, 403, 413, 429, 500} {
		calls := 0
		req, _ := newUpstreamRequest(context.Background(), "https://opencode.ai/inference", protocol.Chat, []byte("{}"), RequestIDs{}, "")
		client := &http.Client{Transport: catalogTestTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("{}"))}, nil
		})}
		resp, err := doCompatibleInferenceAttempt(client, req, protocol.Chat, time.Second)
		if err != nil || calls != 1 || resp.StatusCode != status {
			t.Fatalf("status=%d calls=%d err=%v", status, calls, err)
		}
		resp.Body.Close()
	}
}

func TestProbeFailuresDoNotRemainOptimisticallyHealthy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		failed bool
	}{
		{"EOF", io.ErrUnexpectedEOF, true}, {"TLS", errors.New("TLS handshake failed"), true}, {"local DNS", &net.DNSError{Err: "lookup failed", Name: "example.invalid"}, false}, {"canceled", context.Canceled, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &proxyTransport{client: &http.Client{Transport: catalogTestTransport(func(*http.Request) (*http.Response, error) { return nil, tc.err })}}
			p.healthy.Store(true)
			result := (&transportPool{}).checkClaimedProxy(context.Background(), p, "https://example.invalid", time.Second)
			if result.failed != tc.failed || p.healthy.Load() == tc.failed {
				t.Fatalf("failed=%v healthy=%v", result.failed, p.healthy.Load())
			}
		})
	}
}

func TestAdminHealthCountsCoolingKeysAndDeduplicatesIdentity(t *testing.T) {
	proxy := &proxyTransport{index: 0}
	proxy.healthy.Store(true)
	transports := &transportPool{items: []*proxyTransport{proxy}}
	zen, _ := newNodePool([]string{"shared-key", "cooling-key"}, transports, time.Minute)
	goPool, _ := newNodePool([]string{"shared-key"}, transports, time.Minute)
	zen.nodes[0].cooldownUntil.Store(time.Now().Add(time.Hour).UnixNano())
	zen.nodes[1].cooldownUntil.Store(time.Now().Add(time.Hour).UnixNano())
	rt := &Runtime{ready: true, cfg: defaultConfig(), transports: transports, zenNodes: zen, goNodes: goPool, catalog: NewCatalog(TierZen, nil), pricing: &PricingStore{}}
	data := rt.AdminData(context.Background())
	keys := 0
	for _, row := range data.Accounts {
		if row["id"] == "zen" && (row["available_count"] != 0 || row["total_count"] != 2) {
			t.Fatal("tier hid cooling keys", row)
		}
		if row["credential"] == true {
			keys++
			if row["id"] == PortalKeyUID("shared-key") && (row["healthy"] != true || row["cooldown_until"] != nil) {
				t.Fatal("healthy Go identity overshadowed by cooling Zen", row)
			}
		}
	}
	if keys != 2 {
		t.Fatal("duplicate shared identity", keys)
	}
}

func TestProxyRecoveryUsesFailingServiceOrigin(t *testing.T) {
	target := proxyProbeOrigin("https://upstream.invalid/zen/go?private=1")
	for _, status := range []int{http.StatusNotFound, http.StatusProxyAuthRequired} {
		proxy := &proxyTransport{client: &http.Client{Transport: catalogTestTransport(func(req *http.Request) (*http.Response, error) {
			if req.URL.String() != "https://upstream.invalid/" || req.Method != http.MethodGet || req.Header.Get("Authorization") != "" {
				t.Error("unexpected probe request", req.Method, req.URL)
			}
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
		})}}
		proxy.probeOrigin.Store(&target)
		results := (&transportPool{items: []*proxyTransport{proxy}}).CheckHealth(context.Background(), "https://cloudflare.com/cdn-cgi/trace", time.Second)
		wantHealthy := status != http.StatusProxyAuthRequired
		if len(results) != 1 || proxy.healthy.Load() != wantHealthy || results[0].failed == wantHealthy {
			t.Fatal(status, results, proxy.healthy.Load())
		}
	}
}

func TestForeignIDsRecoverOnceAndRecheckPermission(t *testing.T) {
	for _, denial := range []string{`{"error":{"message":"created under a different resource"}}`, `{"error":{"message":"Invalid 'input[0].id': Expected a string with maximum length 64"}}`} {
		for _, deny := range []bool{false, true} {
			calls, checks := 0, 0
			req, _ := newUpstreamRequest(context.Background(), "https://example.test", protocol.Responses, []byte(`{"store":false,"input":[{"role":"assistant","id":"old","content":"before"},{"role":"user","content":"next"}]}`), RequestIDs{}, "k")
			client := &http.Client{Transport: catalogTestTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				raw, _ := io.ReadAll(r.Body)
				if calls > 1 && strings.Contains(string(raw), `"id":"old"`) {
					t.Fatal("foreign ID retained")
				}
				return &http.Response{StatusCode: 400, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(denial))}, nil
			})}
			resp, err := doCompatibleInferenceAttempt(client, req, protocol.Responses, time.Second, func() error {
				checks++
				if deny {
					return errors.New("revoked")
				}
				return nil
			})
			if checks != 1 || deny && (err == nil || calls != 1) || !deny && (err != nil || calls != 2) {
				t.Fatal(calls, checks, err)
			}
			if resp != nil {
				resp.Body.Close()
			}
		}
	}
}

func subscriptionTestRuntime(t *testing.T, handler func(*http.Request) (*http.Response, error)) *Runtime {
	t.Helper()
	proxy := &proxyTransport{client: &http.Client{Transport: catalogTestTransport(handler)}}
	proxy.healthy.Store(true)
	transports := &transportPool{items: []*proxyTransport{proxy}}
	cfg := defaultConfig()
	cfg.GoKeys = []string{"manual-go-key"}
	goNodes, _ := newNodePool(cfg.GoKeys, transports, time.Minute)
	rt := &Runtime{cfg: cfg, path: filepath.Join(t.TempDir(), "opencode.json"), ready: true, transports: transports, goNodes: goNodes, zenNodes: &nodePool{}, anonymous: newAnonymousPool(false, transports, time.Minute), catalog: NewCatalog(TierGo, nil), pricing: &PricingStore{}, logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	return rt
}
func subscriptionJSON(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}
func subscriptionFixtureConnection() *goauth.Connection {
	return &goauth.Connection{Credential: goauth.Credential{AccessToken: "access-fixture", RefreshToken: "refresh-fixture", AccountID: "account", OrganizationID: "org", OrganizationName: "Org", Email: "test@example.invalid", ExpiresAt: time.Now().Add(time.Hour)}, Routes: map[string]goauth.Route{"m": {ModelID: "remote-m", Protocol: protocol.Chat, BaseURL: "https://opencode.ai/inference/go/openai"}}}
}
func TestSubscriptionDeviceAuthorizationAndPersistence(t *testing.T) {
	var polls, modelCalls atomic.Int32
	rt := subscriptionTestRuntime(t, func(req *http.Request) (*http.Response, error) {
		switch req.URL.Path {
		case "/console/auth/device/code":
			return subscriptionJSON(200, `{"device_code":"private-device-code","user_code":"CODE","verification_uri_complete":"/console/verify?code=CODE","expires_in":600,"interval":1}`), nil
		case "/console/auth/device/token":
			polls.Add(1)
			return subscriptionJSON(200, `{"access_token":"access-fixture","refresh_token":"refresh-fixture","expires_in":3600}`), nil
		case "/console/api/user":
			return subscriptionJSON(200, `{"id":"account","email":"test@example.invalid"}`), nil
		case "/console/api/orgs":
			return subscriptionJSON(200, `[{"id":"org","name":"Org"},{"id":"other","name":"Other"}]`), nil
		case "/console/api/v2/config":
			if req.Header.Get("x-org-id") != "org" {
				t.Error("wrong organization")
			}
			return subscriptionJSON(200, `{"providers":{"go":{"package":"@ai-sdk/openai-compatible","settings":{"baseURL":"https://opencode.ai/inference/go"},"models":{"m":{"modelID":"remote-m"}}}}}`), nil
		case "/inference/go/openai/v1/chat/completions":
			modelCalls.Add(1)
			raw, _ := io.ReadAll(req.Body)
			if !strings.Contains(string(raw), "9007199254740993") {
				t.Error("model mapping altered business number")
			}
			var body map[string]any
			json.Unmarshal(raw, &body)
			if body["model"] != "remote-m" || req.Header.Get("Authorization") != "Bearer access-fixture" || req.Header.Get("x-org-id") != "org" {
				t.Error("wrong subscription dispatch")
			}
			return subscriptionJSON(200, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`), nil
		default:
			t.Error("unexpected request", req.URL.Path)
			return subscriptionJSON(400, `{}`), nil
		}
	})
	view, err := rt.DeviceAuthorization(t.Context(), "start", nil)
	if err != nil {
		t.Fatal(err)
	}
	pending := view["pending"].(map[string]any)
	id := pending["id"]
	rt.subscription().pending.next = time.Time{}
	view, err = rt.DeviceAuthorization(t.Context(), "poll", map[string]any{"id": id})
	if err != nil {
		t.Fatal(err)
	}
	if view["pending"].(map[string]any)["status"] != "choosing_org" {
		t.Fatal(view)
	}
	if _, err = rt.DeviceAuthorization(t.Context(), "poll", map[string]any{"id": id, "organization_id": "unlisted"}); err == nil {
		t.Fatal("accepted foreign organization")
	}
	view, err = rt.DeviceAuthorization(t.Context(), "poll", map[string]any{"id": id, "organization_id": "org"})
	if err != nil || view["connected"] != true {
		t.Fatal(view, err)
	}
	wire, _ := json.Marshal(view)
	for _, secret := range []string{"access-fixture", "refresh-fixture", "private-device-code"} {
		if strings.Contains(string(wire), secret) {
			t.Fatal("credential escaped")
		}
	}
	doc, _ := rt.ConfigDoc()
	wire, _ = json.Marshal(doc)
	if strings.Contains(string(wire), "access-fixture") {
		t.Fatal("config leaked OAuth token")
	}
	if polls.Load() != 1 || rt.goKeyCount() != 0 || rt.cfg.GoKeys[0] != "manual-go-key" {
		t.Fatal("manual keys not paused intact")
	}
	reloaded := &Runtime{path: rt.path}
	active, c := reloaded.subscriptionSnapshot()
	if !active || c == nil || c.Credential.OrganizationID != "org" {
		t.Fatal("connection not persisted")
	}
	uid := subscriptionUID(c)
	if _, err := rt.resolveRoute("m", provider.Caller{UserID: 1, AccountScope: map[string]bool{"other": true}}); err == nil {
		t.Fatal("scope bypass")
	}
	route, err := rt.resolveRoute("m", provider.Caller{UserID: 1, AccountScope: map[string]bool{uid: true}})
	if err != nil || route.Protocol != protocol.Chat {
		t.Fatal(route, err)
	}
	if len(rt.subscriptionModels(nil, map[string]bool{"opencode/other": true})) != 0 {
		t.Fatal("catalog filter bypass")
	}
	accountUID := ""
	resp, err, attempts := rt.doKeyUpstream(t.Context(), route, func(Tier) ([]byte, error) {
		return []byte(`{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}],"metadata":{"n":9007199254740993}}`), nil
	}, RequestIDs{AccountUID: &accountUID}, 0)
	if err != nil || resp == nil || resp.StatusCode != 200 || attempts != 1 || modelCalls.Load() != 1 || accountUID != uid {
		t.Fatal("subscription call failed", err, attempts)
	}
	resp.Body.Close()
	view, err = rt.DeviceAuthorization(t.Context(), "disconnect", nil)
	if err != nil || view["connected"] != false || rt.goKeyCount() != 1 {
		t.Fatal("manual keys not restored", err)
	}
	if _, err = os.Stat(rt.subscriptionPath()); !os.IsNotExist(err) {
		t.Fatal("credential file retained", err)
	}
}
func TestSubscriptionInvalidFileDoesNotEnableManualFallback(t *testing.T) {
	rt := subscriptionTestRuntime(t, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected upstream"); return nil, nil })
	if err := os.WriteFile(rt.subscriptionPath(), []byte(`{"credential":"broken"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if rt.goKeyCount() != 0 {
		t.Fatal("bad OAuth enabled manual keys")
	}
	if _, err := rt.subscriptionConnection(t.Context()); err == nil {
		t.Fatal("bad credential accepted")
	}
	malicious := subscriptionFixtureConnection()
	malicious.Routes["m"] = goauth.Route{ModelID: "m", Protocol: protocol.Chat, BaseURL: "https://attacker.invalid/inference/go/openai"}
	if malicious.Validate() == nil {
		t.Fatal("unsafe endpoint accepted")
	}
	if err := rt.installSubscription(t.Context(), malicious); err == nil {
		t.Fatal("unsafe connection persisted")
	}
}
func TestSubscriptionRefreshSingleFlightAndFailure(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprint(failure), func(t *testing.T) {
			var refreshes atomic.Int32
			rt := subscriptionTestRuntime(t, func(req *http.Request) (*http.Response, error) {
				refreshes.Add(1)
				if failure {
					return subscriptionJSON(400, `{"error":"invalid_grant"}`), nil
				}
				return subscriptionJSON(200, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600,"org_id":"org"}`), nil
			})
			c := subscriptionFixtureConnection()
			c.Credential.ExpiresAt = time.Now().Add(-time.Minute)
			if err := rt.installSubscription(t.Context(), c); err != nil {
				t.Fatal(err)
			}
			var group sync.WaitGroup
			for i := 0; i < 8; i++ {
				group.Go(func() {
					next, err := rt.subscriptionConnection(t.Context())
					if failure {
						if err == nil {
							t.Error("invalid grant accepted")
						}
					} else if err != nil || next.Credential.AccessToken != "rotated-access" {
						t.Error("rotation failed", err)
					}
				})
			}
			group.Wait()
			if refreshes.Load() != 1 {
				t.Fatal("duplicate credential refresh", refreshes.Load())
			}
			if rt.goKeyCount() != 0 {
				t.Fatal("refresh failure enabled manual fallback")
			}
			_, next := rt.subscriptionSnapshot()
			if failure && !next.NeedsLogin {
				t.Fatal("dead authorization not marked")
			}
		})
	}
}
func TestRetryKeepsRejectionAfterTransportFailure(t *testing.T) {
	for _, mode := range []string{"transport", "cancel", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			calls := 0
			var rejectedClosed atomic.Int32
			firstUID := ""
			rt := subscriptionTestRuntime(t, func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					return subscriptionJSON(200, `{}`), nil
				}
				calls++
				if calls == 1 {
					firstUID = PortalKeyUID(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
					resp := subscriptionJSON(429, `{"error":{"message":"capacity"}}`)
					resp.Header.Set("Retry-After", "37")
					resp.Body = &countedResponseBody{resp.Body, &rejectedClosed}
					return resp, nil
				}
				if mode == "cancel" {
					cancel()
					return nil, ctx.Err()
				}
				return nil, &net.OpError{Op: "dial", Err: errors.New("synthetic connection failure")}
			})
			rt.cfg.Retry.MaxAttempts = 2
			nodes, _ := newNodePool([]string{"first", "second"}, rt.transports, time.Minute)
			rt.zenNodes = nodes
			checks := 0
			if mode == "revoked" {
				ctx = provider.WithInvocation(ctx, provider.Invocation{Check: func(context.Context, provider.AccountRef, string) error {
					checks++
					if checks > 1 {
						return provider.NewAPIError(403, "revoked", "model_not_allowed")
					}
					return nil
				}})
			}
			uid := ""
			resp, err, _ := rt.doKeyUpstream(ctx, Route{ID: "m", Tier: TierZen, Protocol: protocol.Chat}, func(Tier) ([]byte, error) { return []byte(`{"model":"m","stream":false}`), nil }, RequestIDs{AccountUID: &uid}, 0)
			if rejectedClosed.Load() != 1 {
				t.Fatal("physical error response leaked", rejectedClosed.Load())
			}
			if mode == "transport" {
				if err != nil || resp == nil || resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "37" || uid != firstUID {
					t.Fatal("lost rejection", resp, err, uid)
				}
				data, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if !strings.Contains(string(data), "capacity") {
					t.Fatal("lost error body")
				}
			} else {
				if resp != nil || err == nil {
					t.Fatal("error did not take precedence", resp, err)
				}
			}
		})
	}
}

func TestSubscription401RecoveryHasNoKeyFallback(t *testing.T) {
	for _, mode := range []string{"success", "second-401", "revoked", "budget"} {
		t.Run(mode, func(t *testing.T) {
			var calls, refreshes atomic.Int32
			rt := subscriptionTestRuntime(t, func(req *http.Request) (*http.Response, error) {
				if req.URL.Path == "/console/auth/device/token" {
					refreshes.Add(1)
					return subscriptionJSON(200, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600,"org_id":"org"}`), nil
				}
				if !strings.HasPrefix(req.URL.Path, "/inference/go/") {
					t.Error("fell back to non-subscription URL", req.URL.Path)
				}
				n := calls.Add(1)
				if n == 1 || mode == "second-401" {
					return subscriptionJSON(401, `{"secret":"access-fixture"}`), nil
				}
				if req.Header.Get("Authorization") != "Bearer rotated-access" {
					t.Error("old token reused")
				}
				return subscriptionJSON(200, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`), nil
			})
			c := subscriptionFixtureConnection()
			if err := rt.installSubscription(t.Context(), c); err != nil {
				t.Fatal(err)
			}
			ctx := t.Context()
			if mode == "revoked" {
				ctx = provider.WithInvocation(ctx, provider.Invocation{Check: func(context.Context, provider.AccountRef, string) error {
					if calls.Load() > 0 {
						return provider.NewAPIError(403, "revoked", "model_not_allowed")
					}
					return nil
				}})
			}
			offset := 0
			if mode == "budget" {
				offset = rt.cfg.Retry.MaxAttempts - 1
			}
			route := Route{ID: "m", Tier: TierGo, Protocol: protocol.Chat, KeyTiers: []Tier{TierGo}}
			resp, err, attempts := rt.doKeyUpstream(ctx, route, func(Tier) ([]byte, error) { return []byte(`{"model":"m","stream":false}`), nil }, RequestIDs{}, offset)
			if mode == "revoked" {
				if err == nil || resp != nil || calls.Load() != 1 || refreshes.Load() != 0 {
					t.Fatal("revoked call was retried", err, calls.Load(), refreshes.Load())
				}
				return
			}
			if err != nil || resp == nil {
				t.Fatal(resp, err)
			}
			defer resp.Body.Close()
			want := int32(2)
			if mode == "budget" {
				want = 1
			}
			if attempts != int(want) || calls.Load() != want {
				t.Fatal("wrong send budget", attempts, calls.Load())
			}
			if mode == "success" && resp.StatusCode != 200 {
				t.Fatal("auth retry failed")
			}
			if mode == "second-401" || mode == "budget" {
				raw, _ := io.ReadAll(resp.Body)
				if strings.Contains(string(raw), "access-fixture") {
					t.Fatal("credential reflected")
				}
				_, next := rt.subscriptionSnapshot()
				if !next.NeedsLogin {
					t.Fatal("persistent unauthorized not surfaced")
				}
			}
			if rt.goKeyCount() != 0 {
				t.Fatal("manual keys resumed on failure")
			}
		})
	}
}
func TestSubscriptionFreeRouteAndStaleWriter(t *testing.T) {
	rt := subscriptionTestRuntime(t, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected request"); return nil, nil })
	c := subscriptionFixtureConnection()
	c.Routes["m-free"] = c.Routes["m"]
	if err := rt.installSubscription(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	rt.cfg.Anonymous = true
	rt.catalog.ReplaceWithCapabilities([]string{"m-free"}, nil, map[Tier]map[string]protocol.Protocol{TierZen: {"m-free": protocol.Chat}}, nil, nil)
	prices, err := decodeModelsDev([]byte(`{"opencode":{"models":{"m-free":{"cost":{"input":0,"output":0}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	rt.catalog.SetPricingStore(&PricingStore{models: prices, updatedAt: time.Now()})
	base, baseErr := rt.catalog.Route("m-free", true, false, true)
	if baseErr != nil {
		t.Fatal(baseErr)
	}
	// Explicit zero-price catalog evidence must remain on its free route.
	if !rt.catalog.IsFreeModel("m-free") {
		t.Fatal("missing free fixture evidence")
	}
	{
		route, err := rt.resolveRoute("m-free", provider.Caller{})
		if err != nil || route.Tier != base.Tier || route.Anonymous != base.Anonymous {
			t.Fatal("free permission consumed a subscription", route, err)
		}
	}
	registry := provider.NewRegistry()
	if err := registry.Register(rt); err != nil {
		t.Fatal(err)
	}
	rt.BindRegistry(registry)
	next := subscriptionTestRuntime(t, func(*http.Request) (*http.Response, error) { return nil, nil })
	next.path = rt.path
	if err := registry.Replace(rt, next); err != nil {
		t.Fatal(err)
	}
	if err := rt.installSubscription(t.Context(), nil); err == nil {
		t.Fatal("stale runtime disconnected new configuration")
	}
	if _, err := os.Stat(rt.subscriptionPath()); err != nil {
		t.Fatal("stale writer removed credential", err)
	}
}

func TestSubscriptionConcurrent401SharesRotation(t *testing.T) {
	var refreshes atomic.Int32
	rt := subscriptionTestRuntime(t, func(*http.Request) (*http.Response, error) {
		refreshes.Add(1)
		return subscriptionJSON(200, `{"access_token":"rotated-access","refresh_token":"rotated-refresh","expires_in":3600,"org_id":"org"}`), nil
	})
	c := subscriptionFixtureConnection()
	if err := rt.installSubscription(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Go(func() {
			next, err := rt.subscriptionConnection(t.Context(), c)
			if err != nil || next.Credential.AccessToken != "rotated-access" {
				t.Error("401 refresh failed", err)
			}
		})
	}
	group.Wait()
	if refreshes.Load() != 1 {
		t.Fatal("parallel 401 rotated more than once", refreshes.Load())
	}
}

type countedResponseBody struct {
	io.ReadCloser
	closed *atomic.Int32
}

func (b *countedResponseBody) Close() error { b.closed.Add(1); return b.ReadCloser.Close() }

func TestOAuthOnlyConfigSurvivesDisconnectAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.json")
	rt := &Runtime{path: path}
	if err := rt.installSubscription(t.Context(), subscriptionFixtureConnection()); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.Anonymous = false
	cfg.ZenKeys = nil
	cfg.GoKeys = nil
	validated, err := Normalize(path, cfg)
	if err != nil {
		t.Fatal("OAuth-only rejected", err)
	}
	if err := writeConfigFile(path, validated); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadConfig(path)
	if err != nil || loaded.Anonymous || len(loaded.GoKeys) != 0 || len(loaded.ZenKeys) != 0 {
		t.Fatal("changed credential exposure", err)
	}
	restarted := &Runtime{path: path}
	active, c := restarted.subscriptionSnapshot()
	if !active || c == nil {
		t.Fatal("lost subscription")
	}
	if err := rt.installSubscription(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatal("disconnected config breaks restart", err)
	}
	disconnected := &Runtime{path: path}
	if active, _ := disconnected.subscriptionSnapshot(); active {
		t.Fatal("disconnected subscription active")
	}
	payload := map[string]any{"tools": []any{map[string]any{"type": "function", "function": map[string]any{"name": "echo", "parameters": map[string]any{"type": "object"}}}}}
	aliased, names := aliasClientTools(payload, protocol.Chat, true, true)
	if len(names) != 1 {
		t.Fatal(names)
	}
	for alias, original := range names {
		if !strings.HasPrefix(alias, "mcp_") || original != "echo" {
			t.Fatal(alias, original)
		}
	}
	_ = aliased
}
