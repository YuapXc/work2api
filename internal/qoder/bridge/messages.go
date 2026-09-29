package bridge

import "encoding/json"

func BuildQoderMessages(templateMsgs []interface{}, incoming []interface{}, prompt string, toolsEnabled bool) []interface{} {
	var rebuilt []interface{}

	hasIncomingSystem := false
	for _, m := range incoming {
		if mm, ok := m.(map[string]interface{}); ok && mm["role"] == "system" {
			hasIncomingSystem = true
			break
		}
	}
	if !hasIncomingSystem && templateMsgs != nil {
		for _, m := range templateMsgs {
			if mm, ok := m.(map[string]interface{}); ok && mm["role"] == "system" {
				rebuilt = append(rebuilt, DeepCopyMap(mm))
			}
		}
	}

	for _, m := range incoming {
		mm, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		// Handle multi-tool-result: a single Claude user message may contain multiple tool_results
		if role, _ := mm["role"].(string); role == "user" {
			if contentArr, ok := mm["content"].([]interface{}); ok && len(contentArr) > 0 {
				if first, ok := contentArr[0].(map[string]interface{}); ok && first["type"] == "tool_result" {
					for _, block := range contentArr {
						b, _ := block.(map[string]interface{})
						if b == nil || b["type"] != "tool_result" {
							continue
						}
						toolCallId, _ := b["tool_use_id"].(string)
						resultContent := ""
						switch c := b["content"].(type) {
						case string:
							resultContent = c
						case []interface{}:
							for _, part := range c {
								if pm, ok := part.(map[string]interface{}); ok {
									if t, ok := pm["text"].(string); ok {
										resultContent += t
									}
								}
							}
						}
						rebuilt = append(rebuilt, map[string]interface{}{
							"role":         "tool",
							"tool_call_id": toolCallId,
							"content":      resultContent,
						})
					}
					continue
				}
			}
		}
		converted := ConvertIncomingMessage(mm, toolsEnabled)
		if converted != nil {
			rebuilt = append(rebuilt, converted)
		}
	}

	if len(rebuilt) == 0 && prompt != "" {
		rebuilt = append(rebuilt, BuildUserMessage(prompt))
	}
	return normalizeOutboundMessages(rebuilt)
}

// normalizeOutboundMessages 对即将出站的消息逐条做上游适配。三条上游硬性要求
// （buddy-proxy #45，2026-09 实测）：
//
//  1. 带 tool_calls 的消息 content 不能是 null——Anthropic 纯 tool_use 回合转
//     出来正是 content:null，上游拒单且报错文案误导（说 role 'tool' 必须回应
//     带 tool_calls 的消息）。content="" 实测 200。不绑 role（绑 assistant 的
//     话 developer 先被改写就命中不了），且必须排在摘 tool_calls 之前。
//  2. developer role 整请求被拒（反序列化阶段就挂），转 system。
//  3. tool_calls 只能挂 assistant 上：system 带 tool_calls 一样被拒（其后的
//     tool 没有 assistant 可配对），所以 developer→system 时连 tool_calls 一起
//     摘掉（系统消息本就不该发起工具调用，摘掉不丢信息）。
func normalizeOutboundMessages(msgs []interface{}) []interface{} {
	for i, m := range msgs {
		mm, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		// content 修正必须在摘 tool_calls 之前：摘掉后判据永远不成立
		if mm["tool_calls"] != nil && mm["content"] == nil {
			mm["content"] = ""
		}
		if role, _ := mm["role"].(string); role == "developer" {
			delete(mm, "tool_calls")
			mm["role"] = "system"
		}
		msgs[i] = mm
	}
	return msgs
}

func ConvertIncomingMessage(msg map[string]interface{}, toolsEnabled bool) map[string]interface{} {
	role, _ := msg["role"].(string)
	content := msg["content"]

	// Handle Claude Messages API content blocks (array of typed blocks)
	if contentArr, ok := content.([]interface{}); ok && len(contentArr) > 0 {
		firstBlock, _ := contentArr[0].(map[string]interface{})
		blockType, _ := firstBlock["type"].(string)

		// assistant message with tool_use blocks
		if role == "assistant" && HasBlockType(contentArr, "tool_use") {
			out := map[string]interface{}{"role": "assistant", "content": ""}
			var toolCalls []interface{}
			var textParts string
			for _, block := range contentArr {
				b, _ := block.(map[string]interface{})
				if b == nil {
					continue
				}
				switch b["type"] {
				case "tool_use":
					inputJSON, _ := json.Marshal(b["input"])
					toolCalls = append(toolCalls, map[string]interface{}{
						"id":   b["id"],
						"type": "function",
						"function": map[string]interface{}{
							"name":      b["name"],
							"arguments": string(inputJSON),
						},
					})
				case "text":
					if t, ok := b["text"].(string); ok {
						textParts += t
					}
				}
			}
			if textParts != "" {
				out["content"] = textParts
			}
			if len(toolCalls) > 0 {
				out["tool_calls"] = toolCalls
			}
			return out
		}

		// user message with tool_result blocks
		if role == "user" && blockType == "tool_result" {
			var results []map[string]interface{}
			for _, block := range contentArr {
				b, _ := block.(map[string]interface{})
				if b == nil || b["type"] != "tool_result" {
					continue
				}
				toolCallId, _ := b["tool_use_id"].(string)
				resultContent := ""
				switch c := b["content"].(type) {
				case string:
					resultContent = c
				case []interface{}:
					for _, part := range c {
						if pm, ok := part.(map[string]interface{}); ok {
							if t, ok := pm["text"].(string); ok {
								resultContent += t
							}
						}
					}
				}
				results = append(results, map[string]interface{}{
					"role":         "tool",
					"tool_call_id": toolCallId,
					"content":      resultContent,
				})
			}
			if len(results) == 1 {
				return results[0]
			}
			// Multiple tool results: return first, caller should handle multi
			// Actually we need to return all - use a special marker
			if len(results) > 0 {
				return results[0]
			}
			return nil
		}

		// Handle thinking blocks - skip them
		if blockType == "thinking" || blockType == "redacted_thinking" {
			// Extract only text blocks
			var textParts string
			for _, block := range contentArr {
				b, _ := block.(map[string]interface{})
				if b != nil && b["type"] == "text" {
					if t, ok := b["text"].(string); ok {
						textParts += t
					}
				}
			}
			if textParts == "" {
				return nil
			}
			return BuildStructuredMessage(role, textParts)
		}
	}

	text := NormalizeMessageContent(msg)

	if role == "assistant" && toolsEnabled {
		if tc, ok := msg["tool_calls"]; ok {
			out := BuildStructuredMessage("assistant", text)
			out["tool_calls"] = tc
			return out
		}
	}

	if role == "tool" {
		out := BuildStructuredMessage("tool", text)
		if name, ok := msg["name"].(string); ok {
			out["name"] = name
		}
		if tcid, ok := msg["tool_call_id"].(string); ok {
			out["tool_call_id"] = tcid
		}
		return out
	}

	if text == "" {
		return nil
	}

	if role == "user" {
		return BuildUserMessage(text)
	}
	return BuildStructuredMessage(role, text)
}

func HasBlockType(blocks []interface{}, blockType string) bool {
	for _, block := range blocks {
		if b, ok := block.(map[string]interface{}); ok {
			if b["type"] == blockType {
				return true
			}
		}
	}
	return false
}

func BuildUserMessage(text string) map[string]interface{} {
	return map[string]interface{}{
		"role":    "user",
		"content": "",
		"contents": []interface{}{
			map[string]interface{}{"type": "text", "text": text},
		},
		"response_meta":               BlankResponseMeta(),
		"reasoning_content_signature": "",
	}
}

func BuildStructuredMessage(role, text string) map[string]interface{} {
	return map[string]interface{}{
		"role":                        role,
		"content":                     text,
		"response_meta":               BlankResponseMeta(),
		"reasoning_content_signature": "",
	}
}

func BlankResponseMeta() map[string]interface{} {
	return map[string]interface{}{
		"id": "",
		"usage": map[string]interface{}{
			"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0,
			"completion_tokens_details": map[string]interface{}{"reasoning_tokens": 0},
			"prompt_tokens_details":     map[string]interface{}{"cached_tokens": 0},
		},
	}
}

func NormalizeMessageContent(msg map[string]interface{}) string {
	c := msg["content"]
	if s, ok := c.(string); ok {
		return s
	}
	if c == nil {
		return ""
	}
	arr, ok := c.([]interface{})
	if !ok {
		data, _ := json.Marshal(c)
		return string(data)
	}
	var sb string
	for _, block := range arr {
		b, _ := block.(map[string]interface{})
		if t, ok := b["text"].(string); ok {
			if sb != "" {
				sb += "\n\n"
			}
			sb += t
		}
	}
	return sb
}
