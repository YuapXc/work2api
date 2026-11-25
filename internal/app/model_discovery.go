package app

import (
	"slices"
	"strings"
)

// Project only the public response; provider catalogs keep their native metadata.
func publicModelEntry(item map[string]any) map[string]any {
	out := make(map[string]any, len(item)+6)
	for k, v := range item {
		if k != "account_uids" {
			out[k] = v
		}
	}
	modalities, _ := item["modalities"].(map[string]any)
	input := discoveryStrings(item["input_modalities"])
	if len(input) == 0 {
		input = discoveryStrings(modalities["input"])
	}
	if len(input) == 0 {
		input = discoveryStrings(item["modalities"])
	}
	output := discoveryStrings(item["output_modalities"])
	if len(output) == 0 {
		output = discoveryStrings(modalities["output"])
	}
	kind, _ := item["model_type"].(string)
	if item["native_protocol"] == "systemone" {
		kind = "decision"
	}
	var endpoints []string
	switch kind {
	case "image":
		out["kind"] = "image"
		for _, tag := range discoveryStrings(item["media_tags"]) {
			switch tag {
			case "text-to-image":
				endpoints = append(endpoints, "/images/generations")
			case "image-to-image":
				endpoints = append(endpoints, "/images/edits")
			}
		}
	case "video":
		out["kind"] = "video"
		endpoints = []string{"/videos"}
	case "audio":
		out["type"] = "transcription"
		endpoints = []string{"/audio/transcriptions"}
	case "decision":
		out["kind"] = "decision"
		endpoints = []string{"/systemone"}
	default:
		endpoints = []string{"/chat/completions", "/messages", "/responses"}
		if len(output) == 0 {
			output = []string{"text"}
		}
		if len(input) == 0 && (modelHasVision(item) || item["provider"] == "unified") {
			input = []string{"text"}
			if modelHasVision(item) {
				input = append(input, "image")
			}
		}
	}
	out["modalities"] = map[string]any{"input": input, "output": output}
	if len(endpoints) > 0 {
		out["supported_endpoints"] = endpoints
	}
	if native := discoveryNativeEndpoints(item); len(native) > 0 {
		out["native_endpoints"] = native
	}
	levels, def, only, _ := discoveryReasoning(item)
	if len(levels) > 0 {
		out["supported_reasoning_levels"] = levels
	}
	if def != "" {
		out["think_efforts"] = map[string]any{"default_effort": def}
	}
	if only {
		out["supports_thinking_type"] = "only"
	}
	for _, key := range []string{"supportsToolCall", "tool_call"} {
		if value, ok := item[key].(bool); ok {
			out["supports_tool_calls"] = value
			break
		}
	}
	return out
}

func discoveryStrings(value any) []string {
	var out []string
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" && !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	switch v := value.(type) {
	case []string:
		for _, s := range v {
			add(s)
		}
	case []any:
		for _, raw := range v {
			if s, ok := raw.(string); ok {
				add(s)
			}
		}
	}
	return out
}

func discoveryReasoning(item map[string]any) (levels []string, def string, only bool, supported *bool) {
	r, _ := item["reasoning"].(map[string]any)
	if b, ok := item["reasoning"].(bool); ok {
		supported = &b
	} else if b, ok := r["supportsReasoning"].(bool); ok {
		supported = &b
	}
	if supported != nil && !*supported {
		return
	}
	only, _ = r["onlyReasoning"].(bool)
	levels = discoveryStrings(r["supportedEfforts"])
	if len(levels) == 0 {
		levels = discoveryStrings(item["reasoning_efforts"])
	}
	if only {
		levels = slices.DeleteFunc(levels, func(s string) bool { return strings.EqualFold(s, "off") || strings.EqualFold(s, "none") })
	} else if disabled, _ := r["canDisableThinking"].(bool); disabled && len(levels) > 0 && !slices.Contains(levels, "off") && !slices.Contains(levels, "none") {
		levels = append(levels, "off")
	}
	def, _ = r["defaultEffort"].(string)
	if def == "" {
		def, _ = item["default_reasoning_effort"].(string)
	}
	if (only && (def == "off" || def == "none")) || len(levels) > 0 && !slices.Contains(levels, def) {
		def = ""
	}
	return
}

func discoveryNativeEndpoints(item map[string]any) []string {
	if paths := discoveryStrings(item["native_endpoints"]); len(paths) > 0 {
		return paths
	}
	switch item["native_protocol"] {
	case "chat":
		return []string{"/chat/completions"}
	case "anthropic":
		return []string{"/messages"}
	case "responses":
		return []string{"/responses"}
	case "systemone":
		return []string{"/systemone"}
	}
	return nil
}

func routeDiscoveryCapabilities(out map[string]any, members []string, catalog map[string]map[string]any) {
	var levels, native []string
	var def string
	allOnly, allReasoning := true, true
	for i, member := range members {
		l, d, only, supported := discoveryReasoning(catalog[member])
		n := discoveryNativeEndpoints(catalog[member])
		allOnly = allOnly && only
		allReasoning = allReasoning && supported != nil && *supported
		if i == 0 {
			levels, def, native = l, d, n
		} else {
			levels = slices.DeleteFunc(levels, func(s string) bool { return !slices.Contains(l, s) })
			native = slices.DeleteFunc(native, func(s string) bool { return !slices.Contains(n, s) })
			if def != d {
				def = ""
			}
		}
	}
	if len(levels) > 0 || allReasoning {
		r := map[string]any{"supportedEfforts": levels}
		if allReasoning {
			r["supportsReasoning"] = true
		}
		if def != "" && (len(levels) == 0 || slices.Contains(levels, def)) {
			r["defaultEffort"] = def
		}
		if allOnly {
			r["onlyReasoning"] = true
		}
		out["reasoning"] = r
	}
	if len(native) > 0 {
		out["native_endpoints"] = native
	}
}
