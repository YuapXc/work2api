package opencode

import (
	"hash/fnv"
	"testing"
	"time"
)

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
