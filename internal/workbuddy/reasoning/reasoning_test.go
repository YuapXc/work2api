package reasoning

import (
	"strings"
	"testing"
)

func eff(model, req string, supported []string) string {
	body := Body{"model": model, "reasoning_effort": req}
	out := NormalizeReasoningEffort(body, map[string][]string{model: supported})
	s, _ := out["reasoning_effort"].(string)
	return s
}

// A1 回归：off 是「不思考」开关而不是最低档。glm-5.2 实测支持 [high,xhigh,off]，
// 客户端要 low/medium 绝不能被降级成 off（思考被整个关掉）。
func TestReasoningEffortNeverDowngradesToOff(t *testing.T) {
	supported := []string{"high", "xhigh", "off"}
	for _, req := range []string{"low", "medium", "minimal"} {
		if got := eff("glm-5.2", req, supported); got != "high" {
			t.Fatalf("req=%s 应抬到最低思考档 high, got %s", req, got)
		}
	}
	// 明确要 off 才给 off
	if got := eff("glm-5.2", "off", supported); got != "off" {
		t.Fatalf("req=off 且支持时应给 off, got %s", got)
	}
	// none 与 off 同义
	if got := eff("glm-5.2", "none", supported); got != "off" {
		t.Fatalf("req=none 应归一到 off, got %s", got)
	}
}

// 请求 off 但模型不支持关思考（onlyReasoning）：实测 bare off 会被上游 11150
// 拒绝，改写为「最低思考档 + thinking:disabled」——真正关掉思考而不是静默强开。
func TestReasoningEffortOffLiftedWhenUnsupported(t *testing.T) {
	body := Body{"model": "m", "reasoning_effort": "off"}
	out := NormalizeReasoningEffort(body, map[string][]string{"m": {"low", "medium", "high"}})
	if out["reasoning_effort"] != "low" {
		t.Fatalf("off 应抬到最低思考档 low, got %v", out["reasoning_effort"])
	}
	th, ok := out["thinking"].(map[string]any)
	if !ok || th["type"] != "disabled" {
		t.Fatalf("应补 thinking:disabled（11150 规避）, got %#v", out["thinking"])
	}
	// 客户端显式带了自己的 thinking：不动它
	body = Body{"model": "m", "reasoning_effort": "off", "thinking": map[string]any{"type": "enabled", "budget_tokens": 4096}}
	out = NormalizeReasoningEffort(body, map[string][]string{"m": {"low", "high"}})
	th, _ = out["thinking"].(map[string]any)
	if th["budget_tokens"] != 4096 {
		t.Fatal("客户端显式 thinking 不应被覆盖")
	}
	// Sanitize 全链路：off 改写后 InjectThinking 应删除被抬的档位、保留 disabled
	body = Body{"model": "deepseek-v4-pro", "reasoning_effort": "off",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	out = Sanitize(body, map[string][]string{"deepseek-v4-pro": {"low", "medium", "high"}})
	if _, has := out["reasoning_effort"]; has {
		t.Fatal("disabled 时 reasoning_effort 应被 InjectThinking 删除")
	}
	th, _ = out["thinking"].(map[string]any)
	if th["type"] != "disabled" {
		t.Fatal("thinking 应保持 disabled")
	}
}

// 模型只有 off（不支持思考）：只能 off。
func TestReasoningEffortOffOnlyModel(t *testing.T) {
	for _, req := range []string{"low", "high", "off"} {
		if got := eff("m", req, []string{"off"}); got != "off" {
			t.Fatalf("req=%s got %s", req, got)
		}
	}
}

// 常规降级：只在思考档里选「不超过请求档的最高档」。
func TestReasoningEffortThinkingDowngrade(t *testing.T) {
	supported := []string{"low", "medium", "high"}
	if got := eff("m", "medium", supported); got != "medium" {
		t.Fatalf("got %s", got)
	}
	if got := eff("m", "minimal", supported); got != "low" {
		t.Fatalf("低于最低档应抬到 low, got %s", got)
	}
	if got := eff("m", "xhigh", supported); got != "high" {
		t.Fatalf("高于最高档应降到 high, got %s", got)
	}
	// 带 off 的支持列表不影响思考档选择
	if got := eff("m", "high", []string{"off", "low", "medium"}); got != "medium" {
		t.Fatalf("got %s", got)
	}
}

// 请求值未知 / 模型不在表内：原样透传，不臆造。
func TestReasoningEffortPassthrough(t *testing.T) {
	body := Body{"model": "unknown-model", "reasoning_effort": "low"}
	out := NormalizeReasoningEffort(body, map[string][]string{"m": {"low"}})
	if out["reasoning_effort"] != "low" {
		t.Fatal("未知模型应透传")
	}
	body = Body{"model": "m", "reasoning_effort": "bogus"}
	out = NormalizeReasoningEffort(body, map[string][]string{"m": {"low", "high"}})
	if out["reasoning_effort"] != "bogus" {
		t.Fatal("未知档位应透传")
	}
}

// A2 回归：11128 校验的是「第一条消息是 system」，不是「存在 system」。
func TestEnsureLeadingSystem(t *testing.T) {
	// [user, system] 顺序：仍需在头部补空 system
	body := Body{"messages": []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "system", "content": "later"},
	}}
	out := EnsureLeadingSystem(body)
	msgs := out["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("应补一条 system, got %d 条", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "" {
		t.Fatalf("补的应是空 system, got %#v", first)
	}
	// 首条已是 system：零改动
	body = Body{"messages": []any{map[string]any{"role": "system", "content": "s"}}}
	out = EnsureLeadingSystem(body)
	if len(out["messages"].([]any)) != 1 {
		t.Fatal("首条是 system 时不应补")
	}
}

// developer→system 归一 + 补首条 system 的组合路径（Sanitize 全链路）。
func TestSanitizeRolesAndLeadingSystem(t *testing.T) {
	body := Body{"messages": []any{
		map[string]any{"role": "user", "content": "hi"},
		map[string]any{"role": "developer", "content": "dev"},
	}}
	out := Sanitize(body, nil)
	msgs := out["messages"].([]any)
	if msgs[0].(map[string]any)["role"] != "system" {
		t.Fatal("首条应为 system")
	}
	// developer 已被改写为 system
	if msgs[2].(map[string]any)["role"] != "system" {
		t.Fatal("developer 应被改写为 system")
	}
	// 补的是空 system，不是 "You are a helpful assistant."
	if c, _ := msgs[0].(map[string]any)["content"].(string); strings.Contains(c, "helpful assistant") {
		t.Fatal("不应再注入自定义兜底文案")
	}
}
