package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

type SessionHeaders interface{ Get(string) string }

// BoundedSessionID rejects malformed or unbounded client identifiers.
func BoundedSessionID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 512 {
		return ""
	}
	for _, c := range value {
		if c < 32 || c == 127 {
			return ""
		}
	}
	return value
}

// HeaderSessionID reads the original request, never a per-request correlation ID.
func HeaderSessionID(headers SessionHeaders) string {
	if headers == nil {
		return ""
	}
	for _, name := range []string{"X-Claude-Code-Session-Id", "X-Session-Id", "Session-Id", "X-Conversation-Id", "Conversation-Id"} {
		if value := BoundedSessionID(headers.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func ExplicitSessionID(headers SessionHeaders, body map[string]any) string {
	if id := HeaderSessionID(headers); id != "" {
		return id
	}
	for _, key := range []string{"session_id", "conversation_id"} {
		if value, _ := body[key].(string); BoundedSessionID(value) != "" {
			return BoundedSessionID(value)
		}
	}
	if meta, ok := body["metadata"].(map[string]any); ok {
		for _, key := range []string{"conversation_id", "conversationId", "session_id"} {
			if value, _ := meta[key].(string); BoundedSessionID(value) != "" {
				return BoundedSessionID(value)
			}
		}
	}
	return ""
}

// ScopedSessionID never retains credentials. Authenticated IDs take precedence;
// direct runtime callers without a principal use a hashed credential fallback.
func ScopedSessionID(caller Caller, headers SessionHeaders, channel, model, signal string) string {
	credential := ""
	if caller.AppID == 0 && caller.UserID == 0 && headers != nil {
		sum := sha256.Sum256([]byte(headers.Get("Authorization") + "\x00" + headers.Get("X-Api-Key")))
		credential = hex.EncodeToString(sum[:])
	}
	data, _ := json.Marshal([]any{channel, caller.AppID, caller.UserID, caller.AppName, credential, model, signal})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
