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

// effortRank orders effort levels low→high. `none` is an alias of `off`
// (OpenAI-style clients say none, Anthropic-style say off), both rank 0.
var effortRank = map[string]int{
	"off": 0, "none": 0, "minimal": 1, "low": 2, "medium": 3, "high": 4, "xhigh": 5, "max": 6,
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
	// space-bunny 为 onlyReasoning 模型，支持 low..max、默认 max（buddy-proxy 实测补录）
	"space-bunny": {"low", "medium", "high", "xhigh", "max"},
}

// Body is the mutable request body.
type Body = map[string]any

// NormalizeReasoningEffort downgrades reasoning_effort to the model's supported
// levels (snake/camel field compatible).
//
// ⚠️ `off`/`none` is the "no thinking" SWITCH, not the lowest rung — it must be
// picked separately from thinking levels (Workbuddy2API #24). The naive
// "highest level ≤ requested" loop always matches off (rank 0), so a client
// asking for low/medium on a model whose lowest thinking rung is high gets
// off — thinking silently disabled, worse than no downgrade at all. Semantics:
//  1. request off/none → give off when supported, else lift to the lowest thinking level;
//  2. request a thinking level → pick the highest thinking level ≤ request
//     (off excluded); when all exceed the request, take the lowest thinking level;
//  3. model offers only off (no thinking at all) → only off.
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
	rank := func(s string) (int, bool) {
		i, ok := effortRank[strings.ToLower(strings.TrimSpace(s))]
		return i, ok
	}
	var thinking, offLike []string
	for _, s := range supported {
		idx, ok := rank(s)
		if !ok {
			continue
		}
		if idx == 0 {
			offLike = append(offLike, s)
		} else {
			thinking = append(thinking, s)
		}
	}
	lowest := func(levels []string) string {
		best, bestIdx := "", 1<<30
		for _, s := range levels {
			idx, _ := rank(s)
			if idx < bestIdx {
				best, bestIdx = s, idx
			}
		}
		return best
	}
	chosen := ""
	if reqIdx == 0 {
		if len(offLike) > 0 {
			// Client explicitly wants no thinking and the model supports it.
			chosen = offLike[0]
		} else if len(thinking) > 0 {
			// Model lists no "off" rung (onlyReasoning): measured against the real
			// upstream (2026-10-07, deepseek-v4-pro, catalog onlyReasoning) bare
			// "off" is rejected with 400 code=11150, while "off" +
			// thinking:{type:disabled} succeeds with no reasoning. Rewrite "off"
			// into the form the upstream actually accepts for "no thinking":
			// lift to the lowest thinking rung + drop the thinking switch.
			// InjectThinking (DeepSeek) then leaves the explicit disabled state
			// alone and strips the rewritten effort, so "off" truly means off
			// instead of silently forcing thinking on. Clients that sent their
			// own thinking object keep it untouched.
			if _, stated := body["thinking"]; !stated {
				body["thinking"] = map[string]any{"type": "disabled"}
			}
			chosen = lowest(thinking)
		}
	} else if len(thinking) > 0 {
		// Thinking levels only: highest level ≤ request, else the lowest one.
		bestIdx := -1
		for _, s := range thinking {
			idx, _ := rank(s)
			if idx <= reqIdx && idx > bestIdx {
				chosen, bestIdx = s, idx
			}
		}
		if chosen == "" {
			chosen = lowest(thinking)
		}
	} else if len(offLike) > 0 {
		// Model offers only off (no thinking capability).
		chosen = offLike[0]
	}
	if chosen == "" {
		return body
	}
	if strings.ToLower(strings.TrimSpace(chosen)) != req {
		log.Printf("reasoning_effort 降级 model=%s %s -> %s", model, req, chosen)
	}
	body[key] = chosen
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
	body = EnsureLeadingSystem(body)
	body = RepairToolSequence(body)
	body = InjectThinking(body)
	body = BackfillReasoningContent(body)
	return body
}

// NormalizeRoles rewrites developer→system (upstream role whitelist rejects
// "developer" with 400 code=11128; developer is OpenAI's alias of system).
// Only this one value is rewritten — no merging, reordering or deletion.
func NormalizeRoles(body Body) Body {
	msgs, ok := body["messages"].([]any)
	if !ok {
		return body
	}
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if mm["role"] == "developer" {
			mm["role"] = "system"
		}
	}
	return body
}

// EnsureLeadingSystem prepends an EMPTY system message when the first message
// is not a system one. Upstream 11128 checks messages[0].role, not "any system
// exists": a client sending [user, system] still 400s unless the first message
// is system (upstream workbuddy_one.ensure_leading_system; an empty system is
// harmless on the domestic site and required on the international one).
func EnsureLeadingSystem(body Body) Body {
	msgs, ok := body["messages"].([]any)
	if !ok || len(msgs) == 0 {
		return body
	}
	if first, ok := msgs[0].(map[string]any); ok && first["role"] == "system" {
		return body
	}
	sys := map[string]any{"role": "system", "content": ""}
	body["messages"] = append([]any{sys}, msgs...)
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
	var consumed []bool
	byID := map[string][]int{}
	firstPending, pendingCount := 0, 0
	resetPending := func() {
		pendingIDs, consumed = nil, nil
		byID = map[string][]int{}
		firstPending, pendingCount = 0, 0
	}
	consume := func(idx int) {
		consumed[idx] = true
		pendingCount--
		id := pendingIDs[idx]
		positions := byID[id]
		// The matched node or fallback is always this ID's earliest live node.
		if len(positions) <= 1 {
			delete(byID, id)
		} else {
			byID[id] = positions[1:]
		}
		for firstPending < len(consumed) && consumed[firstPending] {
			firstPending++
		}
	}
	var userBuffer []any

	missingResults := func() []any {
		res := make([]any, 0, len(pendingIDs))
		for i, id := range pendingIDs {
			if consumed[i] {
				continue
			}
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
			if pendingCount > 0 {
				out = append(out, missingResults()...)
			}
			resetPending()
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
						byID[id] = append(byID[id], len(pendingIDs))
						pendingIDs = append(pendingIDs, id)
						consumed = append(consumed, false)
						pendingCount++
					}
				}
			}
		case "tool":
			callID := toStr(msg["tool_call_id"])
			if positions := byID[callID]; len(positions) > 0 {
				out = append(out, msg)
				consume(positions[0])
			} else if pendingCount > 0 {
				msg["tool_call_id"] = pendingIDs[firstPending]
				consume(firstPending)
				out = append(out, msg)
			} else {
				log.Printf("丢弃孤立的 tool 结果: tool_call_id=%s", callID)
			}
		default:
			if pendingCount > 0 {
				userBuffer = append(userBuffer, msg)
				continue
			}
			if len(userBuffer) > 0 {
				flushUser()
			}
			out = append(out, msg)
		}
	}
	if pendingCount > 0 {
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
	// Explicit off remains meaningful even when the catalog has no support set.
	if _, has := body["thinking"]; !has {
		effort := strings.ToLower(strings.TrimSpace(toStr(body["reasoning_effort"])))
		if effort == "" {
			effort = strings.ToLower(strings.TrimSpace(toStr(body["reasoningEffort"])))
		}
		if effort == "off" || effort == "none" {
			body["thinking"] = map[string]any{"type": "disabled"}
		}
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
