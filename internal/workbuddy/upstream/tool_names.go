package upstream

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
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
// The callback returns nil for an unchanged object.
func rewriteRequestToolNames(body map[string]any, rewrite func(map[string]any) map[string]any) map[string]any {
	var out map[string]any
	set := func(key string, value any) {
		if out == nil {
			out = maps.Clone(body)
		}
		out[key] = value
	}
	rewriteFunction := func(object map[string]any) map[string]any {
		fn, _ := object["function"].(map[string]any)
		if next := rewrite(fn); next != nil {
			object = maps.Clone(object)
			object["function"] = next
			return object
		}
		return nil
	}
	if tools := rewriteToolObjects(body["tools"], rewriteFunction); tools != nil {
		set("tools", tools)
	}
	choice, _ := body["tool_choice"].(map[string]any)
	if next := rewriteFunction(choice); next != nil {
		set("tool_choice", next)
	}
	messages := rewriteToolObjects(body["messages"], func(msg map[string]any) map[string]any {
		var next map[string]any
		if msg["role"] == "tool" {
			next = rewrite(msg)
		}
		if calls := rewriteToolObjects(msg["tool_calls"], rewriteFunction); calls != nil {
			if next == nil {
				next = maps.Clone(msg)
			}
			next["tool_calls"] = calls
		}
		return next
	})
	if messages != nil {
		set("messages", messages)
	}
	return out
}

func rewriteToolObjects(value any, rewrite func(map[string]any) map[string]any) []any {
	list := slice(value)
	var out []any
	for i, raw := range list {
		object, _ := raw.(map[string]any)
		if next := rewrite(object); next != nil {
			if out == nil {
				out = slices.Clone(list)
			}
			out[i] = next
		}
	}
	return out
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
	rewriteRequestToolNames(body, func(fn map[string]any) map[string]any {
		name, _ := fn["name"].(string)
		if name != "" {
			used[name] = true
			if !validToolName(name) {
				invalid[name] = true
			}
		}
		return nil
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
	out := rewriteRequestToolNames(body, func(fn map[string]any) map[string]any {
		name, _ := fn["name"].(string)
		if alias := forward[name]; alias != "" {
			next := maps.Clone(fn)
			next["name"] = alias
			return next
		}
		return nil
	})
	if name, ok := out["tool_choice"].(string); ok && forward[name] != "" {
		out["tool_choice"] = forward[name]
	}
	return out, reverse, nil
}

type toolNameRestorer struct {
	reverse map[string]string
	pending map[string]string
}

func (r *toolNameRestorer) restore(chunk map[string]any) bool {
	if len(r.reverse) == 0 {
		return false
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
			name := r.pending[key] + part
			if original := r.reverse[name]; original != "" {
				fn["name"] = original
				delete(r.pending, key)
				changed = true
				continue
			}
			prefix := false
			for alias := range r.reverse {
				if strings.HasPrefix(alias, name) {
					prefix = true
					break
				}
			}
			if prefix {
				if r.pending == nil {
					r.pending = make(map[string]string)
				}
				r.pending[key] = name
				delete(fn, "name")
				changed = true
			} else if r.pending[key] != "" {
				fn["name"] = name
				delete(r.pending, key)
				changed = true
			}
		}
	}
	return changed
}
