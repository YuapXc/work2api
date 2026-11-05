package opencode

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
	"work2api/internal/core/protocol"
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
