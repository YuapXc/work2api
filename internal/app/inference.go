package app

import (
	"encoding/json"
	"strings"
	"time"

	"work2api/internal/store"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/upstream"
)

type logArgs struct {
	protocol, model string
	acc             *pool.Account
	t0              time.Time
	status, errStr  string
	usage           map[string]any
	cooldown        float64
	input, output   string
	reasoning       string
	appName, effort string
	updatePool      bool
}

func (o *Orchestrator) logUsage(a logArgs) {
	if a.acc != nil && a.updatePool {
		if a.status == "ok" {
			o.pool.OnSuccess(a.acc.UID)
		} else {
			cd := a.cooldown
			if cd == 0 {
				cd = cooldownSoft
			}
			o.pool.OnFailure(a.acc.UID, cd)
		}
	}
	inTok, outTok := usageTokens(a.usage)
	var credits *float64
	if a.usage != nil {
		if c, ok := a.usage["credit"].(float64); ok {
			credits = &c
		}
	}
	uid := ""
	if a.acc != nil {
		uid = a.acc.UID
	}
	effort := a.effort
	if effort == "" {
		effort = "default"
	}
	_ = o.db.LogUsage(store.UsageParams{
		Model:            a.model,
		Protocol:         a.protocol,
		AccountUID:       uid,
		InputTokens:      inTok,
		OutputTokens:     outTok,
		LatencyMs:        float64(time.Since(a.t0).Milliseconds()),
		Status:           a.status,
		Error:            a.errStr,
		InputContent:     a.input,
		OutputContent:    a.output,
		ReasoningContent: a.reasoning,
		Credits:          credits,
		AppName:          a.appName,
		ReasoningEffort:  effort,
	})
}

func usageTokens(u map[string]any) (int, int) {
	if u == nil {
		return 0, 0
	}
	in := firstInt(u, "prompt_tokens", "input_tokens")
	out := firstInt(u, "completion_tokens", "output_tokens")
	return in, out
}

func firstInt(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch x := v.(type) {
			case float64:
				return int(x)
			case int:
				return x
			}
		}
	}
	return 0
}

func usageReasoningEffort(body map[string]any) string {
	if e, ok := body["reasoning_effort"].(string); ok && strings.TrimSpace(e) != "" {
		return strings.ToLower(strings.TrimSpace(e))
	}
	if th, ok := body["thinking"].(map[string]any); ok {
		if th["type"] == "disabled" {
			return "off"
		}
	}
	return "default"
}

// mergeUsageInto merges an SSE chunk's usage into acc (returns merged).
func mergeUsageFromLine(line string, acc map[string]any) map[string]any {
	if !strings.HasPrefix(line, "data:") {
		return acc
	}
	data := strings.TrimSpace(line[5:])
	if data == "[DONE]" || data == "" {
		return acc
	}
	var chunk map[string]any
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return acc
	}
	if u, ok := chunk["usage"].(map[string]any); ok && u != nil {
		return upstream.MergeUsage(acc, u)
	}
	return acc
}

// deltaParts extracts content + reasoning increments (with tool_call recompose).
func deltaParts(line string) (string, string) {
	if !strings.HasPrefix(line, "data:") {
		return "", ""
	}
	data := strings.TrimSpace(line[5:])
	if data == "[DONE]" || data == "" {
		return "", ""
	}
	var chunk map[string]any
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return "", ""
	}
	var content, reasoning strings.Builder
	choices, _ := chunk["choices"].([]any)
	for _, ch := range choices {
		choice, _ := ch.(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if s, ok := delta["content"].(string); ok {
			content.WriteString(s)
		}
		if s, ok := delta["reasoning_content"].(string); ok {
			reasoning.WriteString(s)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, t := range tcs {
				tc, _ := t.(map[string]any)
				fn, _ := tc["function"].(map[string]any)
				if fn == nil {
					continue
				}
				if name, ok := fn["name"].(string); ok && name != "" {
					content.WriteString("<tool_call:" + name + " ")
				}
				if args, ok := fn["arguments"].(string); ok && args != "" {
					content.WriteString(args)
				}
				if _, hasID := tc["id"]; hasID {
					if _, hasArgs := fn["arguments"]; !hasArgs {
						content.WriteString(">")
					}
				}
			}
		}
	}
	return content.String(), reasoning.String()
}

// sanitizeChatSSE strips empty delta fields from a passthrough Chat SSE line.
// Returns "" if the whole line should be dropped.
func sanitizeChatSSE(line string) string {
	if !strings.HasPrefix(line, "data:") {
		return line
	}
	raw := strings.TrimSpace(line[5:])
	if raw == "[DONE]" || raw == "" {
		return line
	}
	var chunk map[string]any
	if json.Unmarshal([]byte(raw), &chunk) != nil {
		return line
	}
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		return line
	}
	var kept []any
	for _, ch := range choices {
		choice, ok := ch.(map[string]any)
		if !ok {
			kept = append(kept, ch)
			continue
		}
		if delta, ok := choice["delta"].(map[string]any); ok {
			for _, k := range []string{"content", "reasoning_content", "refusal", "function_call", "tool_calls", "reasoning"} {
				if v, has := delta[k]; has && isEmptyVal(v) {
					delete(delta, k)
				}
			}
			if len(delta) > 0 {
				choice["delta"] = delta
			} else if choice["finish_reason"] != nil && choice["finish_reason"] != "" {
				delete(choice, "delta")
			} else {
				continue
			}
		}
		kept = append(kept, choice)
	}
	if len(kept) == 0 {
		return ""
	}
	chunk["choices"] = kept
	b, _ := json.Marshal(chunk)
	return "data: " + string(b)
}

func isEmptyVal(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	case bool:
		return !x
	}
	return false
}

func (o *Orchestrator) extractInputText(body map[string]any) string {
	msgs, _ := body["messages"].([]any)
	var parts []string
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		role, _ := mm["role"].(string)
		if role == "" {
			role = "user"
		}
		var text string
		switch c := mm["content"].(type) {
		case string:
			text = c
		case []any:
			var segs []string
			for _, p := range c {
				pm, _ := p.(map[string]any)
				switch pm["type"] {
				case "text":
					if t, ok := pm["text"].(string); ok {
						segs = append(segs, t)
					}
				case "image_url":
					if iu, ok := pm["image_url"].(map[string]any); ok {
						segs = append(segs, "[图片: image_url|"+str2(iu["url"])+"]")
					}
				}
			}
			text = strings.Join(segs, " ")
		}
		if text != "" {
			parts = append(parts, role+": "+text)
		}
	}
	return strings.Join(parts, "\n")
}
