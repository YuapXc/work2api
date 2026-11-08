package protocol

import (
	"encoding/base64"
	"io"
	"maps"
	"reflect"
	"strings"
)

// NormalizeInput touches only protocol-defined media and tool-definition fields.
func NormalizeInput(from Protocol, body map[string]any) map[string]any {
	if from == SystemOne {
		return body
	}
	out := body
	for _, key := range []string{"messages", "input", "system"} {
		if value, ok := body[key].([]any); ok {
			fixed := normalizeMediaList(value, 0)
			if fixed != nil {
				if sameMap(out, body) {
					out = maps.Clone(body)
				}
				out[key] = fixed
			}
		}
	}
	if from == Chat {
		out = normalizeChatTools(out)
	}
	return out
}

func standardBase64(value string) (string, bool) {
	urlSafe := strings.IndexByte(value, '-') >= 0 || strings.IndexByte(value, '_') >= 0
	if value == "" || (!urlSafe && len(value)%4 == 0) || urlSafe && strings.ContainsAny(value, "+/") {
		return value, false
	}
	if strings.ContainsAny(value, "\r\n\t ") {
		return value, false
	}
	encoding := base64.RawStdEncoding.Strict()
	if urlSafe {
		encoding = base64.RawURLEncoding.Strict()
	}
	trimmed := strings.TrimRight(value, "=")
	if len(value)-len(trimmed) > 2 {
		return value, false
	}
	if len(value) != len(trimmed) && len(value)%4 != 0 {
		return value, false
	}
	// Validation streams to a fixed buffer; only the normalized string is copied.
	decoder := base64.NewDecoder(encoding, strings.NewReader(trimmed))
	var buf [4096]byte
	for {
		_, err := decoder.Read(buf[:])
		if err != nil {
			if err != io.EOF {
				return value, false
			}
			break
		}
	}
	clean := strings.NewReplacer("-", "+", "_", "/").Replace(trimmed)
	if padding := len(clean) % 4; padding != 0 {
		clean += strings.Repeat("=", 4-padding)
	}
	return clean, clean != value
}

func normalizedDataURL(value string) (string, bool) {
	if !strings.HasPrefix(value, "data:") {
		return value, false
	}
	header, data, ok := strings.Cut(value, ",")
	if !ok || !strings.HasSuffix(header, ";base64") {
		return value, false
	}
	fixed, changed := standardBase64(data)
	if !changed {
		return value, false
	}
	return header + "," + fixed, true
}

func normalizeMediaList(items []any, depth int) []any {
	if depth > 32 {
		return nil
	}
	var out []any
	for i, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		next := item
		set := func(key string, value any) {
			if sameMap(next, item) {
				next = maps.Clone(item)
			}
			next[key] = value
		}
		fixURL := func(key string) {
			if value, ok := item[key].(string); ok {
				if fixed, changed := normalizedDataURL(value); changed {
					set(key, fixed)
				}
			}
		}
		switch item["type"] {
		case "image_url":
			if image, ok := item["image_url"].(map[string]any); ok {
				if value, ok := image["url"].(string); ok {
					if fixed, changed := normalizedDataURL(value); changed {
						image = maps.Clone(image)
						image["url"] = fixed
						set("image_url", image)
					}
				}
			} else {
				fixURL("image_url")
			}
		case "input_image":
			fixURL("image_url")
		case "image":
			if source, ok := item["source"].(map[string]any); ok && source["type"] == "base64" {
				if data, ok := source["data"].(string); ok {
					if fixed, changed := standardBase64(data); changed {
						source = maps.Clone(source)
						source["data"] = fixed
						set("source", source)
					}
				}
			}
		case "input_audio":
			if audio, ok := item["input_audio"].(map[string]any); ok {
				if data, ok := audio["data"].(string); ok {
					if fixed, changed := standardBase64(data); changed {
						audio = maps.Clone(audio)
						audio["data"] = fixed
						set("input_audio", audio)
					}
				}
			}
		}
		// Tool arguments/input and user text are opaque, even if they resemble media.
		_, message := item["role"]
		if message || item["type"] == "tool_result" || item["type"] == "function_call_output" || item["type"] == "custom_tool_call_output" {
			for _, key := range []string{"content", "output"} {
				if nested, ok := item[key].([]any); ok {
					if fixed := normalizeMediaList(nested, depth+1); fixed != nil {
						set(key, fixed)
					}
				}
			}
		}
		if !sameMap(next, item) {
			if out == nil {
				out = append([]any(nil), items...)
			}
			out[i] = next
		}
	}
	return out
}

func normalizeChatTools(body map[string]any) map[string]any {
	tools, ok := body["tools"].([]any)
	if body["tools"] != nil && !ok {
		return body
	}
	var seen map[string]bool
	nameOf := func(raw any) string {
		t, _ := raw.(map[string]any)
		if f, ok := t["function"].(map[string]any); ok {
			n, _ := f["name"].(string)
			return n
		}
		n, _ := t["name"].(string)
		return n
	}
	changed := false
	var messages []any
	if list, ok := body["messages"].([]any); ok {
		for i, raw := range list {
			m, ok := raw.(map[string]any)
			if !ok || (m["role"] != "system" && m["role"] != "developer") {
				if changed {
					messages = append(messages, raw)
				}
				continue
			}
			added, ok := m["tools"].([]any)
			if !ok || len(added) == 0 {
				if changed {
					messages = append(messages, raw)
				}
				continue
			}
			if !changed {
				seen = map[string]bool{}
				for _, t := range tools {
					if name := nameOf(t); name != "" {
						seen[name] = true
					}
				}
				tools = append([]any(nil), tools...)
				messages = append([]any{}, list[:i]...)
			}
			changed = true
			for _, t := range added {
				name := nameOf(t)
				if name != "" && !seen[name] {
					tools = append(tools, t)
					seen[name] = true
				}
			}
			m = maps.Clone(m)
			delete(m, "tools")
			if c, exists := m["content"]; exists && c != nil && c != "" {
				messages = append(messages, m)
			}
		}
	}
	out := body
	if changed {
		out = maps.Clone(body)
		out["tools"], out["messages"] = tools, messages
	}
	var fixed []any
	for i, raw := range tools {
		t, ok := raw.(map[string]any)
		if !ok || t["function"] != nil || (t["type"] != nil && t["type"] != "" && t["type"] != "function") {
			continue
		}
		name, _ := t["name"].(string)
		if name == "" {
			continue
		}
		fn := map[string]any{}
		next := maps.Clone(t)
		for _, key := range []string{"name", "description", "strict"} {
			if v, ok := t[key]; ok {
				fn[key] = v
				delete(next, key)
			}
		}
		for _, key := range []string{"parameters", "inputSchema", "input_schema"} {
			if v, ok := t[key]; ok {
				if fn["parameters"] == nil {
					fn["parameters"] = v
				}
				delete(next, key)
			}
		}
		next["type"], next["function"] = "function", fn
		if fixed == nil {
			fixed = append([]any(nil), tools...)
		}
		fixed[i] = next
	}
	if fixed != nil {
		if sameMap(out, body) {
			out = maps.Clone(body)
		}
		out["tools"] = fixed
	}
	return out
}

func sameMap(a, b map[string]any) bool {
	return reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer()
}
