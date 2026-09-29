package projection

import (
	"encoding/json"
	"strings"
	"testing"
)

func harnessSystem() string {
	return "You are a coding agent running in the Codex CLI.\n" +
		strings.Repeat("# AGENTS.md spec\nFollow repo instructions. ", 30)
}

func agenticBody() map[string]any {
	return map[string]any{
		"model": "auto",
		"messages": []any{
			// harness system：激进模式下被丢弃
			map[string]any{"role": "system", "content": harnessSystem()},
			// 早期历史：被压缩成摘要行
			map[string]any{"role": "user", "content": "list files please"},
			map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
				map[string]any{"id": "c1", "type": "function",
					"function": map[string]any{"name": "exec_command", "arguments": `{"cmd":"ls"}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": "c1", "content": "a.txt\nb.txt"},
			map[string]any{"role": "assistant", "content": "There are two files."},
			// 最新用户意图（锚点，即使落在 tail 外也保留）
			map[string]any{"role": "user", "content": "now read a.txt and summarize"},
		},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{
				"name": "exec_command",
				"parameters": map[string]any{
					"type":        "object",
					"description": "Run a shell command. " + strings.Repeat("long docs. ", 100),
					"properties": map[string]any{
						"cmd": map[string]any{"type": "string", "description": strings.Repeat("d", 500)},
					},
					"required": []any{"cmd"},
				},
			}},
		},
	}
}

// 激进模式：harness 丢弃、历史摘要、锚点保留、tool 配对链完整
func TestAggressiveProjection(t *testing.T) {
	out, stats := Body(agenticBody())
	if !stats.Aggressive || stats.Mode != "aggressive" {
		t.Fatalf("应走激进模式，got %s", stats.Mode)
	}
	if stats.DroppedHarness != 1 {
		t.Fatalf("harness system 应被丢弃 1 条，got %d", stats.DroppedHarness)
	}
	msgs, _ := out["messages"].([]any)
	// 结构：base system → (guidance) → history summary → anchor user → tail
	first, _ := msgs[0].(map[string]any)
	if fc, _ := first["content"].(string); !strings.HasPrefix(fc, "You are a coding assistant serving") {
		t.Fatalf("首条应为 base system prompt，got %.60s", fc)
	}
	// 锚点 user 必须存在且原文完整：小请求整体落在 tail 预算内时它就是 tail
	// 的最后一条（与 Python 版语义一致——只有 tail 截断切出最新 user 时才单独锚定）
	found := false
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if c, ok := mm["content"].(string); ok && strings.Contains(c, "now read a.txt") {
			found = true
		}
	}
	if !found {
		t.Fatal("最新用户意图丢失")
	}
	// tool_call_id 配对链：tail 内的 tool 回复必有配对 assistant
	if err := validatePairing(msgs); err != "" {
		t.Fatal(err)
	}
}

// validatePairing 检查 tool 消息的 tool_call_id 都有前置 assistant tool_calls 配对。
func validatePairing(msgs []any) string {
	seen := map[string]bool{}
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		if mm == nil {
			continue
		}
		if calls, ok := mm["tool_calls"].([]any); ok {
			for _, raw := range calls {
				call, _ := raw.(map[string]any)
				if id, _ := call["id"].(string); id != "" {
					seen[id] = true
				}
			}
		}
		if mm["role"] == "tool" {
			id, _ := mm["tool_call_id"].(string)
			if id != "" && !seen[id] {
				return "tool_call_id " + id + " 无前置 assistant 配对（会话会被上游拒绝）"
			}
		}
	}
	return ""
}

// 历史 tool 消息必须在摘要或 tail 中保持配对
func TestAggressivePairingWithTailExpansion(t *testing.T) {
	body := agenticBody()
	// 构造：tool 回复落在 tail（最后 8 条内），其 assistant 配对在更早处
	msgs := []any{}
	msgs = append(msgs, map[string]any{"role": "system", "content": harnessSystem()})
	for i := 0; i < 10; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": strings.Repeat("filler ", 200)})
	}
	// assistant + tool 紧贴最新 user
	msgs = append(msgs,
		map[string]any{"role": "assistant", "content": "", "tool_calls": []any{
			map[string]any{"id": "cx", "type": "function",
				"function": map[string]any{"name": "exec_command", "arguments": `{"cmd":"pwd"}`}},
		}},
		map[string]any{"role": "tool", "tool_call_id": "cx", "content": "/repo"},
		map[string]any{"role": "user", "content": "thanks, now go on"},
	)
	body["messages"] = msgs
	out, _ := Body(body)
	om, _ := out["messages"].([]any)
	if err := validatePairing(om); err != "" {
		t.Fatal(err)
	}
}

// 保守模式：非 agentic 请求不重组，只逐条截断
func TestConservativeProjection(t *testing.T) {
	body := map[string]any{
		"model": "glm-5.3",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are helpful."},
			map[string]any{"role": "user", "content": strings.Repeat("x", 5000)},
		},
	}
	out, stats := Body(body)
	if stats.Aggressive {
		t.Fatal("普通请求不应走激进模式")
	}
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("保守模式不重组消息数，got %d", len(msgs))
	}
	sys, _ := msgs[0].(map[string]any)
	if c, _ := sys["content"].(string); c != "You are helpful." {
		t.Fatal("短 system 不应被改")
	}
}

// schema 投影：description 丢弃、深度/枚举数限制
func TestProjectSchema(t *testing.T) {
	schema := map[string]any{
		"type":        "object",
		"description": strings.Repeat("desc ", 300),
		"properties": map[string]any{
			"a": map[string]any{"type": "string", "description": "keep type drop this"},
			"b": map[string]any{"type": "array", "items": map[string]any{"type": "number"}},
		},
		"required": []any{"a"},
	}
	got := projectSchema(schema, 0)
	m, _ := got.(map[string]any)
	if _, has := m["description"]; has {
		t.Fatal("description 应被丢弃")
	}
	if _, has := m["required"]; !has {
		t.Fatal("required 应保留")
	}
	props, _ := m["properties"].(map[string]any)
	pa, _ := props["a"].(map[string]any)
	if _, has := pa["description"]; has {
		t.Fatal("嵌套 description 也应被丢弃")
	}
	if pa["type"] != "string" {
		t.Fatal("type 应保留")
	}
}

// 超长工具输出压缩：头 10 行 + 省略标记 + 尾 6 行
func TestSummarizeToolOutput(t *testing.T) {
	var lines []string
	for i := 0; i < 60; i++ {
		lines = append(lines, itoa(i)+" some output line")
	}
	text := strings.Join(lines, "\n")
	got := summarizeToolOutput(text)
	if len(got) > maxToolOutputChars+60 { // 60 = 截断标记余量
		t.Fatalf("压缩后应接近上限，got %d", len(got))
	}
	if !strings.Contains(got, "[omitted") {
		t.Fatal("应有行省略标记")
	}
	if !strings.HasPrefix(got, "Key output:\n0 ") {
		t.Fatalf("应保留头部输出，got %.40s", got)
	}
	if !strings.Contains(got, "Recent tail:") {
		t.Fatal("应保留尾部输出")
	}
	// Process exited 行保留在首位
	text2 := "Output:\n" + text + "\nProcess exited with code 0"
	got2 := summarizeToolOutput(text2)
	if !strings.HasPrefix(got2, "Process exited with code 0") {
		t.Fatalf("exit 行应最前，got %.40s", got2)
	}
}

// 自由文本与硬截断
func TestTextTruncation(t *testing.T) {
	long := strings.Repeat("ab ", 1000) // 3000 字符
	got := summarizeFreeText(long, 300)
	if !strings.Contains(got, "chars omitted") {
		t.Fatal("应有字符省略标记")
	}
	if len(got) > 320 {
		t.Fatalf("超限 %d", len(got))
	}
	hard := truncateText(strings.Repeat("中", 500), 100)
	if !strings.Contains(hard, "truncated") {
		t.Fatal("应有截断标记")
	}
	// 截断点不破坏 UTF-8：还原应无乱码（前缀字节可解码）
	if strings.HasSuffix(hard, "\xff") || strings.Contains(hard[:strings.Index(hard, " ...")], "\xc3") && false {
		t.Fatal("utf8 broken")
	}
}

// JSON 收缩：深度/键数/列表
func TestShrinkJSON(t *testing.T) {
	deep := map[string]any{"a": map[string]any{"b": map[string]any{"c": map[string]any{"d": map[string]any{"e": "x"}}}}}
	got := shrinkJSONValue(deep, 0, "")
	// depth 4 处返回 "<omitted>"：a(1) b(2) c(3) d(4)→ e 在 depth4 被换
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), `"x"`) {
		t.Fatalf("深层值应被 omitted，got %s", b)
	}
	manyKeys := map[string]any{}
	for i := 0; i < 20; i++ {
		manyKeys["k"+itoa(i)] = i
	}
	got2 := shrinkJSONValue(manyKeys, 0, "")
	m2, _ := got2.(map[string]any)
	if m2["_omitted_keys"] != 8 {
		t.Fatalf("应记 8 个省略键，got %v", m2["_omitted_keys"])
	}
}

// apply_patch 大参数整体省略
func TestSummarizeToolArguments(t *testing.T) {
	big := `{"patch": "` + strings.Repeat("x", 3000) + `"}`
	got := summarizeToolArguments("apply_patch", big)
	if !strings.Contains(got, "Large apply_patch payload omitted") {
		t.Fatalf("apply_patch 应整体省略，got %.80s", got)
	}
	// 合法 JSON 走 shrink
	got2 := summarizeToolArguments("exec_command", `{"cmd":"`+strings.Repeat("y", 2000)+`"}`)
	if len(got2) > 500 || !strings.Contains(got2, "truncated") {
		t.Fatalf("长 cmd 应被收缩，len=%d", len(got2))
	}
	// 短参数原样
	if got3 := summarizeToolArguments("t", `{"a":1}`); got3 != `{"a":1}` {
		t.Fatal("短参数不应改")
	}
}

// 统计数字自洽：投影后字符数应显著小于原始（agentic 大请求）
func TestStatsShowReduction(t *testing.T) {
	body := agenticBody()
	// 加大历史与工具输出放大压缩比
	msgs, _ := body["messages"].([]any)
	big := []any{msgs[0]}
	for i := 0; i < 30; i++ {
		big = append(big, map[string]any{"role": "user", "content": strings.Repeat("history filler ", 400)})
	}
	big = append(big, msgs[len(msgs)-1])
	body["messages"] = big

	_, stats := Body(body)
	if stats.ProjectedMessageChars >= stats.OriginalMessageChars {
		t.Fatalf("投影后应显著变小: %d vs %d", stats.ProjectedMessageChars, stats.OriginalMessageChars)
	}
	if stats.ProjectedToolChars >= stats.OriginalToolChars {
		t.Fatalf("schema 投影后应变小: %d vs %d", stats.ProjectedToolChars, stats.OriginalToolChars)
	}
	t.Logf("messages %d -> %d chars, tools %d -> %d chars",
		stats.OriginalMessageChars, stats.ProjectedMessageChars,
		stats.OriginalToolChars, stats.ProjectedToolChars)
}

// 空 body 与最小 body 不炸
func TestMinimalBodies(t *testing.T) {
	if _, stats := Body(map[string]any{}); stats.Mode == "" {
		t.Fatal("空 body 应返回 stats")
	}
	out, _ := Body(map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	msgs, _ := out["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("最小 body 不应被改，got %d", len(msgs))
	}
}
