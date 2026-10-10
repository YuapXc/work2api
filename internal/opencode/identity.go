package opencode

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"

	"work2api/internal/core/provider"
	"work2api/internal/jsonutil"
)

// RequestIDs are the correlation identifiers sent on upstream requests.
type RequestIDs struct {
	Session       string
	Request       string
	Project       string
	ParentSession string
}

// deriveRequestIDs builds the correlation IDs from the client's session
// affinity signals, headers first (upstream opencode2api identity/request.go
// priority), then in-body signals, then the first user turn so a growing
// conversation stays stable.
//
// Header priority: x-opencode-session, x-session-affinity, then the shared
// Claude Code/common session headers. These let a client explicitly
// separate independent conversations (e.g. Zen free tier needs a canonical
// ses_ id per conversation); body-only signals can't express "same user, new
// conversation".
func deriveRequestIDs(body map[string]any, headers ...Header) RequestIDs {
	var h Header
	if len(headers) > 0 {
		h = headers[0]
	}
	signal := ""
	if h != nil {
		signal = firstNonEmpty(provider.BoundedSessionID(h.Get("x-opencode-session")), provider.BoundedSessionID(h.Get("x-session-affinity")), provider.HeaderSessionID(h))
	}
	if signal == "" {
		signal = jsonutil.FirstString(
			jsonutil.StringAt(body, "conversation_id"),
			jsonutil.StringAt(body, "metadata", "session_id"),
		)
	}
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

// Scope upstream affinity to the authenticated key while keeping the required
// canonical ses_ wire format. Native session headers retain routing precedence.
func deriveCallerRequestIDs(req provider.ServeRequest) RequestIDs {
	ids := deriveRequestIDs(req.Payload, req.Headers)
	caller := req.Caller
	if caller.AppName == "" {
		caller.AppName = req.AppName
	}
	ids.Session = CanonicalSessionID(provider.ScopedSessionID(caller, req.Headers, runtimeName, "", ids.Session))
	if ids.ParentSession != "" {
		ids.ParentSession = CanonicalSessionID(provider.ScopedSessionID(caller, req.Headers, runtimeName, "", CanonicalSessionID(ids.ParentSession)))
	}
	return ids
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

// Header is the minimal header view deriveRequestIDs needs (satisfied by
// http.Header; kept as an interface so tests can inject a plain map).
type Header interface{ Get(string) string }

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
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
