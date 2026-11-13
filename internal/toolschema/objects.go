// Package toolschema contains opt-in provider boundary compatibility for tool schemas.
package toolschema

import (
	"encoding/json"
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
	return mapTools(value, ObjectUnions)
}

// WorkBuddyTools adapts valid JSON schemas to the vendor's accepted spelling.
// Unlike permissive upstream repairs, it keeps type/enum intersections and
// never weakens tuples, root unions, required fields or empty enums.
func WorkBuddyTools(value any) any {
	return mapTools(value, workBuddySchema)
}

func WorkBuddyToolsForModel(value any, model string) any {
	model = strings.ToLower(model)
	if model != "kimi" && !strings.HasPrefix(model, "kimi-") {
		return WorkBuddyTools(value)
	}
	return mapTools(value, func(schema any) any { return workBuddySchema(inlineLocalSchema(schema)) })
}

// Expand only closed local references; an uncertain reference leaves the whole schema intact.
func inlineLocalSchema(schema any) any {
	root, ok := schema.(map[string]any)
	if !ok || root["$defs"] == nil && root["definitions"] == nil {
		return schema
	}
	if root["id"] != nil {
		return schema
	}
	modernDialect := false
	if raw, exists := root["$schema"]; exists {
		dialect, ok := raw.(string)
		if !ok {
			return schema
		}
		switch strings.TrimSuffix(dialect, "#") {
		case "https://json-schema.org/draft/2020-12/schema", "http://json-schema.org/draft/2020-12/schema", "https://json-schema.org/draft/2019-09/schema", "http://json-schema.org/draft/2019-09/schema":
			modernDialect = true
		default:
			return schema
		}
	}
	originalSize, valid := schemaSizeBound(root, 8<<20)
	if !valid {
		return schema
	}
	budget, expanded := 10000, 0
	active := map[string]bool{}
	var walk func(any, int) (any, bool)
	walk = func(value any, depth int) (any, bool) {
		if depth > 64 || budget <= 0 {
			return nil, false
		}
		budget--
		node, ok := value.(map[string]any)
		if !ok {
			_, ok := value.(bool)
			return value, ok
		}
		if depth > 0 && (node["$id"] != nil || node["id"] != nil || node["$schema"] != nil) || node["$dynamicRef"] != nil || node["$recursiveRef"] != nil || node["$anchor"] != nil || node["$dynamicAnchor"] != nil || node["$recursiveAnchor"] != nil || node["$vocabulary"] != nil {
			return nil, false
		}
		out := node
		set := func(key string, v any) {
			if !changedMap(node, out) {
				out = copyMap(node)
			}
			out[key] = v
		}
		remove := func(key string) {
			if _, exists := out[key]; exists {
				if !changedMap(node, out) {
					out = copyMap(node)
				}
				delete(out, key)
			}
		}
		for _, key := range []string{"items", "additionalItems", "unevaluatedItems", "contains", "not", "if", "then", "else", "propertyNames", "additionalProperties", "unevaluatedProperties", "contentSchema"} {
			if child, exists := node[key]; exists {
				if _, tuple := child.([]any); tuple {
					continue
				}
				next, valid := walk(child, depth+1)
				if !valid {
					return nil, false
				}
				if !sameSchema(child, next) {
					set(key, next)
				}
			}
		}
		for _, key := range []string{"anyOf", "oneOf", "allOf", "prefixItems", "items"} {
			if children, ok := node[key].([]any); ok {
				var next []any
				for i, child := range children {
					v, valid := walk(child, depth+1)
					if !valid {
						return nil, false
					}
					if !sameSchema(child, v) {
						if next == nil {
							next = append([]any(nil), children...)
						}
						next[i] = v
					}
				}
				if next != nil {
					set(key, next)
				}
			}
		}
		for _, key := range []string{"properties", "patternProperties", "dependentSchemas", "dependencies"} {
			if children, ok := node[key].(map[string]any); ok {
				var next map[string]any
				for name, child := range children {
					if key == "dependencies" {
						if _, list := child.([]any); list {
							continue
						}
					}
					v, valid := walk(child, depth+1)
					if !valid {
						return nil, false
					}
					if !sameSchema(child, v) {
						if next == nil {
							next = copyMap(children)
						}
						next[name] = v
					}
				}
				if next != nil {
					set(key, next)
				}
			}
		}
		if raw, exists := node["$ref"]; exists {
			if !modernDialect {
				for key := range node {
					switch key {
					case "$ref", "$defs", "definitions", "title", "description", "$comment", "default", "examples", "example", "readOnly", "writeOnly", "deprecated":
					default:
						return nil, false
					}
				}
			}
			ref, ok := raw.(string)
			if !ok || !(strings.HasPrefix(ref, "#/$defs/") || strings.HasPrefix(ref, "#/definitions/")) || active[ref] || strings.Contains(ref, "%") {
				return nil, false
			}
			for i := 0; i < len(ref); i++ {
				if ref[i] == '~' {
					if i+1 == len(ref) || ref[i+1] != '0' && ref[i+1] != '1' {
						return nil, false
					}
					i++
				}
			}
			target := localRef(root, ref)
			if target == nil {
				return nil, false
			}
			active[ref] = true
			resolved, valid := walk(target, depth+1)
			delete(active, ref)
			if !valid {
				return nil, false
			}
			var constraints []any
			if current, exists := out["allOf"]; exists {
				var ok bool
				constraints, ok = current.([]any)
				if !ok {
					return nil, false
				}
			}
			set("allOf", append([]any{resolved}, constraints...))
			remove("$ref")
			expanded++
		}
		remove("$defs")
		remove("definitions")
		return out, true
	}
	out, ok := walk(root, 0)
	if !ok || expanded == 0 {
		return schema
	}
	if _, valid := schemaSizeBound(out, min(8<<20, max(64<<10, originalSize*4))); !valid {
		return schema
	}
	return out
}

// Account for repeated annotation/data bytes before an expanded tree is JSON encoded.
func schemaSizeBound(value any, limit int) (int, bool) {
	left, nodes := limit, 100000
	charge := func(n int) bool {
		if n > left {
			return false
		}
		left -= n
		return true
	}
	stringCost := func(s string) bool {
		if len(s) > (left-2)/6 {
			return false
		}
		return charge(6*len(s) + 2)
	}
	var visit func(any, int) bool
	visit = func(v any, depth int) bool {
		if depth > 64 || nodes <= 0 || !charge(1) {
			return false
		}
		nodes--
		switch x := v.(type) {
		case map[string]any:
			if !charge(2) {
				return false
			}
			for k, child := range x {
				if !stringCost(k) || !charge(1) || !visit(child, depth+1) {
					return false
				}
			}
			return true
		case []any:
			if !charge(2) {
				return false
			}
			for _, child := range x {
				if !visit(child, depth+1) {
					return false
				}
			}
			return true
		case string:
			return stringCost(x)
		case nil, bool:
			return charge(5)
		case json.Number:
			return charge(len(x))
		case float64, float32, int, int64, int32, uint, uint64, uint32:
			return charge(32)
		default:
			return false
		}
	}
	if !visit(value, 0) {
		return 0, false
	}
	return limit - left, true
}

func mapTools(value any, normalize func(any) any) any {
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
			nested := mapTools(tool["tools"], normalize)
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
			mended := normalize(old)
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

func workBuddySchema(schema any) any {
	root, _ := schema.(map[string]any)
	budget := 100000
	var walk func(any, int, bool) any
	walk = func(value any, depth int, keepBool bool) any {
		if depth > 64 || budget <= 0 {
			return value
		}
		budget--
		if b, ok := value.(bool); ok {
			if keepBool || depth == 0 {
				return value
			}
			if b {
				return map[string]any{}
			}
			return map[string]any{"not": map[string]any{}}
		}
		node, ok := value.(map[string]any)
		if !ok {
			return value
		}
		if depth > 0 && (node["$id"] != nil || node["id"] != nil) {
			return value
		}
		out := node
		set := func(key string, value any) {
			if reflectMap(out) == reflectMap(node) {
				out = copyMap(node)
			}
			out[key] = value
		}
		remove := func(key string) {
			if reflectMap(out) == reflectMap(node) {
				out = copyMap(node)
			}
			delete(out, key)
		}
		if _, typed := node["type"]; !typed {
			if branches, ok := node["oneOf"].([]any); ok && len(branches) > 0 {
				all := true
				for _, branch := range branches {
					if !objectBranch(root, branch, nil) {
						all = false
						break
					}
				}
				if all {
					set("type", "object")
				}
			}
		}
		if node["pattern"] == `^[^\uD800-\uDFFF]*$` {
			remove("pattern")
		}
		if pattern, ok := node["pattern"].(string); ok && strings.Contains(pattern, `\0`) {
			var text strings.Builder
			for i := 0; i < len(pattern); i++ {
				if pattern[i] == '\\' && i+1 < len(pattern) {
					if pattern[i+1] == '\\' {
						text.WriteString(pattern[i : i+2])
						i++
						continue
					}
					if pattern[i+1] == '0' && (i+2 == len(pattern) || pattern[i+2] < '0' || pattern[i+2] > '9') {
						text.WriteString(`\u0000`)
						i++
						continue
					}
				}
				text.WriteByte(pattern[i])
			}
			if fixed := text.String(); fixed != pattern {
				set("pattern", fixed)
			}
		}
		if values, ok := node["enum"].([]any); ok {
			if len(values) == 0 {
				remove("enum")
				set("not", map[string]any{})
			} else {
				declared, valid := schemaTypes(node["type"])
				nonNull := 0
				for _, kind := range declared {
					if kind != "null" {
						nonNull++
					}
				}
				if valid && (len(declared) == 0 || nonNull > 1) {
					kinds := map[string][]any{}
					var order []string
					for _, v := range values {
						kind := enumKind(v)
						if kind == "" {
							valid = false
							break
						}
						if _, ok := kinds[kind]; !ok {
							order = append(order, kind)
						}
						kinds[kind] = append(kinds[kind], v)
					}
					if valid && len(order) == 1 && len(declared) == 0 {
						set("type", order[0])
					} else if valid {
						all, hasAll := out["allOf"].([]any)
						if out["allOf"] == nil || hasAll {
							all = append([]any(nil), all...)
							if len(declared) > 0 {
								branches := make([]any, 0, len(declared))
								for _, kind := range declared {
									branches = append(branches, map[string]any{"type": kind})
								}
								all = append(all, map[string]any{"anyOf": branches})
								remove("type")
							}
							branches := make([]any, 0, len(order))
							for _, kind := range order {
								branches = append(branches, map[string]any{"type": kind, "enum": kinds[kind]})
							}
							all = append(all, map[string]any{"anyOf": branches})
							remove("enum")
							set("allOf", all)
						}
					}
				}
			}
		}
		for _, key := range []string{"items", "additionalItems", "unevaluatedItems", "contains", "not", "if", "then", "else", "propertyNames", "additionalProperties", "unevaluatedProperties"} {
			if child, exists := out[key]; exists {
				next := walk(child, depth+1, key == "additionalProperties" || key == "unevaluatedProperties")
				if !sameSchema(child, next) {
					set(key, next)
				}
			}
		}
		for _, key := range []string{"anyOf", "oneOf", "allOf", "prefixItems"} {
			if list, ok := out[key].([]any); ok {
				var next []any
				for i, child := range list {
					fixed := walk(child, depth+1, false)
					if !sameSchema(child, fixed) {
						if next == nil {
							next = append([]any(nil), list...)
						}
						next[i] = fixed
					}
				}
				if next != nil {
					set(key, next)
				}
			}
		}
		for _, key := range []string{"properties", "patternProperties", "$defs", "definitions", "dependentSchemas"} {
			if fields, ok := out[key].(map[string]any); ok {
				var next map[string]any
				for name, child := range fields {
					fixed := walk(child, depth+1, false)
					if !sameSchema(child, fixed) {
						if next == nil {
							next = copyMap(fields)
						}
						next[name] = fixed
					}
				}
				if next != nil {
					set(key, next)
				}
			}
		}
		return out
	}
	return walk(schema, 0, false)
}

func sameSchema(a, b any) bool {
	if left, ok := a.(map[string]any); ok {
		right, ok := b.(map[string]any)
		return ok && reflectMap(left) == reflectMap(right)
	}
	return reflect.DeepEqual(a, b)
}

func schemaTypes(value any) ([]string, bool) {
	if value == nil {
		return nil, true
	}
	var raw []any
	switch v := value.(type) {
	case string:
		raw = []any{v}
	case []any:
		raw = v
	default:
		return nil, false
	}
	var out []string
	for _, v := range raw {
		s, ok := v.(string)
		if !ok {
			return nil, false
		}
		switch s {
		case "string", "number", "integer", "object", "array", "boolean", "null":
			out = append(out, s)
		default:
			return nil, false
		}
	}
	return out, true
}

func enumKind(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64, float32, int, int64, json.Number:
		return "number"
	case map[string]any:
		return "object"
	case []any:
		return "array"
	}
	return ""
}

func reflectMap(m map[string]any) uintptr { return reflect.ValueOf(m).Pointer() }
func reflectSlice(v any) uintptr {
	if s, ok := v.([]any); ok {
		return reflect.ValueOf(s).Pointer()
	}
	return 0
}
