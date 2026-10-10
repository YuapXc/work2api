package opencode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"work2api/internal/core/provider"
)

type catalogTestTransport func(*http.Request) (*http.Response, error)

func (f catalogTestTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestCatalogCancellationPreservesHealth(t *testing.T) {
	for _, anonymous := range []bool{false, true} {
		for _, before := range []bool{false, true} {
			t.Run(fmt.Sprintf("anonymous=%v/before=%v", anonymous, before), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				proxy := &proxyTransport{client: &http.Client{Transport: catalogTestTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					cancel()
					return nil, req.Context().Err()
				})}}
				proxy.healthy.Store(true)
				transports := &transportPool{items: []*proxyTransport{proxy}}
				nodes, err := newNodePool([]string{"one", "two"}, transports, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				anon := newAnonymousPool(true, transports, time.Minute)
				rt := &Runtime{cfg: Config{Retry: RetryConfig{MaxAttempts: 3}}, transports: transports, zenNodes: nodes, goNodes: &nodePool{}, anonymous: anon}
				if before {
					cancel()
				}
				if anonymous {
					rt.refreshAnonymousTier(ctx, "https://upstream.invalid")
				} else {
					rt.refreshTier(ctx, "https://upstream.invalid", nodes)
				}
				want := 1
				if before {
					want = 0
				}
				if calls != want || !proxy.healthy.Load() || proxy.checking.Load() {
					t.Fatalf("calls=%d healthy=%v checking=%v", calls, proxy.healthy.Load(), proxy.checking.Load())
				}
				for _, node := range nodes.nodes {
					if node.failures.Load() != 0 || node.cooldownUntil.Load() != 0 || nodes.Proxy(node) != proxy {
						t.Fatal("cancellation penalized/rebound key")
					}
				}
				for _, node := range anon.nodes {
					if node.failures.Load() != 0 || node.cooldownUntil.Load() != 0 {
						t.Fatal("cancellation penalized anonymous node")
					}
				}
			})
		}
	}
}

func TestDNSFailurePreservesProxyAndNodeHealth(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeout=%v", timeout), func(t *testing.T) {
			proxy := &proxyTransport{}
			proxy.healthy.Store(true)
			transports := &transportPool{items: []*proxyTransport{proxy}}
			nodes, err := newNodePool([]string{"test-key"}, transports, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			anon := &anonymousNode{proxy: proxy}
			rt := &Runtime{transports: transports, zenNodes: nodes, goNodes: &nodePool{}, anonymous: &anonymousPool{cooldown: time.Minute}}
			dnsErr := fmt.Errorf("request: %w", &net.DNSError{Err: "DNS failed", Name: "upstream.invalid", IsTimeout: timeout})
			if isProxyFailure(dnsErr) {
				t.Fatal("DNS failure classified as broken proxy")
			}
			rt.observeKeyResult(context.Background(), nodes, nodes.nodes[0], proxy, nil, dnsErr)
			rt.observeAnonymousResult(context.Background(), anon, nil, dnsErr)
			if !proxy.healthy.Load() || proxy.checking.Load() {
				t.Fatal("DNS failure mutated proxy health or launched a probe")
			}
			if nodes.nodes[0].failures.Load() != 0 || nodes.nodes[0].cooldownUntil.Load() != 0 || anon.failures.Load() != 0 || anon.cooldownUntil.Load() != 0 {
				t.Fatal("DNS failure penalized a key or anonymous node")
			}
			if nodes.Proxy(nodes.nodes[0]) != proxy {
				t.Fatal("DNS failure rebound the key")
			}
		})
	}
}

func TestNonDNSFailuresRemainPenalized(t *testing.T) {
	if !isProxyFailure(context.DeadlineExceeded) {
		t.Fatal("connection deadline must remain a proxy failure")
	}
	p := &anonymousPool{cooldown: time.Minute}
	n := &anonymousNode{}
	p.MarkFailure(n, nil, fmt.Errorf("connection reset"))
	if n.failures.Load() != 1 || n.cooldownUntil.Load() <= time.Now().UnixNano() {
		t.Fatal("ordinary failure no longer cools the node")
	}
}

func TestMergedMetadataRetainsTierLimitsAndNativeAuthority(t *testing.T) {
	models, err := decodeModelsDev([]byte(`{"opencode":{"models":{"same":{"cost":{"input":1,"output":2},"limit":{"context":100,"output":20}},"zen-only":{"cost":{"input":0,"output":0}}}},"opencode-go":{"id":"opencode-go","models":{"same":{"cost":{"input":0,"output":0},"limit":{"context":200,"input":160,"output":40}},"go-only":{"cost":{"input":0,"output":0},"limit":{"output":30}}}}}`))
	if err != nil || len(models) != 3 {
		t.Fatal(models, err)
	}
	store := &PricingStore{models: models, updatedAt: time.Now()}
	if store.Decide("same").Allowed || store.Decide("go-only").Allowed || !store.Decide("zen-only").Allowed {
		t.Fatal("Go pricing leaked to anonymous Zen")
	}
	c := NewCatalog(TierZen, nil)
	c.SetPricingStore(store)
	c.modelMeta = map[Tier]map[string]Metadata{TierZen: {"same": {ContextWindow: 90}}, TierGo: {}}
	zen, goMD := c.MetadataForTier("same", TierZen), c.MetadataForTier("same", TierGo)
	if zen.ContextWindow != 90 || zen.MaxOutput != 20 || goMD.ContextWindow != 200 || goMD.MaxInput != 160 || goMD.MaxOutput != 40 || goMD.ToolCall || goMD.Reasoning {
		t.Fatal(zen, goMD)
	}
	price, _ := store.Price("same")
	price.Limits[TierGo] = ModelLimits{}
	if c.MetadataForTier("same", TierGo).MaxOutput != 40 {
		t.Fatal("metadata map aliases store")
	}
}

func TestStaleConfigEditorDoesNotWritePersistedConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "opencode.json")
	original := []byte("existing configuration")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry()
	old := &Runtime{path: path}
	if err := registry.Register(old); err != nil {
		t.Fatal(err)
	}
	old.BindRegistry(registry)
	next := &Runtime{path: path}
	next.BindRegistry(registry)
	if err := registry.Replace(old, next); err != nil {
		t.Fatal(err)
	}
	if err := old.SaveConfigDoc(map[string]any{"anonymous": true}); err == nil {
		t.Fatal("stale editor accepted")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != string(original) {
		t.Fatal("stale editor overwrote configuration before failing")
	}
}

func TestExplicitCatalogRefreshRetainsFailedTierAndClearsEmptyTier(t *testing.T) {
	var phase atomic.Int32
	proxy := &proxyTransport{client: &http.Client{Transport: catalogTestTransport(func(req *http.Request) (*http.Response, error) {
		body := `{"opencode":{"npm":"@ai-sdk/openai","models":{"new":{"id":"new"}}}}`
		status := 200
		if req.URL.Host == "tier.invalid" {
			switch phase.Load() {
			case 0:
				body = `{"data":[{"id":"new"}]}`
			case 1:
				body = `{"data":[]}`
			case 2:
				status = 503
				body = `{}`
			default:
				body = `{}`
			}
		}
		if strings.HasSuffix(req.URL.Path, ".mdx") {
			status = 503
			body = `{}`
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})}}
	proxy.healthy.Store(true)
	transports := &transportPool{items: []*proxyTransport{proxy}}
	nodes, err := newNodePool([]string{"key"}, transports, 0)
	if err != nil {
		t.Fatal(err)
	}
	c := NewCatalog(TierZen, nil)
	c.ReplaceWithCapabilities([]string{"old"}, []string{"go-old"}, nil, nil, nil)
	rt := &Runtime{cfg: Config{Upstream: UpstreamConfig{Zen: "https://tier.invalid"}, Retry: RetryConfig{MaxAttempts: 1}}, transports: transports, zenNodes: nodes, goNodes: &nodePool{}, anonymous: &anonymousPool{}, catalog: c, ready: true}
	if err := rt.RefreshModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !c.zen["new"] || c.zen["old"] || !c.goModels["go-old"] {
		t.Fatal("successful replacement or unconfigured tier changed")
	}
	phase.Store(2)
	if err := rt.RefreshModels(context.Background()); err == nil || !c.zen["new"] || !c.stale {
		t.Fatal("failed refresh discarded snapshot", err)
	}
	cursor := nodes.Cursor()
	nodes.MarkSuccess(cursor.Next())
	phase.Store(1)
	if err := rt.RefreshModels(context.Background()); err != nil || len(c.zen) != 0 || c.stale {
		t.Fatal("valid empty response did not revoke old models", err)
	}
	phase.Store(3)
	if err := rt.RefreshModels(context.Background()); err == nil {
		t.Fatal("missing data accepted as authoritative empty")
	}
	lifecycle, cancel := context.WithCancel(context.Background())
	rt.lifecycleCtx = lifecycle
	cancel()
	if err := rt.RefreshModels(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatal("closed runtime started refresh", err)
	}
}
