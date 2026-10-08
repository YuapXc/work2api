package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

func compatibilityName(level int) string {
	switch level {
	case 1:
		return "identity"
	case 2:
		return "client_metadata"
	default:
		return "original"
	}
}

func clearCompatibility(info *sessionState) {
	info.compatUID, info.compatFingerprint, info.compatLevel = "", "", 0
	info.Compatibility = ""
}

// Recognize fixed client declarations, never arbitrary mentions in instructions.
func compatibleIdentity(text string) (string, bool) {
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

func billingPrefix(text string) (prefix, rest string) {
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
func channelCompatibilityBody(body map[string]any, level int) (map[string]any, bool) {
	identityChanged, metadataChanged := false, false
	transform := func(text string) string {
		prefix, rest := billingPrefix(text)
		padding := rest[:len(rest)-len(strings.TrimLeft(rest, "\r\n"))]
		value, changed := compatibleIdentity(strings.TrimLeft(rest, "\r\n"))
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

func compatibilityFingerprint(body map[string]any) string {
	normalized, _ := channelCompatibilityBody(body, 2)
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

func (r *sessionRouter) compatibility(key, uid, fingerprint string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.m[key]
	if e.info != nil && e.uid == uid && e.info.compatUID == uid && e.info.compatFingerprint == fingerprint {
		return e.info.compatLevel
	}
	return 0
}

func (r *sessionRouter) rememberCompatibility(key, uid, fingerprint string, level int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.m[key]
	if e.info == nil || e.uid != uid || e.info.Pending != "" {
		return
	}
	e.info.compatUID, e.info.compatFingerprint, e.info.compatLevel = uid, fingerprint, level
	e.info.Compatibility = compatibilityName(level)
}
