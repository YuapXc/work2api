package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"work2api/internal/workbuddy/adapters"
)

func (b *Bridge) HandleCodexResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		WriteCodexErr(w, err)
		return
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		WriteCodexErr(w, err)
		return
	}
	_, _ = b.ServeCodex(r.Context(), w, req)
}

// ServeCodex runs the Codex/Responses conversion + streaming against an
// already-parsed client body, writing the client response to w.
func (b *Bridge) ServeCodex(ctx context.Context, w http.ResponseWriter, req map[string]interface{}) (ServeResult, error) {
	var result ServeResult
	stream, _ := req["stream"].(bool)
	model := StrValDefault(req, "model", "auto")
	instructions, _ := req["instructions"].(string)
	tools := ConvertResponsesToolsToOpenAI(req["tools"])
	incoming := CodexInputToMessages(req["input"], instructions)
	messages := BuildQoderMessages(b.templateMessages(), incoming, ExtractLatestUserPrompt(incoming), tools != nil)
	conv := adapters.NewResponsesStreamConverter(model)
	if !stream {
		conv.SetNonstream()
	}
	var opened bool
	var writeErr error
	send := func(text string) {
		if text == "" || writeErr != nil {
			return
		}
		if !opened {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(200)
			opened = true
		}
		_, writeErr = io.WriteString(w, text)
		if writeErr == nil {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	toolsSeen := false
	var input, output int
	err := b.CallQoderWithOpts(callCtx, "codex", messages, model, tools, requestCallOpts(req), func(d Delta) {
		result.updateUsage(d, &input, &output)
		delta := map[string]any{}
		if d.Content != "" {
			delta["content"] = d.Content
		}
		if d.Reasoning != "" {
			delta["reasoning_content"] = d.Reasoning
		}
		if len(d.ToolCalls) > 0 {
			delta["tool_calls"] = d.ToolCalls
			toolsSeen = true
		}
		chunk := map[string]any{"choices": []any{map[string]any{"delta": delta, "finish_reason": d.FinishReason}}}
		if d.Usage != nil {
			chunk["usage"] = d.Usage
		}
		raw, _ := json.Marshal(chunk)
		send(conv.FeedLine("data: " + string(raw)))
		if writeErr != nil {
			cancel()
		}
	})
	result.InputTokens, result.OutputTokens = input, output
	result.Output, result.Reasoning = conv.TextContent(), conv.Reasoning()
	if writeErr != nil {
		return result, writeErr
	}
	if err != nil {
		if opened {
			message, _ := FriendlyError(err)
			send(conv.Fail(message, ErrorStatus(err)))
		} else {
			WriteCodexErr(w, err)
		}
		return result, err
	}
	// Qoder may close a successful envelope without a finish_reason. Preserve that
	// established transport contract while honoring every explicit upstream reason.
	if result.FinishReason == "" {
		result.FinishReason = "stop"
		if toolsSeen {
			result.FinishReason = "tool_calls"
		}
		raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"finish_reason": result.FinishReason}}})
		send(conv.FeedLine("data: " + string(raw)))
	}
	if stream {
		err = conv.FinishEvents(func(event map[string]any) error {
			raw, err := json.Marshal(event)
			if err != nil {
				return err
			}
			send("data: " + string(raw) + "\n\n")
			return writeErr
		})
	} else {
		WriteJSON(w, conv.GetNonstreamResponse())
	}
	return result, err
}
func CodexInputToMessages(input interface{}, instructions string) []interface{} {
	var msgs []interface{}

	if instructions != "" {
		msgs = append(msgs, map[string]interface{}{"role": "system", "content": instructions})
	}

	switch v := input.(type) {
	case string:
		msgs = append(msgs, map[string]interface{}{"role": "user", "content": v})
	case []interface{}:
		for _, item := range v {
			itemMap, ok := item.(map[string]interface{})
			if !ok {
				continue
			}
			itemType, _ := itemMap["type"].(string)
			switch itemType {
			case "message":
				role, _ := itemMap["role"].(string)
				content := itemMap["content"]
				if contentStr, ok := content.(string); ok {
					msgs = append(msgs, map[string]interface{}{"role": role, "content": contentStr})
				} else if contentArr, ok := content.([]interface{}); ok {
					// 保留原数组：user 消息里的 input_image 图片块由
					// ConvertIncomingMessage→imageContentParts 归一成上游
					// image_url 形式；纯文本消息最终仍塌缩成字符串。
					msgs = append(msgs, map[string]interface{}{"role": role, "content": contentArr})
				}
			case "function_call":
				name, _ := itemMap["name"].(string)
				args, _ := itemMap["arguments"].(string)
				callId, _ := itemMap["call_id"].(string)
				msgs = append(msgs, map[string]interface{}{
					"role": "assistant", "content": "",
					"tool_calls": []interface{}{
						map[string]interface{}{
							"id": callId, "type": "function",
							"function": map[string]interface{}{"name": name, "arguments": args},
						},
					},
				})
			case "function_call_output":
				callId, _ := itemMap["call_id"].(string)
				// Preserve image/text output arrays for the shared tool converter.
				output := itemMap["output"]
				msgs = append(msgs, map[string]interface{}{
					"role": "tool", "tool_call_id": callId, "content": output,
				})
			}
		}
	}

	return msgs
}

// ConvertResponsesToolsToOpenAI wraps the Responses function definition in the
// Chat Completions schema required by Qoder. Already nested tools stay intact.
func ConvertResponsesToolsToOpenAI(raw interface{}) interface{} {
	tools, ok := raw.([]interface{})
	if !ok || len(tools) == 0 {
		return nil
	}
	converted := make([]interface{}, 0, len(tools))
	for _, tool := range tools {
		tm, ok := tool.(map[string]interface{})
		if !ok || tm["type"] != "function" || tm["function"] != nil {
			converted = append(converted, tool)
			continue
		}
		fn := map[string]interface{}{}
		for _, key := range []string{"name", "description", "parameters", "strict"} {
			if value, exists := tm[key]; exists {
				fn[key] = value
			}
		}
		converted = append(converted, map[string]interface{}{"type": "function", "function": fn})
	}
	return converted
}

func WriteCodexErr(w http.ResponseWriter, err error) {
	// 友好中文消息 + 分类类型（内容审核/瞬时/普通）
	errMsg, errType := FriendlyError(err)
	if errType == "" || errType == "qoder_error" {
		errType = "server_error"
	}
	body, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{"message": errMsg, "type": errType},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ErrorStatus(err))
	w.Write(body)
}
