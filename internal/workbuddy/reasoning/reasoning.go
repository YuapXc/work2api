// Package reasoning normalizes the upstream request body: reasoning_effort is
// downgraded per model capability, tool_choice is coerced to the upstream's
// string form, roles are normalized, tool sequences repaired, and DeepSeek
// thinking is injected. Ported from workbuddy_one/reasoning.py.
package reasoning

import (
	"log"
	"strconv"
	"strings"
)

// effortRank orders effort levels low→high.
var effortRank = map[string]int{
	"off": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6,
}

// KnownEfforts is the cold-start fallback of per-model reasoning support.
// At runtime the dynamic catalog table takes precedence.
var KnownEfforts = map[string][]string{
	"glm-5.2":             {"low", "medium", "high"},
	"glm-5.1":             {"low", "medium", "high"},
	"glm-5v-turbo":        {"low", "medium", "high"},
	"kimi-k2.7":           {"low", "medium", "high"},
	"kimi-k2.6":           {"low", "medium", "high"},
	"kimi-k2.5":           {"low", "medium", "high"},
	"deepseek-v4-pro":     {"off", "low", "medium", "high"},
	"deepseek-v4-flash":   {"off", "low", "medium", "high"},
	"deepseek-v4.1-flash": {"low", "medium", "high"},
	"minimax-m3-pay":      {"low", "medium", "high"},
	"hy3-preview-agent":   {"low", "medium", "high"},
}

// Body is the mutable request body.
type Body = map[string]any

// NormalizeReasoningEffort downgrades reasoning_effort to the model's supported
// levels (snake/camel field compatible).
func NormalizeReasoningEffort(body Body, efforts map[string][]string) Body {
	if efforts == nil {
		efforts = KnownEfforts
	}
	if len(efforts) == 0 {
		return body
	}
	model, _ := body["model"].(string)
	if model == "" {
		return body
	}
	supported, ok := efforts[model]
	if !ok || len(supported) == 0 {
		return body
	}
	key := ""
	for _, k := range []string{"reasoning_effort", "reasoningEffort"} {
		if _, present := body[k]; present {
			key = k
			break
		}
	}
	if key == "" {
		return body
	}
	req := strings.ToLower(strings.TrimSpace(toStr(body[key])))
	reqIdx, ok := effortRank[req]
	if !ok {
		return body
	}
	best, bestIdx := "", -1
	for _, s := range supported {
		idx, ok := effortRank[strings.ToLower(strings.TrimSpace(s))]
		if ok && idx <= reqIdx && idx > bestIdx {
			best, bestIdx = s, idx
		}
	}
	if best != "" {
		if strings.ToLower(best) != req {
			log.Printf("reasoning_effort 降级 model=%s %s -> %s", model, req, best)
			body[key] = best
		}
		return body
	}
	// all supported levels exceed the request: pick the lowest
	lowest, lowestIdx := "", 1<<30
	for _, s := range supported {
		idx, ok := effortRank[strings.ToLower(strings.TrimSpace(s))]
		if !ok {
			idx = 1 << 30
		}
		if idx < lowestIdx {
			lowest, lowestIdx = s, idx
		}
	}
	body[key] = lowest
	return body
}

// NormalizeToolChoice coerces an OpenAI object-form tool_choice into the
// upstream's string form.
func NormalizeToolChoice(body Body) Body {
	tc, present := body["tool_choice"]
	if !present || tc == nil {
		return body
	}
	switch v := tc.(type) {
	case string:
		if v == "none" {
			delete(body, "tools")
			delete(body, "functions")
			delete(body, "tool_choice")
		}
		return body
	case map[string]any:
		t, _ := v["type"].(string)
		switch t {
		case "none":
			delete(body, "tools")
			delete(body, "functions")
			delete(body, "tool_choice")
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name != "" {
				body["tool_choice"] = name
			} else {
				body["tool_choice"] = "auto"
			}
		case "auto", "required":
			body["tool_choice"] = t
		default:
			delete(body, "tool_choice")
		}
		return body
	default:
		delete(body, "tool_choice")
		return body
	}
}

// Sanitize applies all upstream-bound body normalizations.
func Sanitize(body Body, efforts map[string][]string) Body {
	body = NormalizeToolChoice(body)
	body = NormalizeReasoningEffort(body, efforts)
	body = NormalizeRoles(body)
	body = RepairToolSequence(body)
	body = InjectThinking(body)
	body = BackfillReasoningContent(body)
	return body
}

// NormalizeRoles rewrites developer→system and ensures the first message is a
// system message (international site returns 400 code=11128 otherwise).
func NormalizeRoles(body Body) Body {
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body
	}
	hasSystem := false
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if mm["role"] == "developer" {
			mm["role"] = "system"
		}
		if mm["role"] == "system" {
			hasSystem = true
		}
	}
	if !hasSystem {
		sys := map[string]any{"role": "system", "content": "You are a helpful assistant."}
		body["messages"] = append([]any{sys}, msgs...)
	}
	return body
}

// RepairToolSequence repairs tool_calls↔tool result sequences broken by client
// context trimming.
func RepairToolSequence(body Body) Body {
	msgs, ok := body["messages"].([]any)
	if !ok {
		return body
	}
	var out []any
	var pendingIDs []string
	var userBuffer []any

	missingResults := func() []any {
		res := make([]any, 0, len(pendingIDs))
		for _, id := range pendingIDs {
			res = append(res, map[string]any{
				"role":         "tool",
				"tool_call_id": id,
				"content":      "工具调用结果在客户端上下文压缩中丢失；网关已补占位结果，请继续。",
			})
		}
		return res
	}
	flushUser := func() {
		out = append(out, userBuffer...)
		userBuffer = nil
	}

	for _, raw := range msgs {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch role {
		case "assistant":
			if len(pendingIDs) > 0 {
				out = append(out, missingResults()...)
				pendingIDs = nil
			}
			if len(userBuffer) > 0 {
				flushUser()
			}
			out = append(out, msg)
			if calls, ok := msg["tool_calls"].([]any); ok {
				for _, c := range calls {
					if call, ok := c.(map[string]any); ok {
						id := toStr(call["id"])
						if id == "" {
							id = "missing-" + strconv.Itoa(len(pendingIDs))
						}
						pendingIDs = append(pendingIDs, id)
					}
				}
			}
		case "tool":
			callID := toStr(msg["tool_call_id"])
			if idx := indexOf(pendingIDs, callID); idx >= 0 {
				out = append(out, msg)
				pendingIDs = append(pendingIDs[:idx], pendingIDs[idx+1:]...)
			} else if len(pendingIDs) > 0 {
				msg["tool_call_id"] = pendingIDs[0]
				pendingIDs = pendingIDs[1:]
				out = append(out, msg)
			} else {
				log.Printf("丢弃孤立的 tool 结果: tool_call_id=%s", callID)
			}
		default:
			if len(pendingIDs) > 0 {
				userBuffer = append(userBuffer, msg)
				continue
			}
			if len(userBuffer) > 0 {
				flushUser()
			}
			out = append(out, msg)
		}
	}
	if len(pendingIDs) > 0 {
		out = append(out, missingResults()...)
	}
	if len(userBuffer) > 0 {
		flushUser()
	}
	body["messages"] = out
	return body
}

func isDeepseek(model any) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(toStr(model))), "deepseek")
}

// InjectThinking turns on DeepSeek thinking when no explicit thinking is given.
func InjectThinking(body Body) Body {
	if !isDeepseek(body["model"]) {
		return body
	}
	if _, has := body["thinking"]; !has {
		var mt any = body["max_completion_tokens"]
		if mt == nil {
			mt = body["max_tokens"]
		}
		if mt != nil {
			if n, ok := toInt(mt); ok && n < 1024 {
				return body
			}
		}
	}
	if th, ok := body["thinking"].(map[string]any); ok {
		typ := strings.TrimSpace(toStr(th["type"]))
		if typ != "" {
			if strings.ToLower(typ) == "disabled" {
				delete(body, "reasoning_effort")
				delete(body, "reasoningEffort")
			}
			return body
		}
		th["type"] = "enabled"
		return body
	}
	body["thinking"] = map[string]any{"type": "enabled"}
	return body
}

// BackfillReasoningContent enforces DeepSeek multi-turn consistency.
func BackfillReasoningContent(body Body) Body {
	if !isDeepseek(body["model"]) {
		return body
	}
	msgs, ok := body["messages"].([]any)
	if !ok {
		return body
	}
	var assistants []map[string]any
	for _, m := range msgs {
		if mm, ok := m.(map[string]any); ok && mm["role"] == "assistant" {
			assistants = append(assistants, mm)
		}
	}
	hasTrace := func(m map[string]any) bool {
		rc, _ := m["reasoning_content"].(string)
		r, _ := m["reasoning"].(string)
		return rc != "" || r != ""
	}
	traced := false
	for _, m := range assistants {
		if hasTrace(m) {
			traced = true
			break
		}
	}
	if !traced {
		return body
	}
	for _, m := range assistants {
		if _, ok := m["reasoning_content"].(string); ok {
			continue
		}
		if reason, ok := m["reasoning"].(string); ok {
			m["reasoning_content"] = reason
		} else {
			m["reasoning_content"] = ""
		}
	}
	return body
}

func toStr(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	case bool:
		if x {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(x))
		return n, err == nil
	default:
		return 0, false
	}
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}
