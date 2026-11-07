package opencode

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"work2api/internal/core/protocol"
	"work2api/internal/streamwatch"
)

type fallbackTransport func(*http.Request) (*http.Response, error)

func (f fallbackTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInferenceRecordsHeadersBeforeReadingOutput(t *testing.T) {
	ctx, timing := streamwatch.WithTiming(t.Context())
	body, release := io.Pipe()
	defer body.Close()
	defer release.Close()
	client := &http.Client{Transport: fallbackTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"fixture"}}, Body: body}, nil
	})}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://example.test", nil)
	if err != nil {
		t.Fatal(err)
	}
	streamwatch.StartAttempt(ctx)
	result := make(chan error, 1)
	go func() {
		resp, err := readInferenceAttempt(client, req, protocol.Chat)
		if resp != nil {
			resp.Body.Close()
		}
		result <- err
	}()
	deadline := time.After(time.Second)
	for !timing.Snapshot().AttemptStages[0].Finished {
		select {
		case <-deadline:
			t.Fatal("header timing waited for SSE output")
		case <-time.After(time.Millisecond):
		}
	}
	stage := timing.Snapshot().AttemptStages[0]
	if stage.HTTPStatus != 200 || stage.UpstreamRequestID != "fixture" {
		t.Fatal("missing HTTP stage information", stage)
	}
	_, err = io.WriteString(release, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
	if err != nil || <-result != nil {
		t.Fatal("SSE prefix was not released", err)
	}
}

func TestInferenceMessagesBetaOnlyForKnownV2Endpoint(t *testing.T) {
	for _, tc := range []struct {
		base  string
		proto protocol.Protocol
		beta  string
	}{
		{"https://opencode.ai/inference", protocol.Anthropic, "true"},
		{"https://opencode.ai/inference/", protocol.Anthropic, "true"},
		{"https://opencode.ai/inference", protocol.Chat, ""},
		{"https://opencode.ai/inference", protocol.Responses, ""},
		{"https://opencode.ai/zen", protocol.Anthropic, ""},
		{"https://custom.invalid/inference", protocol.Anthropic, ""},
	} {
		req, err := newUpstreamRequest(context.Background(), tc.base, tc.proto, []byte(`{}`), RequestIDs{}, "synthetic-key")
		if err != nil || req.URL.Query().Get("beta") != tc.beta {
			t.Fatalf("unexpected beta flag for %s %s: %v %v", tc.base, tc.proto, req, err)
		}
	}
}

func TestTierTransportFailurePreservesRealUpstreamError(t *testing.T) {
	for _, status := range []int{429, 503} {
		cfg := Config{Retry: RetryConfig{MaxAttempts: 1}}
		cfg.Upstream.Anonymous = "https://anonymous.invalid"
		cfg.Upstream.Zen = "https://paid.invalid"
		transports, err := newTransportPool([]string{"direct"}, PerformanceConfig{}, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		// Keep background proxy health probes out of this deterministic lane test.
		transports.items[0].checking.Store(true)
		calls := 0
		transports.items[0].client = &http.Client{Transport: fallbackTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Host == "anonymous.invalid" {
				return &http.Response{StatusCode: status, Header: http.Header{"Retry-After": {"37"}}, Body: io.NopCloser(strings.NewReader(`{"error":"busy"}`)), Request: r}, nil
			}
			return nil, errors.New("synthetic connection failure")
		})}
		keys, _ := newNodePool([]string{"synthetic-key"}, transports, time.Second)
		rt := &Runtime{cfg: cfg, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), transports: transports, zenNodes: keys, goNodes: &nodePool{}, anonymous: newAnonymousPool(true, transports, time.Second), catalog: NewCatalog(TierZen, nil)}
		uid := ""
		route := Route{ID: "probe", Tier: TierZen, Protocol: protocol.Chat, Anonymous: true, KeyTiers: []Tier{TierZen}}
		resp, effective, _, err := rt.doUpstreamTiers(context.Background(), route, fixtureRouteBodies(map[Tier][]byte{TierZen: []byte(`{"model":"probe","messages":[]}`)}), RequestIDs{Session: "probe", AccountUID: &uid}, 0)
		if err != nil || resp == nil {
			t.Fatal("real error replaced by transport failure", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if calls != 2 || resp.StatusCode != status || resp.Header.Get("Retry-After") != "37" || string(body) != `{"error":"busy"}` || !effective.Anonymous || uid != "opencode:anonymous" {
			t.Fatalf("wrong retained response calls=%d status=%d route=%+v uid=%s body=%s", calls, resp.StatusCode, effective, uid, body)
		}
	}
}

func TestDeterministicRejectionPreservesBody(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   bool
	}{
		{413, "payload too large", true},
		{400, `{"error":{"code":"context_length_exceeded"}}`, true},
		{422, `{"error":{"message":"Maximum context length exceeded"}}`, true},
		{400, `{"error":{"type":"invalid_request_error","message":"model unavailable"}}`, false},
		{400, `{"error":{"message":"unsupported tool"}}`, false},
		{403, `{"error":{"code":"context_length_exceeded"}}`, false},
		{429, `{"error":{"message":"prompt is too long"}}`, false},
		{400, `{"error":{"message":"other","param":"prompt is too long"}}`, false},
		{400, strings.Repeat("x", 20000), false},
	} {
		resp := &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body))}
		for i := 0; i < 2; i++ {
			if got := deterministicRequestRejection(resp); got != tc.want {
				t.Fatalf("status=%d want=%v got=%v", tc.status, tc.want, got)
			}
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil || string(body) != tc.body {
			t.Fatal("inspection changed the upstream error", err)
		}
	}
}

func TestRequestRejectionStopsAnonymousExitsAndKeyFallback(t *testing.T) {
	for _, anonymous := range []bool{true, false} {
		cfg := Config{Retry: RetryConfig{MaxAttempts: 2}}
		cfg.Upstream.Anonymous = "https://anonymous.invalid"
		cfg.Upstream.Zen = "https://paid.invalid"
		transports, err := newTransportPool([]string{"direct"}, PerformanceConfig{}, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		const rejection = `{"error":{"code":"context_length_exceeded","message":"shorten input"}}`
		client := &http.Client{Transport: fallbackTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(rejection)), Request: r}, nil
		})}
		transports.items[0].checking.Store(true)
		transports.items[0].client = client
		second := &proxyTransport{index: 1, name: "synthetic-second-exit", client: client}
		second.healthy.Store(true)
		second.checking.Store(true)
		transports.items = append(transports.items, second)
		keys, _ := newNodePool([]string{"synthetic-one", "synthetic-two"}, transports, time.Second)
		rt := &Runtime{cfg: cfg, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), transports: transports, zenNodes: keys, goNodes: &nodePool{}, anonymous: newAnonymousPool(true, transports, time.Second), catalog: NewCatalog(TierZen, nil)}
		route := Route{ID: "probe", Tier: TierZen, Protocol: protocol.Chat, Anonymous: anonymous, KeyTiers: []Tier{TierZen, TierGo}}
		resp, _, attempts, err := rt.doUpstreamTiers(context.Background(), route, fixtureRouteBodies(map[Tier][]byte{TierZen: []byte(`{"model":"probe","messages":[]}`)}), RequestIDs{Session: "probe"}, 0)
		if err != nil || resp == nil {
			t.Fatal("missing original rejection", err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if calls != 1 || attempts != 1 || resp.StatusCode != 400 || string(body) != rejection {
			t.Fatalf("anonymous=%v calls=%d attempts=%d status=%d body=%s", anonymous, calls, attempts, resp.StatusCode, body)
		}
		for _, node := range rt.anonymous.nodes {
			if node.failures.Load() != 0 || node.cooldownUntil.Load() != 0 {
				t.Fatal("request error penalized a healthy exit")
			}
		}
	}
}
