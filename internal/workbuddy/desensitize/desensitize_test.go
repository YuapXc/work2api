package desensitize

import (
	"strings"
	"testing"
)

var ccHarnessSample = "You are an interactive agent that helps users with software engineering tasks." + `

# Harness
The harness routes tool calls and reports outcomes.

# Session-specific guidance
When the user types a command, invoke it.

# Memory
Long-term memory is available.

# Environment
The working directory is the project root.

# Context management
When the conversation grows long, context is summarized.` +
	strings.Repeat("\n\nAdditional harness guidance text to push the template length past the 1000 byte threshold used for detection. ", 10)

func TestLooksLikeClaudeHarness(t *testing.T) {
	if !looksLikeClaudeHarness(ccHarnessSample) {
		t.Fatal("CC 2.x harness 模板应被识别（长度>1000 且特征≥2）")
	}
	// 普通短 system prompt 不应误伤
	if looksLikeClaudeHarness("You are Claude Code, be helpful.") {
		t.Fatal("短文本不应被识别为 harness 模板")
	}
	// 长但只有单个特征的普通 prompt 不应误伤
	only := strings.Repeat("x", 1100) + " # Memory"
	if looksLikeClaudeHarness(only) {
		t.Fatal("仅 1 个特征不应被识别为 harness 模板")
	}
}

func TestCompactHarnessCC2x(t *testing.T) {
	out, ok := compactHarnessMessage("system", ccHarnessSample)
	if !ok {
		t.Fatal("CC 2.x 模板应命中压缩")
	}
	if out != claudeHarnessSummary {
		t.Fatalf("应整体替换为行为摘要，got: %s", out[:80])
	}
	if strings.Contains(out, "Harness") {
		t.Fatal("摘要不应保留渠道身份文本")
	}
}

func TestCompactHarnessCodexStillWorks(t *testing.T) {
	out, ok := compactHarnessMessage("system", "You are Claude Code.\n\n"+strings.Repeat("body ", 10))
	if !ok {
		t.Fatal("旧版 Codex/CC 标记仍应命中压缩")
	}
	if !strings.HasPrefix(out, "You are a coding assistant.") {
		t.Fatalf("unexpected: %s", out)
	}
}

func TestExpandedWordList(t *testing.T) {
	for _, term := range []string{"MCP Server", "subagent", "CLAUDE.md", "skill://", "antml:invoke", "Codex CLI"} {
		got := Text("use " + term + " here")
		if strings.Contains(got, term) {
			t.Fatalf("%q 应被零宽空格打断，got %q", term, got)
		}
		if strings.Contains(strings.ReplaceAll(got, zwsp, ""), term) == false {
			t.Fatalf("脱敏后可见字符应不变，got %q", got)
		}
	}
	// 普通中文不受影响
	if got := Text("这是一段正常文本"); got != "这是一段正常文本" {
		t.Fatalf("正常文本不应被改写: %q", got)
	}
}

func TestMessagesAppliesCC2xCompaction(t *testing.T) {
	msgs := []any{
		map[string]any{"role": "system", "content": ccHarnessSample},
		map[string]any{"role": "user", "content": "explain DoS attacks"},
	}
	out := Messages(msgs, Options{Roles: []string{"system", "developer"}, CompactHarness: true})
	sys, _ := out[0].(map[string]any)
	if sys["content"] != claudeHarnessSummary {
		t.Fatalf("system 应被压缩为摘要，got len=%d", len(sys["content"].(string)))
	}
	user, _ := out[1].(map[string]any)
	if strings.Contains(user["content"].(string), zwsp) == false && !strings.Contains(user["content"].(string), "DoS") {
		t.Fatal("user 消息不应丢失")
	}
}
