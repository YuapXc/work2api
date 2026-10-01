package bridge

import (
	"encoding/json"
	"strings"
)

func textBlocks(blocks []interface{}, separator string) string {
	var text strings.Builder
	for _, block := range blocks {
		b, _ := block.(map[string]interface{})
		if t, ok := b["text"].(string); ok {
			if text.Len() > 0 {
				text.WriteString(separator)
			}
			text.WriteString(t)
		}
	}
	return text.String()
}

// imageContentParts 把入站消息 content 数组里的图片块归一成上游 COSY 接口
// 接受的 {"type":"image_url","image_url":{"url":...}} 形式。识别三种入站格式：
//   - OpenAI Chat:  {"type":"image_url","image_url":{"url":...}}
//   - Responses:    {"type":"input_image","image_url":"..." 或 {url:...}}
//   - Anthropic:    {"type":"image","source":{type:url|base64, media_type, data}}
//
// base64 图片转成 data: URL（上游按 data URL 接收）。返回 (parts, hasImage)。
// url 与 base64 数据都缺失的图片块被跳过——上游收到无 url 的图片块会直接拒单。
func imageContentParts(blocks []interface{}) ([]interface{}, bool) {
	out := make([]interface{}, 0, len(blocks))
	hasImage := false
	appendImage := func(url string) {
		if url == "" {
			return
		}
		out = append(out, map[string]interface{}{
			"type":      "image_url",
			"image_url": map[string]interface{}{"url": url},
		})
		hasImage = true
	}
	for _, block := range blocks {
		b, _ := block.(map[string]interface{})
		if b == nil {
			continue
		}
		switch b["type"] {
		case "text", "input_text", "output_text":
			if t, ok := b["text"].(string); ok {
				out = append(out, map[string]interface{}{"type": "text", "text": t})
			}
		case "image_url":
			// OpenAI: image_url 是 {url} 或裸字符串
			switch raw := b["image_url"].(type) {
			case map[string]interface{}:
				appendImage(StrVal(raw, "url"))
			case string:
				appendImage(raw)
			}
		case "input_image":
			// Responses: image_url 可为字符串或 {url}
			if s, ok := b["image_url"].(string); ok {
				appendImage(s)
			} else if m, ok := b["image_url"].(map[string]interface{}); ok {
				appendImage(StrVal(m, "url"))
			}
		case "image":
			// Anthropic: source {type:"url"} 或 {type:"base64", media_type, data}
			src, _ := b["source"].(map[string]interface{})
			if StrVal(src, "type") == "url" {
				appendImage(StrVal(src, "url"))
			} else if data := StrVal(src, "data"); data != "" {
				mediaType := StrValDefault(src, "media_type", "image/png")
				appendImage("data:" + mediaType + ";base64," + data)
			}
		}
	}
	return out, hasImage
}

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
		// Emit all tool replies before their image attachments, preserving pairing.
		if role, _ := mm["role"].(string); role == "user" {
			if blocks, ok := mm["content"].([]interface{}); ok && HasBlockType(blocks, "tool_result") {
				var attachments, siblings []interface{}
				for _, block := range blocks {
					b, _ := block.(map[string]interface{})
					if b == nil || b["type"] != "tool_result" {
						siblings = append(siblings, block)
						continue
					}
					tool := map[string]interface{}{"role": "tool", "tool_call_id": b["tool_use_id"], "content": b["content"]}
					reply, image := toolReplyWithImages(tool)
					rebuilt = append(rebuilt, reply)
					if image != nil {
						attachments = append(attachments, image)
					}
				}
				rebuilt = append(rebuilt, attachments...)
				if user := ConvertIncomingMessage(map[string]interface{}{"role": "user", "content": siblings}, toolsEnabled); user != nil {
					rebuilt = append(rebuilt, user)
				}
				continue
			}
		}
		if mm["role"] == "tool" {
			if blocks, ok := mm["content"].([]interface{}); ok {
				if _, images := imageContentParts(blocks); images {
					reply, image := toolReplyWithImages(mm)
					rebuilt = append(rebuilt, reply)
					// Consecutive tool results must stay together. Delay images until
					// the complete incoming sequence has been rebuilt below.
					rebuilt = append(rebuilt, image)
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
	return normalizeOutboundMessages(orderToolAttachments(rebuilt))
}

// toolReplyWithImages preserves the tool ID and text, and forwards images as
// user content because COSY accepts image arrays only in user messages.
func toolReplyWithImages(tool map[string]interface{}) (map[string]interface{}, map[string]interface{}) {
	reply := BuildStructuredMessage("tool", NormalizeMessageContent(tool))
	for _, key := range []string{"tool_call_id", "name"} {
		if value, ok := tool[key]; ok {
			reply[key] = value
		}
	}
	blocks, ok := tool["content"].([]interface{})
	if !ok {
		if tool["content"] == nil {
			reply["content"] = ""
		}
		return reply, nil
	}
	reply["content"] = textBlocks(blocks, "\n")
	parts, images := imageContentParts(blocks)
	if !images {
		return reply, nil
	}
	// Text already belongs to the paired tool reply. Attach only the images here.
	imageParts := []interface{}{map[string]interface{}{"type": "text", "text": "Images returned by tool call " + StrVal(tool, "tool_call_id") + ":"}}
	for _, part := range parts {
		if b, ok := part.(map[string]interface{}); ok && b["type"] == "image_url" {
			imageParts = append(imageParts, part)
		}
	}
	image := ConvertIncomingMessage(map[string]interface{}{"role": "user", "content": imageParts}, false)
	image["_tool_image_attachment"] = true
	return reply, image
}

func orderToolAttachments(messages []interface{}) []interface{} {
	ordered := make([]interface{}, 0, len(messages))
	var pending []interface{}
	flush := func() { ordered = append(ordered, pending...); pending = nil }
	for _, item := range messages {
		msg, _ := item.(map[string]interface{})
		if marker, _ := msg["_tool_image_attachment"].(bool); marker {
			delete(msg, "_tool_image_attachment")
			pending = append(pending, item)
			continue
		}
		if msg["role"] != "tool" {
			flush()
		}
		ordered = append(ordered, item)
	}
	flush()
	return ordered
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
			var textParts strings.Builder
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
						textParts.WriteString(t)
					}
				}
			}
			if textParts.Len() > 0 {
				out["content"] = textParts.String()
			}
			if len(toolCalls) > 0 {
				out["tool_calls"] = toolCalls
			}
			return out
		}

		// The full sequence (including images and sibling blocks) is expanded by
		// BuildQoderMessages; this singular converter preserves the first reply.
		if role == "user" && HasBlockType(contentArr, "tool_result") {
			for _, block := range contentArr {
				b, _ := block.(map[string]interface{})
				if b != nil && b["type"] == "tool_result" {
					reply, _ := toolReplyWithImages(map[string]interface{}{"role": "tool", "tool_call_id": b["tool_use_id"], "content": b["content"]})
					return reply
				}
			}
		}

		// Handle thinking blocks - skip them
		if blockType == "thinking" || blockType == "redacted_thinking" {
			// Extract only text blocks
			var textParts strings.Builder
			for _, block := range contentArr {
				b, _ := block.(map[string]interface{})
				if b != nil && b["type"] == "text" {
					if t, ok := b["text"].(string); ok {
						textParts.WriteString(t)
					}
				}
			}
			if textParts.Len() == 0 {
				return nil
			}
			return BuildStructuredMessage(role, textParts.String())
		}
	}

	text := NormalizeMessageContent(msg)

	// user 消息含图片块：content 用归一化数组（text + image_url）透传上游，
	// 不再附 contents——两者并存时上游只读 contents，图片会丢（Python 版实测结论）。
	if role == "user" {
		if contentArr, ok := content.([]interface{}); ok {
			if parts, hasImage := imageContentParts(contentArr); hasImage {
				return map[string]interface{}{
					"role":                        "user",
					"content":                     parts,
					"response_meta":               BlankResponseMeta(),
					"reasoning_content_signature": "",
				}
			}
		}
	}

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
	return textBlocks(arr, "\n\n")
}
