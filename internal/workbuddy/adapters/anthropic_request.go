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
			converted, err := convertAnthropicMessage(mm)
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
	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		chat["tools"] = convertAnthropicTools(tools)
	}
	if tc, ok := body["tool_choice"]; ok {
		switch v := tc.(type) {
		case map[string]any:
			typ, _ := v["type"].(string)
			if typ == "" {
				typ = "any"
			}
			name, _ := v["name"].(string)
			chat["tool_choice"] = map[string]any{"type": typ, "function": map[string]any{"name": name}}
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
func contentWithImages(blocks any) (any, error) {
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

func convertAnthropicMessage(msg map[string]any) ([]any, error) {
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
				output, err := contentWithImages(block["content"])
				if err != nil {
					return nil, err
				}
				result = append(result, map[string]any{"role": "tool", "tool_call_id": tcID, "content": output})
			} else {
				userBlocks = append(userBlocks, block)
			}
		}
		if len(userBlocks) > 0 {
			c, err := contentWithImages(userBlocks)
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
