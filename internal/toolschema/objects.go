// Package toolschema contains opt-in provider boundary compatibility for tool schemas.
package toolschema

import (
	"reflect"
	"strings"
)

// ObjectUnions adds only a redundant object type to an untyped oneOf whose
// every branch proves object-only. It preserves the caller's schema and data.
// Adapted from CPA workbuddy 0.9.76 / qoder 0.8.69; no external refs are fetched.
func ObjectUnions(schema any) any {
	root, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	budget := 100000
	var walk func(map[string]any, int) map[string]any
	walk = func(s map[string]any, depth int) map[string]any {
		if depth > 64 || budget <= 0 {
			return s
		}
		budget--
		if depth > 0 && (s["$id"] != nil || s["id"] != nil) {
			return s
		}
		out := s
		copied := false
		set := func(k string, v any) {
			if !copied {
				out = copyMap(s)
				copied = true
			}
			out[k] = v
		}
		if _, typed := s["type"]; !typed {
			if branches, ok := s["oneOf"].([]any); ok && len(branches) > 0 {
				all := true
				for _, b := range branches {
					if !objectBranch(root, b, nil) {
						all = false
						break
					}
				}
				if all {
					set("type", "object")
				}
			}
		}
		for _, k := range []string{"items", "additionalItems", "unevaluatedItems", "contains", "not", "if", "then", "else", "propertyNames", "additionalProperties", "unevaluatedProperties"} {
			if child, ok := s[k].(map[string]any); ok {
				next := walk(child, depth+1)
				if changedMap(child, next) {
					set(k, next)
				}
			}
		}
		for _, k := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
			if list, ok := s[k].([]any); ok {
				var next []any
				for i, v := range list {
					if child, ok := v.(map[string]any); ok {
						m := walk(child, depth+1)
						if changedMap(child, m) {
							if next == nil {
								next = append([]any{}, list...)
							}
							next[i] = m
						}
					}
				}
				if next != nil {
					set(k, next)
				}
			}
		}
		for _, k := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
			if entries, ok := s[k].(map[string]any); ok {
				var next map[string]any
				for key, v := range entries {
					if child, ok := v.(map[string]any); ok {
						m := walk(child, depth+1)
						if changedMap(child, m) {
							if next == nil {
								next = copyMap(entries)
							}
							next[key] = m
						}
					}
				}
				if next != nil {
					set(k, next)
				}
			}
		}
		return out
	}
	return walk(root, 0)
}

// A map is copied only when modified; compare identity without scanning data.
func changedMap(a, b map[string]any) bool { return reflectMap(a) != reflectMap(b) }

func objectBranch(root map[string]any, value any, seen []string) bool {
	b, ok := value.(map[string]any)
	if !ok || len(seen) > 16 || b["$id"] != nil || b["id"] != nil {
		return false
	}
	if ref, ok := b["$ref"].(string); ok {
		for _, v := range seen {
			if v == ref {
				return false
			}
		}
		return objectBranch(root, localRef(root, ref), append(seen, ref))
	}
	return b["type"] == "object"
}
func localRef(root map[string]any, ref string) any {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	var at any = root
	for i, key := range strings.Split(ref[2:], "/") {
		m, ok := at.(map[string]any)
		if !ok {
			return nil
		}
		if i > 0 && (m["$id"] != nil || m["id"] != nil) {
			return nil
		}
		key = strings.ReplaceAll(strings.ReplaceAll(key, "~1", "/"), "~0", "~")
		at = m[key]
	}
	return at
}
func copyMap(s map[string]any) map[string]any {
	out := make(map[string]any, len(s)+1)
	for k, v := range s {
		out[k] = v
	}
	return out
}

// Tools handles canonical function tools and flat Responses tools. Namespace
// lowering occurs before these provider boundaries; namespaces are supported
// here as well for direct callers. No descriptions or argument data are changed.
func Tools(value any) any {
	list, ok := value.([]any)
	if !ok {
		return value
	}
	var out []any
	for i, raw := range list {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		next := tool
		if tool["type"] == "namespace" {
			nested := Tools(tool["tools"])
			if reflectSlice(tool["tools"]) != reflectSlice(nested) {
				next = copyMap(tool)
				next["tools"] = nested
			}
		} else if tool["type"] == "function" {
			fn, nested := tool["function"].(map[string]any)
			if !nested {
				fn = tool
			}
			old := fn["parameters"]
			mended := ObjectUnions(old)
			if oldMap, ok := old.(map[string]any); ok && changedMap(oldMap, mended.(map[string]any)) {
				nf := copyMap(fn)
				nf["parameters"] = mended
				if nested {
					next = copyMap(tool)
					next["function"] = nf
				} else {
					next = nf
				}
			}
		}
		if changedMap(tool, next) {
			if out == nil {
				out = append([]any{}, list...)
			}
			out[i] = next
		}
	}
	if out == nil {
		return value
	}
	return out
}

func reflectMap(m map[string]any) uintptr { return reflect.ValueOf(m).Pointer() }
func reflectSlice(v any) uintptr {
	if s, ok := v.([]any); ok {
		return reflect.ValueOf(s).Pointer()
	}
	return 0
}
