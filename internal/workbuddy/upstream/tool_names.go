package upstream

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

func validToolName(name string) bool {
	if len(name) == 0 || len(name) > 64 {
		return false
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func toolNameError(message string) error {
	raw, _ := json.Marshal(map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": message}})
	return &UpstreamError{StatusCode: 400, Raw: raw}
}

// Only protocol name fields are visited; schemas, text and argument JSON stay opaque.
func visitRequestToolNames(body map[string]any, visit func(map[string]any)) {
	for _, raw := range slice(body["tools"]) {
		tool, _ := raw.(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		visit(fn)
	}
	if choice, ok := body["tool_choice"].(map[string]any); ok {
		fn, _ := choice["function"].(map[string]any)
		visit(fn)
	}
	for _, raw := range slice(body["messages"]) {
		msg, _ := raw.(map[string]any)
		for _, raw := range slice(msg["tool_calls"]) {
			call, _ := raw.(map[string]any)
			fn, _ := call["function"].(map[string]any)
			visit(fn)
		}
		if msg["role"] == "tool" {
			visit(msg)
		}
	}
}

func slice(v any) []any { out, _ := v.([]any); return out }

func prepareToolNames(body map[string]any) (map[string]any, map[string]string, error) {
	used, invalid := map[string]bool{}, map[string]bool{}
	for _, raw := range slice(body["tools"]) {
		tool, _ := raw.(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		name, _ := fn["name"].(string)
		if strings.TrimSpace(name) == "" {
			return nil, nil, toolNameError("function 工具必须指定非空名称")
		}
	}
	visitRequestToolNames(body, func(fn map[string]any) {
		name, _ := fn["name"].(string)
		if name == "" {
			return
		}
		used[name] = true
		if !validToolName(name) {
			invalid[name] = true
		}
	})
	if len(invalid) == 0 {
		return body, nil, nil
	}
	names := make([]string, 0, len(invalid))
	for name := range invalid {
		names = append(names, name)
	}
	sort.Strings(names)
	forward, reverse := map[string]string{}, map[string]string{}
	for _, name := range names {
		for salt := 0; ; salt++ {
			sum := sha256.Sum256([]byte(fmt.Sprintf("workbuddy-tool\x00%s\x00%d", name, salt)))
			alias := "w2a_" + hex.EncodeToString(sum[:16])
			if used[alias] {
				continue
			}
			used[alias] = true
			forward[name], reverse[alias] = alias, name
			break
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, nil, err
	}
	var out map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err = decoder.Decode(&out); err != nil {
		return nil, nil, err
	}
	visitRequestToolNames(out, func(fn map[string]any) {
		name, _ := fn["name"].(string)
		if alias := forward[name]; alias != "" {
			fn["name"] = alias
		}
	})
	if name, ok := out["tool_choice"].(string); ok && forward[name] != "" {
		out["tool_choice"] = forward[name]
	}
	return out, reverse, nil
}

func restoreToolNameLines(yield LineFunc, reverse map[string]string) LineFunc {
	if len(reverse) == 0 {
		return yield
	}
	pending := map[string]string{}
	return func(line string) error {
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			if len(pending) > 0 {
				return invalidStream("上游工具名称未完整返回")
			}
			return yield(line)
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			return yield(line)
		}
		changed := false
		for _, raw := range slice(chunk["choices"]) {
			choice, _ := raw.(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			for _, raw := range slice(delta["tool_calls"]) {
				call, _ := raw.(map[string]any)
				fn, _ := call["function"].(map[string]any)
				part, _ := fn["name"].(string)
				if part == "" {
					continue
				}
				key := fmt.Sprint(choice["index"], ":", call["index"])
				name := pending[key] + part
				if original := reverse[name]; original != "" {
					fn["name"] = original
					delete(pending, key)
					changed = true
					continue
				}
				prefix := false
				for alias := range reverse {
					if strings.HasPrefix(alias, name) {
						prefix = true
						break
					}
				}
				if prefix {
					pending[key] = name
					delete(fn, "name")
					changed = true
				} else if pending[key] != "" {
					fn["name"] = name
					delete(pending, key)
					changed = true
				}
			}
		}
		if !changed {
			return yield(line)
		}
		raw, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		return yield("data: " + string(raw))
	}
}
