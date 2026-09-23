package adapters

import (
	"encoding/json"
	"strings"
)

// ResponsesRequestToChat converts an OpenAI Responses request body into a Chat
// Completions request body.
func ResponsesRequestToChat(body map[string]any) (map[string]any, error) {
	var messages []any

	if instructions, ok := body["instructions"]; ok && instructions != nil && instructions != "" {
		messages = append(messages, map[string]any{"role": "system", "content": instructions})
	}

	switch inp := body["input"].(type) {
	case string:
		messages = append(messages, map[string]any{"role": "user", "content": inp})
	case []any:
		converted, err := convertInputItems(inp)
		if err != nil {
			return nil, err
		}
		messages = append(messages, converted...)
	}

	chat := map[string]any{
		"messages":       messages,
		"stream":         true,
		"stream_options": map[string]any{"include_usage": true},
	}
	if v, ok := body["model"]; ok {
		chat["model"] = v
	}
	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
		chat["tools"] = convertToolsForChat(tools)
	}
	if tc, ok := body["tool_choice"]; ok {
		chat["tool_choice"] = tc
	}
	for _, key := range []string{"temperature", "top_p", "stop", "seed",
		"presence_penalty", "frequency_penalty", "response_format", "reasoning_effort"} {
		if v, ok := body[key]; ok {
			chat[key] = v
		}
	}
	if v, ok := body["max_output_tokens"]; ok {
		chat["max_tokens"] = v
	} else if v, ok := body["max_tokens"]; ok {
		chat["max_tokens"] = v
	}
	return chat, nil
}

func convertInputItems(items []any) ([]any, error) {
	var messages []any
	var pendingAssistantContent any = nil
	pendingSet := false
	var pendingToolCalls []any

	flushAssistant := func() {
		if pendingSet || len(pendingToolCalls) > 0 {
			content := pendingAssistantContent
			if content == nil {
				content = ""
			}
			msg := map[string]any{"role": "assistant", "content": content}
			if len(pendingToolCalls) > 0 {
				calls := make([]any, len(pendingToolCalls))
				copy(calls, pendingToolCalls)
				msg["tool_calls"] = calls
			}
			messages = append(messages, msg)
			pendingAssistantContent = nil
			pendingSet = false
			pendingToolCalls = nil
		}
	}

	for _, it := range items {
		item, ok := it.(map[string]any)
		if !ok {
			continue
		}
		itemType, hasType := item["type"].(string)
		role, _ := item["role"].(string)

		// simple message {"role":"user","content":...} with no type
		if !hasType && (role == "user" || role == "system" || role == "developer") {
			flushAssistant()
			mapped := role
			if role == "developer" {
				mapped = "system"
			}
			content, err := responsesContentWithImages(item["content"])
			if err != nil {
				return nil, err
			}
			messages = append(messages, map[string]any{"role": mapped, "content": content})
			continue
		}
		if itemType == "message" && (role == "user" || role == "system" || role == "developer") {
			flushAssistant()
			mapped := role
			if role == "developer" {
				mapped = "system"
			}
			content, err := responsesContentWithImages(item["content"])
			if err != nil {
				return nil, err
			}
			messages = append(messages, map[string]any{"role": mapped, "content": content})
			continue
		}
		if itemType == "message" && role == "assistant" {
			flushAssistant()
			content, err := responsesContentWithImages(item["content"])
			if err != nil {
				return nil, err
			}
			pendingAssistantContent = content
			pendingSet = true
			continue
		}
		if !hasType && role == "assistant" {
			flushAssistant()
			content, err := responsesContentWithImages(item["content"])
			if err != nil {
				return nil, err
			}
			pendingAssistantContent = content
			pendingSet = true
			continue
		}
		if itemType == "function_call" {
			if !pendingSet {
				pendingAssistantContent = ""
				pendingSet = true
			}
			id := firstStr(item["call_id"], item["id"])
			if id == "" {
				id = randID("call_")
			}
			name, _ := item["name"].(string)
			args, _ := item["arguments"].(string)
			if args == "" {
				args = "{}"
			}
			pendingToolCalls = append(pendingToolCalls, map[string]any{
				"id":       id,
				"type":     "function",
				"function": map[string]any{"name": name, "arguments": args},
			})
			continue
		}
		if itemType == "function_call_output" {
			flushAssistant()
			callID, _ := item["call_id"].(string)
			content, err := responsesContentWithImages(item["output"])
			if err != nil {
				return nil, err
			}
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": callID, "content": content})
			continue
		}
		// unknown typed item with a role: treat as a plain message
		if role != "" {
			flushAssistant()
			content, err := responsesContentWithImages(item["content"])
			if err != nil {
				return nil, err
			}
			messages = append(messages, map[string]any{"role": role, "content": content})
		}
	}
	flushAssistant()
	return messages, nil
}

func imageDataURL(block map[string]any) string {
	switch url := block["image_url"].(type) {
	case string:
		if url != "" {
			return url
		}
	case map[string]any:
		if u, ok := url["url"].(string); ok && u != "" {
			return u
		}
	}
	if img, ok := block["image"].(map[string]any); ok {
		if data, ok := img["data"].(string); ok && data != "" {
			mt, _ := img["media_type"].(string)
			if mt == "" {
				mt = "image/png"
			}
			return "data:" + mt + ";base64," + data
		}
	}
	return ""
}

func responsesContentWithImages(content any) (any, error) {
	switch v := content.(type) {
	case string:
		return v, nil
	case []any:
		var parts []any
		var textOnly strings.Builder
		hasImage := false
		for _, p := range v {
			if s, ok := p.(string); ok {
				parts = append(parts, map[string]any{"type": "text", "text": s})
				textOnly.WriteString(s)
				continue
			}
			block, ok := p.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "input_text", "text", "output_text":
				t, _ := block["text"].(string)
				parts = append(parts, map[string]any{"type": "text", "text": t})
				textOnly.WriteString(t)
			case "input_image", "image_url", "output_image":
				url := imageDataURL(block)
				if url == "" {
					return nil, adapterError("图片内容缺少可用的 image_url 或 base64 数据")
				}
				image := map[string]any{"url": url}
				if d, ok := block["detail"]; ok && d != nil {
					image["detail"] = d
				}
				parts = append(parts, map[string]any{"type": "image_url", "image_url": image})
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
		b, _ := json.Marshal(v)
		return string(b), nil
	}
}

func convertToolsForChat(tools []any) []any {
	var result []any
	for _, t := range tools {
		tt, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if tt["type"] != "function" {
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
		if p, ok := tt["parameters"]; ok {
			fn["parameters"] = p
		}
		if s, ok := tt["strict"]; ok {
			fn["strict"] = s
		}
		result = append(result, map[string]any{"type": "function", "function": fn})
	}
	return result
}

func firstStr(vals ...any) string {
	for _, v := range vals {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}
