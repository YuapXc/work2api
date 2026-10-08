// Package adapters converts between the Anthropic Messages / OpenAI Responses
// protocols and OpenAI Chat Completions. Ported from
// workbuddy_one/adapters/anthropic.py and responses.py.
//
// Claude Code speaks Anthropic Messages (POST /v1/messages) and Codex speaks
// OpenAI Responses (POST /v1/responses), while the CodeBuddy backend only
// speaks OpenAI Chat Completions. These adapters do the bidirectional
// conversion: request in → Chat, and Chat SSE → native SSE event stream.
package adapters

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

func randID(prefix string) string {
	buf := make([]byte, 12)
	_, _ = rand.Read(buf)
	return prefix + hex.EncodeToString(buf)
}

// AnthropicRequestToChat converts an Anthropic Messages request body into an
// OpenAI Chat Completions request body.
func AnthropicRequestToChat(body map[string]any) (map[string]any, error) {
	var messages []any
	tools, catalog, err := anthropicClientTools(body)
	if err != nil {
		return nil, err
	}

	if system := body["system"]; system != nil {
		if sysContent := extractSystemText(system); sysContent != "" {
			messages = append(messages, map[string]any{"role": "system", "content": sysContent})
		}
	}

	if msgs, ok := body["messages"].([]any); ok {
		for _, m := range msgs {
			mm, ok := m.(map[string]any)
			if !ok {
				continue
			}
			converted, err := convertAnthropicMessage(mm, catalog)
			if err != nil {
				return nil, err
			}
			messages = append(messages, converted...)
		}
	}

	chat := map[string]any{
		"messages":       messages,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if v, ok := body["model"]; ok {
		chat["model"] = v
	}
	if v, ok := body["max_tokens"]; ok {
		chat["max_tokens"] = v
	}
	if oc, ok := body["output_config"].(map[string]any); ok {
		if effort, ok := oc["effort"].(string); ok {
			switch strings.ToLower(effort) {
			case "low", "medium", "high", "xhigh", "max":
				chat["reasoning_effort"] = strings.ToLower(effort)
			}
		}
	}
	// thinking.budget_tokens → reasoning_effort 档位。显式 output_config.effort
	// 恒优先（上方已写入）；budget 只在客户端未声明 effort 时生效。ladder 与
	// core/protocol 的 effortForThinkingBudget 对齐：8192→high、16384→xhigh、
	// 32768→max，往返不重排档位。
	if _, stated := chat["reasoning_effort"]; !stated {
		if th, ok := body["thinking"].(map[string]any); ok {
			if typ, _ := th["type"].(string); strings.EqualFold(typ, "disabled") {
				// 显式关闭：不注入档位。
			} else if effort := budgetToEffort(jsonNum(th["budget_tokens"])); effort != "" {
				chat["reasoning_effort"] = effort
			}
		}
	}
	if len(tools) > 0 {
		chat["tools"] = convertAnthropicTools(tools)
	}
	if tc, ok := body["tool_choice"]; ok {
		switch v := tc.(type) {
		case map[string]any:
			if value, present := v["disable_parallel_tool_use"]; present {
				disabled, ok := value.(bool)
				if !ok {
					return nil, fmt.Errorf("tool_choice.disable_parallel_tool_use 必须为布尔值")
				}
				chat["parallel_tool_calls"] = !disabled
			}
			typ, _ := v["type"].(string)
			if typ == "" {
				typ = "any"
			}
			switch typ {
			case "any":
				chat["tool_choice"] = "required"
			case "auto", "none":
				chat["tool_choice"] = typ
			case "tool":
				name, _ := v["name"].(string)
				if strings.TrimSpace(name) == "" {
					return nil, fmt.Errorf("tool_choice.type=tool 必须指定 name")
				}
				chat["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
			default:
				return nil, fmt.Errorf("不支持的 tool_choice.type: %s", typ)
			}
		case string:
			if v == "none" || v == "auto" || v == "required" {
				chat["tool_choice"] = v
			} else {
				chat["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": v}}
			}
		}
	}
	for _, key := range []string{"temperature", "top_p", "stop"} {
		if v, ok := body[key]; ok {
			chat[key] = v
		}
	}
	if v, ok := body["stop_sequences"]; ok {
		chat["stop"] = v
	}
	return chat, nil
}

// budgetToEffort maps an Anthropic thinking budget onto an effort rung, same
// ladder as core/protocol.effortForThinkingBudget: 32768+→max, 16384+→xhigh,
// 8192+→high, 2048+→medium, else low. Missing budget defaults to "high"
// (thinking enabled without a budget asks for deep thinking). Empty means the
// caller should not inject an effort.
func budgetToEffort(budget int) string {
	switch {
	case budget <= 0:
		// thinking:{enabled} without a budget asks for deep thinking.
		return "high"
	case budget >= 32768:
		return "max"
	case budget >= 16384:
		return "xhigh"
	case budget >= 8192:
		return "high"
	case budget > 2048:
		return "medium"
	default:
		return "low"
	}
}

// jsonNum reads a JSON number out of a decoded body (float64/int shapes).
func jsonNum(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	}
	return 0
}

func extractSystemText(system any) string {
	switch v := system.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, b := range v {
			if block, ok := b.(map[string]any); ok && block["type"] == "text" {
				text, _ := block["text"].(string)
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// contentWithImages maps Anthropic content blocks to Chat content, preserving
// image blocks (returns either a string or a []any of typed parts).
func contentWithImages(blocks any, catalog map[string]map[string]any) (any, error) {
	switch v := blocks.(type) {
	case string:
		return v, nil
	case []any:
		var parts []any
		var textOnly strings.Builder
		hasImage := false
		for _, b := range v {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "tool_reference":
				name, _ := block["tool_name"].(string)
				if _, ok := catalog[name]; !ok || name == "" {
					return nil, fmt.Errorf("tool_reference 必须引用当前 tools 中已定义的客户端工具")
				}
				// Chat has no tool_reference block. Keep the discovery result in
				// the tool's output; the corresponding schema is loaded in tools.
				encoded, _ := json.Marshal(map[string]any{"type": "tool_reference", "tool_name": name})
				text := "\n" + string(encoded) + "\n"
				parts = append(parts, map[string]any{"type": "text", "text": text})
				textOnly.WriteString(text)
			case "text":
				text, _ := block["text"].(string)
				parts = append(parts, map[string]any{"type": "text", "text": text})
				textOnly.WriteString(text)
			case "image":
				source, ok := block["source"].(map[string]any)
				if !ok {
					return nil, errInvalidImageSource
				}
				var url string
				if source["type"] == "url" {
					url, _ = source["url"].(string)
				} else {
					data, _ := source["data"].(string)
					mediaType, _ := source["media_type"].(string)
					if mediaType == "" {
						mediaType = "image/png"
					}
					if data != "" {
						url = "data:" + mediaType + ";base64," + data
					}
				}
				if url == "" {
					return nil, errImageMissingData
				}
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				hasImage = true
			default:
				return nil, fmt.Errorf("不支持的 Anthropic 内容块类型: %v", block["type"])
			}
		}
		if hasImage {
			return parts, nil
		}
		return textOnly.String(), nil
	case nil:
		return "", nil
	default:
		return "", nil
	}
}

func convertAnthropicMessage(msg map[string]any, catalog map[string]map[string]any) ([]any, error) {
	role, _ := msg["role"].(string)
	content := msg["content"]

	if s, ok := content.(string); ok {
		return []any{map[string]any{"role": role, "content": s}}, nil
	}
	blocks, ok := content.([]any)
	if !ok || len(blocks) == 0 {
		return nil, nil
	}

	switch role {
	case "user":
		var result []any
		var userBlocks []any
		for _, b := range blocks {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			if block["type"] == "tool_result" {
				tcID, _ := block["tool_use_id"].(string)
				output, err := contentWithImages(block["content"], catalog)
				if err != nil {
					return nil, err
				}
				if value, present := block["is_error"]; present {
					isError, ok := value.(bool)
					if !ok {
						return nil, fmt.Errorf("tool_result.is_error 必须为布尔值")
					}
					if isError {
						const prefix = "[tool_result is_error=true]\n"
						switch value := output.(type) {
						case string:
							output = prefix + value
						case []any:
							output = append([]any{map[string]any{"type": "text", "text": prefix}}, value...)
						}
					}
				}
				result = append(result, map[string]any{"role": "tool", "tool_call_id": tcID, "content": output})
			} else {
				userBlocks = append(userBlocks, block)
			}
		}
		if len(userBlocks) > 0 {
			c, err := contentWithImages(userBlocks, catalog)
			if err != nil {
				return nil, err
			}
			result = append(result, map[string]any{"role": "user", "content": c})
		}
		return result, nil

	case "assistant":
		var textParts []string
		var toolCalls []any
		for _, b := range blocks {
			block, ok := b.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "text":
				t, _ := block["text"].(string)
				textParts = append(textParts, t)
			case "tool_use":
				id, _ := block["id"].(string)
				if id == "" {
					id = randID("call_")
				}
				name, _ := block["name"].(string)
				input := block["input"]
				if input == nil {
					input = map[string]any{}
				}
				args, _ := json.Marshal(input)
				toolCalls = append(toolCalls, map[string]any{
					"id":       id,
					"type":     "function",
					"function": map[string]any{"name": name, "arguments": string(args)},
				})
			}
		}
		out := map[string]any{"role": "assistant", "content": strings.Join(textParts, "")}
		if len(toolCalls) > 0 {
			out["tool_calls"] = toolCalls
		}
		return []any{out}, nil

	default:
		text := extractBlocksText(blocks)
		if text != "" {
			return []any{map[string]any{"role": role, "content": text}}, nil
		}
		return nil, nil
	}
}

func extractBlocksText(blocks []any) string {
	var parts []string
	for _, b := range blocks {
		if block, ok := b.(map[string]any); ok && block["type"] == "text" {
			t, _ := block["text"].(string)
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "")
}

// anthropicClientTools implements client-side discovery for a Chat upstream.
// Anthropic requires all deferred definitions in each request, but exposes only
// non-deferred and discovered tools to the model. References in prior tool
// results remain effective on subsequent turns. This does not execute server
// tools or scan the client's filesystem for skills/MCP tools.
// https://platform.claude.com/docs/en/agents-and-tools/tool-use/tool-search-tool
func anthropicClientTools(body map[string]any) ([]any, map[string]map[string]any, error) {
	catalog := make(map[string]map[string]any)
	loaded := make(map[string]bool)
	tools, _ := body["tools"].([]any)
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("tools 必须包含工具定义对象")
		}
		typ, _ := tool["type"].(string)
		if typ != "" && typ != "custom" && typ != "function" {
			return nil, nil, fmt.Errorf("Chat 上游不支持该 Anthropic 工具类型: %s", typ)
		}
		definition := tool
		if fn, ok := tool["function"].(map[string]any); ok {
			definition = fn
		}
		name, _ := definition["name"].(string)
		if strings.TrimSpace(name) == "" {
			return nil, nil, fmt.Errorf("客户端工具必须指定 name")
		}
		if _, exists := catalog[name]; exists {
			return nil, nil, fmt.Errorf("客户端工具 name 不得重复")
		}
		catalog[name] = tool
		deferred := false
		if value, present := tool["defer_loading"]; present {
			var valid bool
			deferred, valid = value.(bool)
			if !valid {
				return nil, nil, fmt.Errorf("工具 defer_loading 必须为布尔值")
			}
		}
		loaded[name] = !deferred
	}
	var discover func(any) error
	discover = func(content any) error {
		blocks, _ := content.([]any)
		for _, raw := range blocks {
			block, _ := raw.(map[string]any)
			switch block["type"] {
			case "tool_reference":
				name, _ := block["tool_name"].(string)
				if _, exists := catalog[name]; !exists || name == "" {
					return fmt.Errorf("tool_reference 必须引用当前 tools 中已定义的客户端工具")
				}
				loaded[name] = true
			case "tool_use":
				// A previously invoked deferred tool must remain callable even
				// after client compaction removes its original search result.
				if name, _ := block["name"].(string); catalog[name] != nil {
					loaded[name] = true
				}
			case "tool_result":
				if err := discover(block["content"]); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if messages, ok := body["messages"].([]any); ok {
		for _, raw := range messages {
			message, _ := raw.(map[string]any)
			if err := discover(message["content"]); err != nil {
				return nil, nil, err
			}
		}
	}
	var chosenName string
	switch choice := body["tool_choice"].(type) {
	case map[string]any:
		if choice["type"] == "tool" {
			chosenName, _ = choice["name"].(string)
		}
	case string:
		// Keep the pre-existing named-string compatibility form consistent
		// with the native Anthropic object form when tools are deferred.
		if choice != "auto" && choice != "none" && choice != "required" {
			chosenName = choice
		}
	}
	if catalog[chosenName] != nil {
		loaded[chosenName] = true
	}
	var result []any
	for _, raw := range tools {
		tool := raw.(map[string]any)
		definition := tool
		if fn, ok := tool["function"].(map[string]any); ok {
			definition = fn
		}
		name, _ := definition["name"].(string)
		if loaded[name] {
			result = append(result, tool)
		}
	}
	if len(tools) > 0 && len(result) == 0 {
		return nil, nil, fmt.Errorf("至少需要一个非延迟或已发现的客户端工具")
	}
	return result, catalog, nil
}

func convertAnthropicTools(tools []any) []any {
	var result []any
	for _, t := range tools {
		tt, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if _, has := tt["function"]; has {
			result = append(result, tt)
			continue
		}
		fn := map[string]any{}
		fn["name"], _ = tt["name"].(string)
		if d, ok := tt["description"]; ok {
			fn["description"] = d
		}
		if s, ok := tt["input_schema"]; ok {
			fn["parameters"] = s
		}
		result = append(result, map[string]any{"type": "function", "function": fn})
	}
	return result
}

type adapterError string

func (e adapterError) Error() string { return string(e) }

const (
	errInvalidImageSource = adapterError("图片来源格式无效")
	errImageMissingData   = adapterError("图片内容缺少 URL 或 base64 数据")
)
