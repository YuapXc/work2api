package qoder

import (
	"testing"
	"work2api/internal/qoder/bridge"
)

func TestWorkspaceQuotaAndQueueDoNotCoolOrSwitchAccounts(t *testing.T) {
	for _, raw := range []string{`{"error":{"message":"Workspace allocated quota exceeded"}}`, `{"subCode":"RESOURCE_LIMIT","details":{"isQueued":true}}`, `{"subCode":"RESOURCE_LIMIT","serviceAvailable":false}`} {
		for _, err := range []error{bridge.NewUpstreamError(429, raw), bridge.NewStreamBusinessError(raw)} {
			if canFailover(err) {
				t.Fatal("resource failure must not switch/cool healthy accounts", err)
			}
		}
	}
	if !canFailover(bridge.NewUpstreamError(429, `{"error":{"message":"rate limited"}}`)) {
		t.Fatal("ordinary account rate limiting lost failover")
	}
	if !canFailover(bridge.NewUpstreamError(503, `{"error":{"message":"unavailable"}}`)) {
		t.Fatal("transient fallback regressed")
	}
}

func TestQuota112AllowsOutputBeforeAccountFallback(t *testing.T) {
	for _, raw := range []string{`{"code":112}`, `{"code":"112"}`, `{"body":"{\"code\":112}"}`} {
		if !canFailover(bridge.NewStreamBusinessError(raw)) || !canFailover(bridge.NewUpstreamError(403, raw)) {
			t.Fatal(raw)
		}
	}
}

func TestQueueExitCanRotateWithoutCoolingWholeAccount(t *testing.T) {
	for _, kind := range []string{bridge.ErrTypeQueueStalled, bridge.ErrTypeQueueTimeout} {
		err := &bridge.UpstreamError{Status: 503, ErrType: kind, Message: "queue wait ended"}
		if !canFailover(err) {
			t.Fatal(kind)
		}
		runtime := New("error")
		runtime.coolAccount(nil, "", err)
		if len(runtime.accountCooldowns) != 0 {
			t.Fatal("queue failure cooled whole account")
		}
	}
	if canFailover(bridge.NewStreamBusinessError(`{"subCode":"RESOURCE_LIMIT","isQueued":true}`)) {
		t.Fatal("ordinary queue unexpectedly rotates")
	}
}
