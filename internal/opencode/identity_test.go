package opencode

import (
	"hash/fnv"
	"net/http"
	"strings"
	"testing"
	"time"
	"work2api/internal/core/provider"
)

// E2 回归：会话亲和信号按上游优先级取请求头，回落 body 信号。
func TestDeriveRequestIDsHeaderSignals(t *testing.T) {
	// x-session-id 头优先于 body 信号（客户端显式分离会话）
	h := http.Header{"X-Session-Id": []string{"ses_0123456789abABCDEFGHIJKLMN"}}
	ids := deriveRequestIDs(map[string]any{"conversation_id": "other"}, h)
	if ids.Session != "ses_0123456789abABCDEFGHIJKLMN" {
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
	valid := "ses_0123456789ab" + "ABCDEFGHIJKLMN" // ses_ + 12 hex + 14 base62
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

	const session = "ses_0123456789abABCDEFGHIJKLMN"
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
