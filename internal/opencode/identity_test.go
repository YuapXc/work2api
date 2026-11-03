package opencode

import (
	"context"
	"encoding/hex"
	"hash/fnv"
	"net/http"
	"strings"
	"testing"
	"time"
	"work2api/internal/core/protocol"
	"work2api/internal/core/provider"
)

func TestSyntheticSessionTimestampWindow(t *testing.T) {
	now := time.Date(2026, 10, 25, 23, 59, 59, 0, time.UTC)
	for _, hash := range [][]byte{{0, 0, 0, 0, 0, 0}, {255, 255, 255, 255, 255, 255}, {1, 2, 3, 4, 5, 6}} {
		part := descendingTimePartAt(now, hash)
		if part != descendingTimePartAt(now.Add(-23*time.Hour), hash) {
			t.Fatal("identity changes within UTC day")
		}
		if part == descendingTimePartAt(now.Add(24*time.Hour), hash) {
			t.Fatal("timestamp not refreshed across days")
		}
		b, err := hex.DecodeString(part)
		if err != nil {
			t.Fatal(err)
		}
		var packed uint64
		for _, v := range b {
			packed = packed<<8 | uint64(v)
		}
		// Official packing retains the low 36 bits of milliseconds. Compare
		// modular age rather than interpreting the truncated value as Unix time.
		stamp := (^packed & 0xffffffffffff) >> 12
		age := (uint64(now.UTC().Truncate(24*time.Hour).UnixMilli()) - stamp) & ((1 << 36) - 1)
		if age > uint64(30*24*time.Hour/time.Millisecond) {
			t.Fatal("session timestamp outside accepted window")
		}
	}
}

func TestRootChildIdentityKeepsAffinityAndCallerIsolation(t *testing.T) {
	h := http.Header{"X-Opencode-Session": {"root"}, "X-Opencode-Session-Id": {"child"}, "X-Parent-Session-Id": {"parent"}}
	request := provider.ServeRequest{Headers: h, Caller: provider.Caller{AppID: 1}}
	first := deriveCallerRequestIDs(request)
	if first.Session == first.ChildSession || first.ParentSession == "" {
		t.Fatal("lost root or child", first)
	}
	h.Set("X-Opencode-Session-Id", "other-child")
	second := deriveCallerRequestIDs(request)
	if first.Session != second.Session || first.ChildSession == second.ChildSession {
		t.Fatal("child changed root affinity")
	}
	request.Caller.AppID = 2
	third := deriveCallerRequestIDs(request)
	if third.Session == second.Session || third.ChildSession == second.ChildSession || third.ParentSession == second.ParentSession {
		t.Fatal("caller isolation lost")
	}
	req, err := newUpstreamRequest(context.Background(), "https://example.invalid", protocol.Chat, []byte(`{}`), first, "public")
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("x-opencode-session") != first.Session || req.Header.Get("x-opencode-session-id") != first.ChildSession || req.Header.Get("x-session-affinity") != first.Session {
		t.Fatal("wire lost root/child distinction")
	}
	for _, name := range []string{"session_id", "conversation_id"} {
		ids := deriveRequestIDs(nil, http.Header{http.CanonicalHeaderKey(name): {"underscore-session"}})
		if ids.Session != CanonicalSessionID("underscore-session") {
			t.Fatal("underscore session header ignored")
		}
	}
}

// E2 回归：会话亲和信号按上游优先级取请求头，回落 body 信号。
func TestDeriveRequestIDsHeaderSignals(t *testing.T) {
	// x-session-id 头优先于 body 信号（客户端显式分离会话）
	h := http.Header{"X-Session-Id": []string{"ses_012345678ffeABCDEFGHIJKLMN"}}
	ids := deriveRequestIDs(map[string]any{"conversation_id": "other"}, h)
	if ids.Session != "ses_012345678ffeABCDEFGHIJKLMN" {
		t.Fatalf("header 信号应优先, got %q", ids.Session)
	}
	// 上游头优先级：x-opencode-session > x-session-affinity > X-Session-Id > x-session-id > conversation-id
	h = http.Header{
		"Conversation-Id":    []string{"low"},
		"X-Session-Id":       []string{"mid"},
		"X-Opencode-Session": []string{"high"},
	}
	ids = deriveRequestIDs(map[string]any{}, h)
	if got := CanonicalSessionID("high"); ids.Session != got {
		t.Fatalf("x-opencode-session 应最优先, got %q want %q", ids.Session, got)
	}
	// 无头时回落 body 信号（原有行为不变）
	ids = deriveRequestIDs(map[string]any{"metadata": map[string]any{"session_id": "body-sig"}}, nil)
	if got := CanonicalSessionID("body-sig"); ids.Session != got {
		t.Fatalf("body 信号回落失败, got %q want %q", ids.Session, got)
	}
}

func TestCanonicalSessionIDPassesValidIDUnchanged(t *testing.T) {
	valid := "ses_012345678ffe" + "ABCDEFGHIJKLMN" // ses_ + 12 hex + 14 base62
	if !canonicalSessionPattern.MatchString(valid) {
		t.Fatalf("test fixture is not a canonical session id: %q", valid)
	}
	if got := CanonicalSessionID(valid); got != valid {
		t.Fatalf("canonical id was rewritten: got %q want %q", got, valid)
	}
}

func TestCanonicalSessionIDHashesNonCanonicalIntoShape(t *testing.T) {
	for _, in := range []string{
		"not-a-session",
		"550e8400-e29b-41d4-a716-446655440000",
		"conversation-42",
		`[{"role":"user","content":"hello"}]`,
	} {
		got := CanonicalSessionID(in)
		if !canonicalSessionPattern.MatchString(got) {
			t.Fatalf("hashed session id %q does not match canonical shape (from %q)", got, in)
		}
		if again := CanonicalSessionID(in); again != got {
			t.Fatalf("hashing is not deterministic: %q vs %q", got, again)
		}
	}
}

func TestNodePoolCursorForIsDeterministicPerSession(t *testing.T) {
	transports, err := newTransportPool([]string{"direct", "direct", "direct"}, PerformanceConfig{}, 0)
	if err != nil {
		t.Fatalf("newTransportPool: %v", err)
	}
	pool, err := newNodePool([]string{"k1", "k2", "k3", "k4", "k5"}, transports, time.Second)
	if err != nil {
		t.Fatalf("newNodePool: %v", err)
	}

	const session = "ses_012345678ffeABCDEFGHIJKLMN"
	first := pool.CursorFor(session)
	second := pool.CursorFor(session)
	if first.next != second.next {
		t.Fatalf("same session yielded different starting cursors: %d vs %d", first.next, second.next)
	}

	// The starting index must be the FNV-64a hash of the session over the node count.
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(session))
	want := int(hash.Sum64() % uint64(pool.Len()))
	if first.next != want {
		t.Fatalf("cursor start = %d, want FNV-derived %d", first.next, want)
	}

	// A different session should (for this fixture) land on a different node,
	// proving the start depends on the affinity key rather than being constant.
	other := pool.CursorFor("ses_ffffffffffffZZZZZZZZZZZZZZ")
	if other.next == first.next {
		t.Logf("note: distinct sessions collided on node %d (acceptable, hash-dependent)", first.next)
	}
}

func TestClaudeSessionHeaderAndAuthenticatedIsolation(t *testing.T) {
	h := http.Header{}
	h.Set("X-Claude-Code-Session-Id", "cc-session")
	h.Set("X-Session-Id", "legacy-session")
	if ids := deriveRequestIDs(map[string]any{}, h); ids.Session != CanonicalSessionID("cc-session") {
		t.Fatal("CC session ignored")
	}
	h.Set("X-Opencode-Session", "native-session")
	if ids := deriveRequestIDs(map[string]any{}, h); ids.Session != CanonicalSessionID("native-session") {
		t.Fatal("native priority changed")
	}
	req := provider.ServeRequest{Caller: provider.Caller{AppID: 1, AppName: "same-name"}, Headers: h, Payload: map[string]any{"model": "opencode/model", "metadata": map[string]any{"parent_session_id": "native-session"}}}
	first := deriveCallerRequestIDs(req)
	if !canonicalSessionPattern.MatchString(first.Session) || first.ParentSession != first.Session {
		t.Fatal("canonical parent/session mapping differs", first)
	}
	h.Set("X-Request-Id", "another-request")
	if deriveCallerRequestIDs(req).Session != first.Session {
		t.Fatal("request ID changed affinity")
	}
	req.Caller.AppID = 2
	if deriveCallerRequestIDs(req).Session == first.Session {
		t.Fatal("affinity crosses keys")
	}
	h.Del("X-Opencode-Session")
	h.Set("X-Claude-Code-Session-Id", strings.Repeat("x", 513))
	if provider.HeaderSessionID(h) != "legacy-session" {
		t.Fatal("unbounded session accepted")
	}
}

func TestSystemOneExternalProtocolIsNotChat(t *testing.T) {
	if externalProtocol(provider.ProtocolSystemOne) != protocol.SystemOne {
		t.Fatal("decision protocol downgraded to chat")
	}
}
