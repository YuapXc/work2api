package opencode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"

	"work2api/internal/jsonutil"
)

// RequestIDs are the correlation identifiers sent on upstream requests.
type RequestIDs struct {
	Session       string
	Request       string
	Project       string
	ParentSession string
}

// deriveRequestIDs builds the correlation IDs from the client payload alone.
//
// Deviation from opencode2api: the shared Runtime contract hands us the parsed
// payload but not the raw *http.Request, so the x-opencode-*/x-session-* header
// signals are unavailable. Session affinity therefore derives from in-body
// signals (conversation_id, metadata.session_id, previous_response_id) and, as
// upstream does, the first user turn so a growing conversation stays stable.
func deriveRequestIDs(body map[string]any) RequestIDs {
	signal := jsonutil.FirstString(
		jsonutil.StringAt(body, "conversation_id"),
		jsonutil.StringAt(body, "metadata", "session_id"),
	)
	if signal == "" {
		signal = conversationSeed(body)
	}
	if signal == "" {
		signal = jsonutil.StringAt(body, "previous_response_id")
	}
	if signal == "" || signal == `{}` {
		signal = RandomID("fallback", 16)
	}
	session := CanonicalSessionID(signal)
	projectSignal := jsonutil.StringAt(body, "metadata", "project_id")
	if projectSignal == "" {
		projectSignal = "work2api-opencode:default-project"
	}
	parentSession := jsonutil.StringAt(body, "metadata", "parent_session_id")
	return RequestIDs{
		Session:       session,
		Request:       RandomID("req", 16),
		Project:       StableID("prj", projectSignal),
		ParentSession: parentSession,
	}
}

func conversationSeed(body map[string]any) string {
	if input, ok := body["input"].(string); ok && input != "" {
		return input
	}
	for _, field := range []string{"messages", "input"} {
		for _, raw := range jsonutil.SliceAt(body, field) {
			item, ok := raw.(map[string]any)
			if !ok || jsonutil.StringAt(item, "role") != "user" {
				continue
			}
			encoded, _ := json.Marshal(item["content"])
			if len(encoded) > 0 && string(encoded) != "null" {
				return string(encoded)
			}
		}
	}
	return ""
}

// StableID hashes value into a deterministic prefixed id.
func StableID(prefix, value string) string {
	sum := sha256.Sum256([]byte(prefix + "\x00" + value))
	return prefix + "_" + hex.EncodeToString(sum[:12])
}

// canonicalSessionPattern matches OpenCode's canonical session format:
// "ses_" + 12 lowercase hex characters + 14 Base62 characters. The Zen free
// tier (Authorization: Bearer public) rejects any other shape with 403.
var canonicalSessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$`)

const base62Alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// CanonicalSessionID returns signal unchanged when it already carries an
// official OpenCode session id (preserving prompt-cache affinity). Any other
// downstream identity is deterministically hashed into the canonical shape.
func CanonicalSessionID(signal string) string {
	if canonicalSessionPattern.MatchString(signal) {
		return signal
	}
	sum := sha256.Sum256([]byte("ses\x00" + signal))
	timePart := hex.EncodeToString(sum[:6])
	randomPart := base62Fixed(new(big.Int).SetBytes(sum[6:16]), 14)
	return "ses_" + timePart + randomPart
}

func base62Fixed(n *big.Int, width int) string {
	base := big.NewInt(62)
	out := make([]byte, width)
	remainder := new(big.Int)
	for i := width - 1; i >= 0; i-- {
		n.DivMod(n, base, remainder)
		out[i] = base62Alphabet[remainder.Int64()]
	}
	return string(out)
}

// RandomID returns a prefixed random hex id.
func RandomID(prefix string, size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return prefix + "_" + hex.EncodeToString(buf)
}
