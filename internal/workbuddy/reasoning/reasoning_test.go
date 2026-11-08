package reasoning

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func BenchmarkUpstreamHistoryGuards(b *testing.B) {
	for _, count := range []int{256, 2048} {
		messages := make([]any, count)
		for i := range messages {
			role := "user"
			if i%2 == 1 {
				role = "assistant"
			}
			messages[i] = map[string]any{"role": role, "content": strings.Repeat("synthetic context ", 128)}
		}
		body := Body{"model": "glm-5.3-flash", "messages": messages}
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				out := MirrorUpstreamReasoning(body)
				GuardEmptyTextParts(out, false)
			}
		})
	}
}

func TestWorkBuddyDeepseekReasoningReplay(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields map[string]any
		want   string
	}{
		{"alias", map[string]any{"reasoning_content": "recorded thought"}, "recorded thought"},
		{"canonical wins", map[string]any{"reasoning": "canonical", "reasoning_content": "alias"}, "canonical"},
		{"details", map[string]any{"reasoning_details": []any{map[string]any{"text": "first"}, map[string]any{"summary": "second"}}}, "firstsecond"},
		{"empty", map[string]any{"reasoning_content": ""}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message := map[string]any{"role": "assistant", "content": "answer", "tool_calls": []any{map[string]any{"id": "c1", "function": map[string]any{"name": "report", "arguments": `{"value":"recorded thought"}`}}}}
			for key, value := range tc.fields {
				message[key] = value
			}
			body := Body{"model": "deepseek-v4.1-flash", "messages": []any{map[string]any{"role": "user", "content": "keep", "reasoning_content": "user data"}, message, map[string]any{"role": "tool", "tool_call_id": "c1", "content": "result"}}}
			before, _ := json.Marshal(body)
			out := BackfillReasoningContent(body)
			got := out["messages"].([]any)[1].(map[string]any)
			if got["reasoning"] != tc.want || got["content"] != "answer" || !reflect.DeepEqual(got["tool_calls"], message["tool_calls"]) {
				t.Fatal("reasoning or tool contract changed", got)
			}
			if _, exists := got["reasoning_content"]; exists {
				t.Fatal("duplicate replay text")
			}
			after, _ := json.Marshal(body)
			if string(before) != string(after) || !reflect.DeepEqual(out["messages"].([]any)[0], body["messages"].([]any)[0]) {
				t.Fatal("source request mutated")
			}
			if !reflect.DeepEqual(BackfillReasoningContent(out), out) {
				t.Fatal("not idempotent")
			}
			body["model"] = "other-model"
			if !reflect.DeepEqual(BackfillReasoningContent(body), body) {
				t.Fatal("other model changed")
			}
		})
	}
	opaque := Body{"model": "deepseek-v4.1-flash", "messages": []any{map[string]any{"role": "assistant", "reasoning_details": []any{map[string]any{"type": "reasoning.encrypted", "data": "opaque"}}}}}
	if !reflect.DeepEqual(BackfillReasoningContent(opaque), opaque) {
		t.Fatal("opaque reasoning interpreted as text")
	}
}

func TestRepairToolHistoryMatchesOnlyExactIDs(t *testing.T) {
	call := func(id, name string) any {
		return map[string]any{"id": id, "type": "function", "function": map[string]any{"name": name, "arguments": `{}`}}
	}
	assistant := map[string]any{"role": "assistant", "content": "Keep explanation", "tool_calls": []any{call("read", "read_file"), call("bad", ""), call("", "shell"), call("exec", "shell")}}
	orphan := map[string]any{"role": "tool", "tool_call_id": "bad", "content": "NOT A SHELL RESULT"}
	exec := map[string]any{"role": "tool", "tool_call_id": "exec", "content": "shell output"}
	read := map[string]any{"role": "tool", "tool_call_id": "read", "content": "file output"}
	body := Body{"messages": []any{assistant, orphan, exec, read, map[string]any{"role": "user", "content": "continue"}}}
	want := []any{map[string]any{"role": "assistant", "content": "Keep explanation", "tool_calls": []any{call("read", "read_file"), call("exec", "shell")}}, exec, read, map[string]any{"role": "user", "content": "continue"}}
	RepairToolSequence(body)
	if !reflect.DeepEqual(body["messages"], want) {
		t.Fatalf("history = %#v", body["messages"])
	}
	if len(assistant["tool_calls"].([]any)) != 4 || orphan["tool_call_id"] != "bad" {
		t.Fatal("repair mutated original call or result")
	}
	RepairToolSequence(body)
	if !reflect.DeepEqual(body["messages"], want) {
		t.Fatal("repair is not idempotent")
	}
}

func TestRepairToolHistoryDoesNotGuessMissingResults(t *testing.T) {
	assistant := map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "real", "function": map[string]any{"name": "read", "arguments": `{}`}}}}
	for _, id := range []string{"wrong", ""} {
		body := Body{"messages": []any{assistant, map[string]any{"role": "tool", "tool_call_id": id, "content": "unrelated secret"}}}
		RepairToolSequence(body)
		messages := body["messages"].([]any)
		if len(messages) != 2 || messages[1].(map[string]any)["tool_call_id"] != "real" || strings.Contains(messages[1].(map[string]any)["content"].(string), "secret") {
			t.Fatalf("unmatched result was misassigned: %#v", messages)
		}
	}
	for _, content := range []any{nil, "", "preserve assistant text", []any{map[string]any{"type": "text", "text": "preserve block"}}} {
		body := Body{"messages": []any{map[string]any{"role": "assistant", "content": content, "tool_calls": []any{map[string]any{"id": "bad"}}}}}
		RepairToolSequence(body)
		messages := body["messages"].([]any)
		if content == nil || content == "" {
			if len(messages) != 0 {
				t.Fatal("empty invalid shell retained")
			}
		} else if len(messages) != 1 || !reflect.DeepEqual(messages[0].(map[string]any)["content"], content) {
			t.Fatal("assistant content lost")
		}
	}
}

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

func TestExplicitOffWithUnknownSupportDisablesDeepseekThinking(t *testing.T) {
	for _, value := range []string{"off", "none"} {
		body := Body{"model": "deepseek-future", "reasoning_effort": value}
		out := Sanitize(body, map[string][]string{"deepseek-future": {}})
		if out["thinking"].(map[string]any)["type"] != "disabled" {
			t.Fatal("explicit off lost")
		}
		if _, present := out["reasoning_effort"]; present {
			t.Fatal("disabled effort not removed")
		}
	}
}

func TestFamilyReasoningMirrorAndEmptyTextBoundaries(t *testing.T) {
	for _, model := range []string{"kimi", "kimi-k3.1-pro", "glm-5.3-flash", "minimax-m3.1", "mimo-v2", "step-5", "space-bunny-sg", "kimono", "glmish", "stepfather", "hy4-preview", "gpt-5"} {
		original := Body{"model": model, "messages": []any{map[string]any{"role": "assistant", "content": "answer", "reasoning_content": " thought "}, map[string]any{"role": "assistant", "content": "native", "reasoning": "canonical", "reasoning_content": "alias"}, map[string]any{"role": "assistant", "reasoning_content": " \t "}, map[string]any{"role": "user", "reasoning_content": "user data"}}}
		before, _ := json.Marshal(original)
		out := MirrorUpstreamReasoning(original)
		msgs := out["messages"].([]any)
		want := model == "kimi" || strings.HasPrefix(model, "kimi-") || strings.HasPrefix(model, "glm-") || strings.HasPrefix(model, "minimax-") || strings.HasPrefix(model, "mimo-") || strings.HasPrefix(model, "step-") || strings.HasPrefix(model, "space-bunny-")
		if (msgs[0].(map[string]any)["reasoning"] == " thought ") != want || msgs[1].(map[string]any)["reasoning"] != "canonical" || msgs[2].(map[string]any)["reasoning"] != nil || msgs[3].(map[string]any)["reasoning"] != nil {
			t.Fatal(model, out)
		}
		after, _ := json.Marshal(original)
		if string(before) != string(after) || !reflect.DeepEqual(out, MirrorUpstreamReasoning(out)) {
			t.Fatal("source mutation or repeated mirror", model)
		}
	}
	for _, content := range []any{
		[]any{map[string]any{"type": "text", "text": "", "tag": "keep"}, map[string]any{"type": "image_url", "image_url": "https://example.test/image"}, map[string]any{"type": "text", "text": "Next Step"}},
		` [{"type":"text","text":null,"integer":9007199254740993},{"type":"text","text":"Next Step"}]`,
		`[{"type":"text"}]`,
	} {
		original := Body{"messages": []any{map[string]any{"role": "tool", "content": content, "tool_call_id": "call"}}, "tools": []any{map[string]any{"const": content}}}
		before, _ := json.Marshal(original)
		out, changed := GuardEmptyTextParts(original, true)
		if !changed {
			t.Fatal("empty part not repaired", content)
		}
		raw, _ := json.Marshal(out)
		if !strings.Contains(string(raw), "Next Step") && strings.Contains(string(before), "Next Step") {
			t.Fatal("sibling lost")
		}
		if strings.Contains(string(before), "9007199254740993") && !strings.Contains(string(raw), "9007199254740993") {
			t.Fatal("business number rounded")
		}
		again, changed := GuardEmptyTextParts(out, true)
		if changed || !reflect.DeepEqual(again, out) {
			t.Fatal("not idempotent")
		}
		after, _ := json.Marshal(original)
		if string(before) != string(after) || !reflect.DeepEqual(original["tools"], out["tools"]) {
			t.Fatal("business input mutated")
		}
		if _, isString := content.(string); isString {
			if out, changed := GuardEmptyTextParts(original, false); changed || !reflect.DeepEqual(out, original) {
				t.Fatal("JSON string sniffed without rejection")
			}
		}
	}
	for _, content := range []string{`[{"type":"text","text":"   "}]`, `[{"text":""}]`, `{"type":"text","text":""}`, `["",{"type":"text","text":""}]`, `[{"type":"text","text":0}]`, `[{"type":"text","text":""}] trailing`, `[invalid`, "plain text"} {
		body := Body{"messages": []any{map[string]any{"role": "tool", "content": content}}}
		if _, changed := GuardEmptyTextParts(body, true); changed {
			t.Fatal("ordinary business content changed", content)
		}
	}
}
