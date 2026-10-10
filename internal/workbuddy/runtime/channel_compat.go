package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

func CompatibilityName(level int) string {
	switch level {
	case 1:
		return "identity"
	case 2:
		return "client_metadata"
	default:
		return "original"
	}
}

func ClearCompatibility(info *SessionState) {
	info.CompatUID, info.CompatFingerprint, info.CompatLevel = "", "", 0
	info.Compatibility = ""
}

// Recognize fixed client declarations, never arbitrary mentions in instructions.
func CompatibleIdentity(text string) (string, bool) {
	for _, banner := range []string{
		"You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.",
		"You are Claude Code, Anthropic's official CLI for Claude.",
		"You are a Claude agent, built on Anthropic's Claude Agent SDK.",
		"You are an agent for Claude Code, Anthropic's official CLI for Claude.",
	} {
		if !strings.HasPrefix(text, banner) {
			continue
		}
		tail := text[len(banner):]
		if tail != "" && !strings.HasPrefix(tail, "\n") && !strings.HasPrefix(tail, "\r\n") && !(strings.HasPrefix(banner, "You are an agent for") && strings.HasPrefix(tail, " Given the user's message,")) {
			continue
		}
		return "You are a coding assistant." + tail, true
	}
	return text, false
}

func BillingPrefix(text string) (prefix, rest string) {
	line, tail, found := strings.Cut(text, "\n")
	if !strings.HasPrefix(line, "x-anthropic-billing-header: cc_version=") || len(line) > 1024 {
		return "", text
	}
	for _, char := range strings.TrimSuffix(line, "\r") {
		if char < 32 || char > 126 {
			return "", text
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(line), ";") {
		return "", text
	}
	if found {
		return line + "\n", tail
	}
	return line, ""
}

// Transform only leading system text. Identity replacement retains every byte
// after the fixed declaration; level 2 additionally removes client attribution.
// Skills, tools, permission rules, user messages and tool results are untouched.
func ChannelCompatibilityBody(body map[string]any, level int) (map[string]any, bool) {
	identityChanged, metadataChanged := false, false
	transform := func(text string) string {
		prefix, rest := BillingPrefix(text)
		padding := rest[:len(rest)-len(strings.TrimLeft(rest, "\r\n"))]
		value, changed := CompatibleIdentity(strings.TrimLeft(rest, "\r\n"))
		identityChanged = identityChanged || changed
		if level == 2 && prefix != "" {
			metadataChanged = true
			prefix = ""
		}
		return prefix + padding + value
	}
	messages, _ := body["messages"].([]any)
	copyMessages := append([]any(nil), messages...)
	for i, item := range messages {
		message, ok := item.(map[string]any)
		if !ok || message["role"] != "system" {
			break
		}
		var content any
		switch value := message["content"].(type) {
		case string:
			content = transform(value)
		case []any:
			blocks := append([]any(nil), value...)
			for j, item := range value {
				block, ok := item.(map[string]any)
				if !ok || block["type"] != "text" {
					continue
				}
				text, ok := block["text"].(string)
				if !ok {
					continue
				}
				copyBlock := make(map[string]any, len(block))
				for k, v := range block {
					copyBlock[k] = v
				}
				copyBlock["text"] = transform(text)
				blocks[j] = copyBlock
			}
			content = blocks
		default:
			continue
		}
		copyMessage := make(map[string]any, len(message))
		for k, v := range message {
			copyMessage[k] = v
		}
		copyMessage["content"] = content
		copyMessages[i] = copyMessage
	}
	if level == 1 && !identityChanged || level == 2 && !metadataChanged {
		return body, false
	}
	out := make(map[string]any, len(body))
	for k, v := range body {
		out[k] = v
	}
	out["messages"] = copyMessages
	return out, true
}

func CompatibilityFingerprint(body map[string]any) string {
	normalized, _ := ChannelCompatibilityBody(body, 2)
	messages, _ := normalized["messages"].([]any)
	var system []any
	for _, item := range messages {
		message, ok := item.(map[string]any)
		if !ok || message["role"] != "system" {
			break
		}
		system = append(system, message["content"])
	}
	raw, _ := json.Marshal([]any{system, body["tools"]})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (r *SessionRouter) Compatibility(key, uid, fingerprint string) int {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	e := r.M[key]
	if e.Info != nil && e.Uid == uid && e.Info.CompatUID == uid && e.Info.CompatFingerprint == fingerprint {
		return e.Info.CompatLevel
	}
	return 0
}

func (r *SessionRouter) RememberCompatibility(key, uid, fingerprint string, level int) {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	e := r.M[key]
	if e.Info == nil || e.Uid != uid || e.Info.Pending != "" {
		return
	}
	e.Info.CompatUID, e.Info.CompatFingerprint, e.Info.CompatLevel = uid, fingerprint, level
	e.Info.Compatibility = CompatibilityName(level)
}
