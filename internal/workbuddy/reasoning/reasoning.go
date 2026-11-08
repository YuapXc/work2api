// Package reasoning normalizes the upstream request body: reasoning_effort is
// downgraded per model capability, tool_choice is coerced to the upstream's
// string form, roles are normalized, tool sequences repaired, and DeepSeek
// thinking is injected. Ported from workbuddy_one/reasoning.py.
package reasoning

import (
	"encoding/json"
	"io"
	"log"
	"maps"
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
	body = MirrorUpstreamReasoning(body)
	body, _ = GuardEmptyTextParts(body, false)
	return body
}

func MirrorUpstreamReasoning(body Body) Body {
	model, _ := body["model"].(string)
	model = strings.ToLower(model)
	matched := false
	for _, family := range []string{"kimi", "glm", "minimax", "mimo", "step", "space-bunny"} {
		if model == family || strings.HasPrefix(model, family+"-") {
			matched = true
			break
		}
	}
	if !matched {
		return body
	}
	msgs, _ := body["messages"].([]any)
	var out []any
	for i, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok || m["role"] != "assistant" {
			continue
		}
		canonical, _ := m["reasoning"].(string)
		alias, _ := m["reasoning_content"].(string)
		if canonical != "" || strings.TrimSpace(alias) == "" {
			continue
		}
		if out == nil {
			out = append([]any(nil), msgs...)
		}
		next := maps.Clone(m)
		next["reasoning"] = alias
		out[i] = next
	}
	if out == nil {
		return body
	}
	next := maps.Clone(body)
	next["messages"] = out
	return next
}

// Strings are interpreted as protocol parts only after an explicit 11151.
func GuardEmptyTextParts(body Body, stringParts bool) (Body, bool) {
	msgs, _ := body["messages"].([]any)
	var out []any
	for i, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		parts, isList := m["content"].([]any)
		if !isList {
			if !stringParts {
				continue
			}
			text, ok := m["content"].(string)
			if !ok {
				continue
			}
			parts = sniffTextParts(text)
			if len(parts) == 0 {
				continue
			}
		}

		var fixed []any
		for j, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok || part["type"] != "text" {
				continue
			}
			if v := part["text"]; v != nil && v != "" {
				continue
			}
			if fixed == nil {
				fixed = append([]any(nil), parts...)
			}
			next := maps.Clone(part)
			next["text"] = " "
			fixed[j] = next
		}
		if fixed == nil {
			continue
		}
		var content any = fixed
		if !isList {
			encoded, err := json.Marshal(fixed)
			if err != nil {
				continue
			}
			content = string(encoded)
		}
		if out == nil {
			out = append([]any(nil), msgs...)
		}
		next := maps.Clone(m)
		next["content"] = content
		out[i] = next
	}
	if out == nil {
		return body, false
	}
	next := maps.Clone(body)
	next["messages"] = out
	return next, true
}

func sniffTextParts(text string) []any {
	if !strings.HasPrefix(strings.TrimLeft(text, " \t\r\n"), "[") {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var parts []any
	if decoder.Decode(&parts) != nil {
		return nil
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil
	}
	for _, part := range parts {
		if _, ok := part.(map[string]any); !ok {
			return nil
		}
	}
	return parts
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
	pendingCount := 0
	resetPending := func() {
		pendingIDs, consumed = nil, nil
		byID = map[string][]int{}
		pendingCount = 0
	}
	consume := func(idx int) {
		consumed[idx] = true
		pendingCount--
		id := pendingIDs[idx]
		positions := byID[id]
		// Consume only the result with this call's exact ID.
		if len(positions) <= 1 {
			delete(byID, id)
		} else {
			byID[id] = positions[1:]
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
			if calls, ok := msg["tool_calls"].([]any); ok {
				kept := make([]any, 0, len(calls))
				for _, rawCall := range calls {
					call, ok := rawCall.(map[string]any)
					if !ok {
						continue
					}
					id, _ := call["id"].(string)
					fn, _ := call["function"].(map[string]any)
					name, _ := fn["name"].(string)
					typ, _ := call["type"].(string)
					if strings.TrimSpace(id) == "" || strings.TrimSpace(name) == "" || (typ != "" && typ != "function") {
						continue
					}
					kept = append(kept, call)
				}
				if len(kept) != len(calls) {
					copyMessage := make(map[string]any, len(msg))
					for key, value := range msg {
						copyMessage[key] = value
					}
					msg = copyMessage
					if len(kept) > 0 {
						msg["tool_calls"] = kept
					} else {
						delete(msg, "tool_calls")
						if msg["content"] == nil || msg["content"] == "" {
							continue
						}
					}
				}
			}
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
			} else {
				log.Print("丢弃无法匹配有效调用的 tool 结果")
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

// WorkBuddy reads DeepSeek's tool-round history from reasoning, not the
// OpenAI-compatible reasoning_content spelling returned to clients.
func BackfillReasoningContent(body Body) Body {
	if !isDeepseek(body["model"]) {
		return body
	}
	msgs, ok := body["messages"].([]any)
	if !ok {
		return body
	}
	var copied []any
	for i, raw := range msgs {
		m, ok := raw.(map[string]any)
		if !ok || m["role"] != "assistant" {
			continue
		}
		reason, _ := m["reasoning"].(string)
		alias, hasAlias := m["reasoning_content"].(string)
		if reason == "" {
			reason = alias
		}
		var detailsText strings.Builder
		if reason == "" {
			if details, ok := m["reasoning_details"].([]any); ok {
				for _, raw := range details {
					detail, _ := raw.(map[string]any)
					text, _ := detail["text"].(string)
					if text == "" {
						text, _ = detail["summary"].(string)
					}
					detailsText.WriteString(text)
				}
			}
			reason = detailsText.String()
		}
		if !hasAlias && detailsText.Len() == 0 {
			continue
		}
		if copied == nil {
			copied = append([]any(nil), msgs...)
		}
		next := make(map[string]any, len(m)+1)
		for key, value := range m {
			next[key] = value
		}
		next["reasoning"] = reason
		delete(next, "reasoning_content")
		if detailsText.Len() > 0 {
			delete(next, "reasoning_details")
		}
		copied[i] = next
	}
	if copied != nil {
		next := make(Body, len(body))
		for key, value := range body {
			next[key] = value
		}
		next["messages"] = copied
		return next
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
