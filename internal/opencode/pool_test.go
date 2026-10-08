package opencode

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

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
