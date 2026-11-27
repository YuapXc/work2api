// Adapted from mmqz/cpa-multi-plugins, MIT; see NOTICE.md.
package upstream

import (
	"encoding/json"
	"strings"
)

func PrepareBody(src []byte, variant string) []byte {
	return PrepareBodyResolved(src, variant, "")
}
func PrepareBodyResolved(src []byte, variant, resolvedModel string) []byte {
	body, _ := PrepareBodyResolvedWithLimits(src, variant, resolvedModel)
	return body
}

// Capture effective limits from the same object that produces the wire body;
// callers must not prepare and decode the entire conversation again for logs.
func PrepareBodyResolvedWithLimits(src []byte, variant, resolvedModel string, models ...ModelInfo) ([]byte, map[string]int) {
	if len(src) == 0 {
		return src, nil
	}
	var obj map[string]any
	if err := json.Unmarshal(src, &obj); err != nil {
		return src, nil
	}
	obj["stream"] = true
	obj["function"] = FunctionFor(variant)

	if msgs, ok := obj["messages"].([]any); ok {
		for _, item := range msgs {
			message, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if message["role"] == "developer" {
				message["role"] = "system"
			}
			if message["role"] == "assistant" && message["content"] == nil {
				message["content"] = ""
			}
			if calls, ok := message["tool_calls"].([]any); ok {
				for _, item := range calls {
					if call, ok := item.(map[string]any); ok {
						if fn, ok := call["function"].(map[string]any); ok {
							call["function_call"] = fn
							delete(call, "function")
						}
					}
				}
			}
			if content, ok := message["content"].(string); ok {
				message["content"] = []any{map[string]any{"type": "text", "text": content}}
			}
		}
	}
	model, _ := obj["model"].(string)
	if r := strings.TrimSpace(resolvedModel); r != "" {
		model = r
	}
	model = SanitizeModelName(model, variant)
	if model == "" {
		model = DefaultConfigName
	}

	normalizeToolChoice(obj)
	normalizeTools(obj)
	out := make(map[string]any, 16)
	if msgs, ok := obj["messages"]; ok {
		out["messages"] = msgs
	}
	out["function"] = FunctionFor(variant)
	out["stream"] = true
	out["config_name"] = model
	out["model"] = model
	if session, ok := obj["session_id"].(string); ok && session != "" {
		out["session_id"] = session
		out["request_id"] = newRequestID()
	}
	if tools, ok := obj["tools"]; ok {
		out["tools"] = tools
	}
	if tc, ok := obj["tool_choice"]; ok {
		out["tool_choice"] = tc
	}
	for _, k := range []string{"temperature", "top_p", "max_tokens",
		"presence_penalty", "frequency_penalty", "seed", "n"} {
		if v, ok := obj[k].(float64); ok {
			out[k] = v
		}
	}
	if v, ok := obj["max_completion_tokens"].(float64); ok {
		out["max_tokens"] = v
	}
	if len(models) > 0 {
		info := models[0]
		if info.ID == model && info.UseMaxMode {
			out["is_max_mode"] = true
		}
		if info.ID == model && info.ConsoleRoute && info.Enterprise != nil {
			ent := info.Enterprise
			out["config_source"], out["is_custom_model"] = 2, true
			if ent.ModelName != "" {
				out["model_name"] = ent.ModelName
			}
			if ent.Provider != "" {
				out["provider"] = ent.Provider
			}
		}
		asked, exists := obj["max_completion_tokens"]
		if !exists {
			asked, exists = obj["max_tokens"]
		}
		if info.ID == model {
			limit := float64(info.MaxTokens)
			if exists {
				n, _ := asked.(float64)
				if n <= 0 || n > 1<<53 || n != float64(int64(n)) {
					limit = 0
				} else if limit <= 0 || n < limit {
					limit = n
				}
			}
			if limit > 0 {
				out["max_tokens"] = limit
				if ent := info.Enterprise; info.ConsoleRoute && ent != nil {
					hyper := map[string]any{"max_tokens": limit}
					if ent.PromptMax > 0 {
						hyper["prompt_max_tokens"] = ent.PromptMax
					}
					if info.Multimodal != nil {
						hyper["multimodal"] = *info.Multimodal
					}
					out["enterprise_custom_hyper_params"] = hyper
				}
			}
		}
		if re, ok := obj["reasoning_effort"].(string); ok {
			if native, known := models[0].NativeEffort(re); known {
				obj["reasoning_effort"] = native
			} else {
				delete(obj, "reasoning_effort")
			}
		}
	}
	if re, ok := obj["reasoning_effort"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(re)) {
		case "", "auto", "none", "off":
		default:
			out["reasoning_effort"] = re
		}
	}
	switch stop := obj["stop"].(type) {
	case string:
		out["stop"] = stop
	case []any:
		out["stop"] = stop
	}
	if len(models) > 0 && models[0].ID == model && models[0].ConfigName != "" {
		out["config_name"], out["model"] = models[0].ConfigName, models[0].ConfigName
	}
	b, err := json.Marshal(out)
	if err != nil {
		return src, nil
	}
	var limits map[string]int
	if n, ok := out["max_tokens"].(float64); ok && n > 0 && n <= 1<<30 && n == float64(int(n)) {
		limits = map[string]int{"max_tokens": int(n)}
	}
	return b, limits
}
func SanitizeModelName(model, variant string) string { return strings.TrimSpace(model) }

const DefaultConfigName = "glm-5.2"

func normalizeToolChoice(obj map[string]any) {
	suppress := func() {
		delete(obj, "tools")
		delete(obj, "functions")
	}
	tc, present := obj["tool_choice"]
	if !present {
		return
	}
	switch v := tc.(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(v), "none") {
			delete(obj, "tool_choice")
			suppress()
		}
	case map[string]any:
		typ, _ := v["type"].(string)
		typ = strings.ToLower(strings.TrimSpace(typ))
		switch typ {
		case "none":
			delete(obj, "tool_choice")
			suppress()
		case "auto", "required":
			obj["tool_choice"] = typ
		case "function":
			name := ""
			if fn, ok := v["function"].(map[string]any); ok {
				name, _ = fn["name"].(string)
			}
			if name == "" {
				name, _ = v["name"].(string)
			}
			if name = strings.TrimSpace(name); name != "" {
				obj["tool_choice"] = name
			} else {
				obj["tool_choice"] = "auto"
			}
		default:
			delete(obj, "tool_choice")
		}
	default:
		delete(obj, "tool_choice")
	}
}
func normalizeTools(obj map[string]any) {
	raw, present := obj["tools"]
	if !present {
		return
	}
	list, ok := raw.([]any)
	if !ok || len(list) == 0 {
		return
	}
	out := make([]any, 0, len(list))
	for _, item := range list {
		t, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := t["function"].(map[string]any)
		if !ok {
			continue
		}
		if params, ok := fn["parameters"]; ok {
			if paramsMap, isMap := params.(map[string]any); isMap {
				if s, err := json.Marshal(paramsMap); err == nil {
					fn["parameters"] = string(s)
				}
			}
		}
		out = append(out, t)
	}
	if len(out) == 0 {
		delete(obj, "tools")
		return
	}
	obj["tools"] = out
}
