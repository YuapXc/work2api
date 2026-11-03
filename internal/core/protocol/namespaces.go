package protocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"work2api/internal/jsonutil"
)

type NamespaceName struct{ Namespace, Name string }

// PrepareClientTools lowers request-local identities without mutating history.
func PrepareClientTools(body map[string]any) (map[string]any, map[string]bool, map[string]NamespaceName, error) {
	needsNamespace := false
	for _, raw := range jsonutil.SliceAt(body, "tools") {
		item, _ := raw.(map[string]any)
		needsNamespace = needsNamespace || jsonutil.StringAt(item, "type") == "namespace"
	}
	for _, raw := range jsonutil.SliceAt(body, "input") {
		item, _ := raw.(map[string]any)
		kind := jsonutil.StringAt(item, "type")
		needsNamespace = needsNamespace || (kind == "function_call" || kind == "custom_tool_call") && jsonutil.StringAt(item, "namespace") != ""
	}
	choice := jsonutil.MapAt(body, "tool_choice")
	needsNamespace = needsNamespace || jsonutil.StringAt(choice, "namespace") != "" || jsonutil.StringAt(choice, "type") == "namespace"
	if !needsNamespace {
		prepared, custom, err := PrepareCustomTools(body)
		return prepared, custom, nil, err
	}
	out := copyCustomMap(body)
	mapping := map[string]NamespaceName{}
	used := map[string]bool{}
	for _, raw := range jsonutil.SliceAt(body, "tools") {
		tool, _ := raw.(map[string]any)
		if jsonutil.StringAt(tool, "type") != "namespace" {
			used[jsonutil.FirstString(jsonutil.StringAt(tool, "name"), jsonutil.StringAt(tool, "function", "name"))] = true
		}
	}
	bind := func(namespace, name string) (string, error) {
		if strings.TrimSpace(namespace) == "" || strings.TrimSpace(name) == "" {
			return "", fmt.Errorf("namespace 工具必须指定名称")
		}
		sum := sha256.Sum256([]byte(namespace + "\x00" + name))
		flat := "ns_" + hex.EncodeToString(sum[:16])
		entry := NamespaceName{namespace, name}
		if used[flat] {
			return "", fmt.Errorf("namespace 工具与普通工具名称冲突")
		}
		if old, ok := mapping[flat]; ok && old != entry {
			return "", fmt.Errorf("namespace 工具名称冲突")
		}
		mapping[flat] = entry
		return flat, nil
	}
	tools := []any{}
	declared := map[string]bool{}
	groups := map[string]map[string]bool{}
	paths := map[string]string{}
	aliases := map[string]string{}
	ambiguous := map[string]bool{}
	var collect func(map[string]any, []string, string) error
	collect = func(tool map[string]any, parents []string, description string) error {
		kind := jsonutil.StringAt(tool, "type")
		if kind == "namespace" {
			name := jsonutil.StringAt(tool, "name")
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("namespace 工具必须指定名称")
			}
			parts := append(append([]string(nil), parents...), name)
			namespace := strings.Join(parts, "__")
			encoded, _ := json.Marshal(parts)
			if old, exists := paths[namespace]; exists && old != string(encoded) {
				return fmt.Errorf("namespace 路径存在歧义")
			}
			paths[namespace] = string(encoded)
			if groups[namespace] == nil {
				groups[namespace] = map[string]bool{}
			}
			children := jsonutil.SliceAt(tool, "tools")
			if len(children) == 0 {
				children = jsonutil.SliceAt(tool, "children")
			}
			if len(children) == 0 {
				children = jsonutil.SliceAt(tool, "functions")
			}
			if len(children) == 0 {
				return fmt.Errorf("namespace 工具不能为空")
			}
			for _, raw := range children {
				child, ok := raw.(map[string]any)
				if !ok {
					return fmt.Errorf("namespace 子工具必须为对象")
				}
				if err := collect(child, parts, description+jsonutil.StringAt(tool, "description")+"\n"); err != nil {
					return err
				}
			}
			return nil
		}
		if len(parents) == 0 {
			tools = append(tools, tool)
			return nil
		}
		if kind != "function" && kind != "custom" {
			return fmt.Errorf("namespace 仅支持 namespace/function/custom 子工具")
		}
		namespace, name := strings.Join(parents, "__"), jsonutil.StringAt(tool, "name")
		flat, err := bind(namespace, name)
		if err != nil {
			return err
		}
		if declared[flat] {
			return fmt.Errorf("namespace 子工具重复")
		}
		declared[flat] = true
		for i := range parents {
			groups[strings.Join(parents[:i+1], "__")][flat] = true
		}
		for _, alias := range []string{namespace + "__" + name, strings.Join(parents, "::") + "::" + name} {
			if previous, exists := aliases[alias]; exists && previous != flat {
				ambiguous[alias] = true
			}
			aliases[alias] = flat
		}
		copy := copyCustomMap(tool)
		copy["name"] = flat
		copy["description"] = strings.TrimSpace("Tool identity: " + namespace + "." + name + ".\n" + description + jsonutil.StringAt(tool, "description"))
		tools = append(tools, copy)
		return nil
	}
	for _, raw := range jsonutil.SliceAt(body, "tools") {
		tool, ok := raw.(map[string]any)
		if !ok {
			return nil, nil, nil, fmt.Errorf("工具定义必须为对象")
		}
		if err := collect(tool, nil, ""); err != nil {
			return nil, nil, nil, err
		}
	}
	resolve := func(namespace, name string) (string, error) {
		if namespace != "" {
			if groups[namespace] == nil && groups[strings.ReplaceAll(namespace, "::", "__")] != nil {
				namespace = strings.ReplaceAll(namespace, "::", "__")
			}
			return bind(namespace, name)
		}
		if used[name] {
			return name, nil
		}
		if ambiguous[name] {
			return "", fmt.Errorf("namespace 工具引用存在歧义")
		}
		if flat := aliases[name]; flat != "" {
			return flat, nil
		}
		return name, nil
	}

	if _, ok := body["tools"]; ok {
		out["tools"] = tools
	}
	if history, ok := body["input"].([]any); ok {
		var cloned []any
		for i, raw := range history {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			kind := jsonutil.StringAt(item, "type")
			if kind == "function_call" || kind == "custom_tool_call" {
				flat, err := resolve(jsonutil.StringAt(item, "namespace"), jsonutil.StringAt(item, "name"))
				if err != nil {
					return nil, nil, nil, err
				}
				if flat == jsonutil.StringAt(item, "name") && jsonutil.StringAt(item, "namespace") == "" {
					continue
				}
				copy := copyCustomMap(item)
				if cloned == nil {
					cloned = append([]any(nil), history...)
				}
				copy["name"] = flat
				delete(copy, "namespace")
				cloned[i] = copy
			}
		}
		if cloned != nil {
			out["input"] = cloned
		}
	}
	if choice, ok := body["tool_choice"].(map[string]any); ok {
		if jsonutil.StringAt(choice, "type") == "namespace" {
			namespace := jsonutil.StringAt(choice, "name")
			members := groups[namespace]
			if members == nil {
				members = groups[strings.ReplaceAll(namespace, "::", "__")]
			}
			if len(members) == 0 {
				return nil, nil, nil, fmt.Errorf("tool_choice 指向未声明的 namespace")
			}
			selected := []any{}
			for _, raw := range tools {
				tool, _ := raw.(map[string]any)
				if members[jsonutil.StringAt(tool, "name")] {
					selected = append(selected, raw)
				}
			}
			out["tools"], out["tool_choice"] = selected, "auto"
		} else {
			name := jsonutil.FirstString(jsonutil.StringAt(choice, "name"), jsonutil.StringAt(choice, "function", "name"))
			namespace := jsonutil.StringAt(choice, "namespace")
			flat, err := resolve(namespace, name)
			if err != nil {
				return nil, nil, nil, err
			}
			if namespace != "" && !declared[flat] {
				return nil, nil, nil, fmt.Errorf("tool_choice 指向未声明的 namespace 工具")
			}
			_, nested := choice["function"].(map[string]any)
			if flat != name || namespace != "" || nested {
				copy := copyCustomMap(choice)
				copy["name"] = flat
				delete(copy, "function")
				delete(copy, "namespace")
				out["tool_choice"] = copy
			}
		}
	}

	prepared, custom, err := PrepareCustomTools(out)
	return prepared, custom, mapping, err
}
