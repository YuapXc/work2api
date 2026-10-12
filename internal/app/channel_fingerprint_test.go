package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"work2api/internal/workbuddy/models"
)

// 全消息指纹脱敏：同一批客户端模板出现在历史、reasoning 或工具载荷里时，最深一档
// （level 2）要一并改写；level 1 保持既有契约，只动开头 system。
func TestChannelFingerprintScrubCoversHistoryAndToolPayloads(t *testing.T) {
	const banner = "You are Claude Code, Anthropic's official CLI for Claude."
	const codexIntro = "You are a coding agent running in the Codex CLI, a terminal-based coding assistant."
	const branch = "Main branch (you will usually use this for PRs): main"
	const feedback = "To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"
	const bareCode = "11128"
	const dashedCode = "11-128"
	if len(bareCode) != 5 || len(dashedCode) != 6 {
		t.Fatal("error-code fixtures must stay the bare and hyphenated forms")
	}
	body := map[string]any{"model": "test-model", "messages": []any{
		map[string]any{"role": "system", "content": banner + "\nKeep every project rule."},
		map[string]any{"role": "user", "content": "<system-reminder>Available skills: example</system-reminder>"},
		map[string]any{"role": "assistant", "content": []any{
			map[string]any{"type": "text", "text": codexIntro, "cache_control": map[string]any{"type": "ephemeral"}},
			map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "AA=="}},
		}, "reasoning_content": branch},
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
			map[string]any{"id": "call-1", "type": "function", "function": map[string]any{"name": "shell", "arguments": `{"command":"rg -n \"` + feedback + `\" README.md"}`}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "call-1", "content": feedback + " returned code " + bareCode},
	}}
	before, _ := json.Marshal(body)

	shallow, matched := channelCompatibilityBody(body, 1)
	if !matched {
		t.Fatal("identity tier missed the leading banner")
	}
	shallowMessages := shallow["messages"].([]any)
	if got := shallowMessages[0].(map[string]any)["content"].(string); !strings.HasPrefix(got, "You are a coding assistant.") {
		t.Fatalf("identity tier did not rewrite the leading banner: %q", got)
	}
	if got := shallowMessages[2].(map[string]any)["reasoning_content"]; got != branch {
		t.Fatalf("identity tier must leave history untouched: %q", got)
	}
	if got := shallowMessages[4].(map[string]any)["content"]; got != feedback+" returned code "+bareCode {
		t.Fatalf("identity tier must leave tool results untouched: %q", got)
	}

	deep, matched := channelCompatibilityBody(body, 2)
	if !matched {
		t.Fatal("fingerprint tier missed the history fingerprints")
	}
	messages := deep["messages"].([]any)
	if got := messages[0].(map[string]any)["content"].(string); got != "You are a coding assistant.\nKeep every project rule." {
		t.Fatalf("fingerprint tier changed the leading system rewrite: %q", got)
	}
	blocks := messages[2].(map[string]any)["content"].([]any)
	text := blocks[0].(map[string]any)["text"].(string)
	if strings.Contains(text, "Codex CLI,") || !strings.Contains(text, "Codex CLI tool,") {
		t.Fatalf("codex intro not de-fingerprinted: %q", text)
	}
	if blocks[0].(map[string]any)["cache_control"] == nil || blocks[1].(map[string]any)["type"] != "image" {
		t.Fatal("fingerprint tier dropped block metadata or image content")
	}
	if got := messages[2].(map[string]any)["reasoning_content"].(string); got != strings.Replace(branch, "Main branch", "Default branch", 1) {
		t.Fatalf("reasoning fingerprint survived: %q", got)
	}
	args := messages[3].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	if strings.Contains(args, "anthropics/claude-code/issues") || !strings.Contains(args, "anthropics/cl4ude-c0de/issues") {
		t.Fatalf("tool arguments not de-fingerprinted: %q", args)
	}
	if got := messages[4].(map[string]any)["content"].(string); strings.Contains(got, bareCode) || !strings.Contains(got, dashedCode) {
		t.Fatalf("error-code fingerprint survived: %q", got)
	}
	after, _ := json.Marshal(body)
	if string(after) != string(before) {
		t.Fatal("fingerprint tier mutated the caller body")
	}
	if _, again := channelCompatibilityBody(deep, 2); again {
		t.Fatal("fingerprint tier is not idempotent")
	}
}

// 脱敏只在被渠道拒绝之后才上线：前两次尝试保持原文，第三次（level 2）才改写历史。
func TestChannelFingerprintRetryScrubsHistoryAfterDenial(t *testing.T) {
	const denial = `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`
	const banner = "You are Claude Code, Anthropic's official CLI for Claude."
	const buried = "To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues"
	client := &channelRetryUpstream{body: denial, denyCalls: 2}
	o, first := newDNSFailoverOrch(t, &failingUpstream{})
	o.wb.UpstreamClient = client
	first.Profile = "cn-cli"
	o.wb.Catalog = models.NewWithCatalogClient(o.wb.Pool, o.db, &http.Client{Transport: catalogTransport{}})
	o.wb.Catalog.Refresh()
	o.cfg.ChannelIdentityCompat = true
	o.cfg.ChannelMetadataCompat = true
	body := map[string]any{"model": "test-model", "messages": []any{
		map[string]any{"role": "system", "content": banner + "\nKeep every project rule."},
		map[string]any{"role": "user", "content": "<system-reminder>Available skills: example</system-reminder>"},
		map[string]any{"role": "assistant", "content": buried},
	}}
	before, _ := json.Marshal(body)
	ctx, finish := o.sessions.Begin(context.Background(), "scrub", "test-model", nil, body)
	defer finish()
	o.sessions.Bind("scrub", first.UID)
	_, err := o.openUpstreamScoped(ctx, first, body, "test-model", "scrub", func(string) error { return nil }, nil, map[string]bool{first.UID: true})
	if err != nil || client.calls != 3 {
		t.Fatalf("denial then deep retry should succeed: calls=%d err=%v", client.calls, err)
	}
	history := func(index, message int) string {
		return client.bodies[index]["messages"].([]any)[message].(map[string]any)["content"].(string)
	}
	if history(0, 2) != buried || history(1, 2) != buried {
		t.Fatal("history rewritten before the deepest tier")
	}
	if got := history(2, 2); got == buried || !strings.Contains(got, "cl4ude-c0de") {
		t.Fatalf("deep tier did not scrub history: %q", got)
	}
	after, _ := json.Marshal(body)
	if string(after) != string(before) {
		t.Fatal("retry ladder mutated the caller body")
	}
}
