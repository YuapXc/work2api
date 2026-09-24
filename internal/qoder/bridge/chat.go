package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"work2api/internal/qoder/cosy"
	"work2api/internal/qoder/logger"
)

// ServeResult carries the aggregated outcome of a single Serve* call so the
// unified runtime can build a provider.UsageReport. The client response has
// already been written to w by the time this returns.
type ServeResult struct {
	InputTokens  int
	OutputTokens int
	Output       string
	Reasoning    string
}

func (b *Bridge) HandleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		WriteErr(w, err)
		return
	}
	var req map[string]interface{}
	if err := json.Unmarshal(body, &req); err != nil {
		WriteErr(w, err)
		return
	}
	_, _ = b.ServeChat(r.Context(), w, req)
}

// ServeChat runs the OpenAI chat/completions conversion + streaming against an
// already-parsed client body, writing the client response to w. The streaming
// and aggregation logic is byte-identical to the original HTTP handler.
func (b *Bridge) ServeChat(ctx context.Context, w http.ResponseWriter, req map[string]interface{}) (ServeResult, error) {
	startTime := time.Now()
	var result ServeResult

	reqID := cosy.NewUUID()[:8]

	stream, _ := req["stream"].(bool)
	model := StrValDefault(req, "model", "auto")
	incomingMsgs, _ := req["messages"].([]interface{})
	tools := req["tools"]
	toolsEnabled := tools != nil

	logger.Info("[Chat][%s] model=%s stream=%v tools=%v msgs=%d", reqID, model, stream, toolsEnabled, len(incomingMsgs))

	prompt := ExtractLatestUserPrompt(incomingMsgs)
	messages := BuildQoderMessages(b.templateMessages(), incomingMsgs, prompt, toolsEnabled)

	reqId := "chatcmpl-" + cosy.NewRequestID()
	created := cosy.UnixSec()

	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(200)
		flusher, _ := w.(http.Flusher)

		var toolCallBuf []interface{}
		var totalInputTokens, totalOutputTokens int
		var streamFull strings.Builder

		err := b.CallQoder(ctx, InferAgent(model), messages, model, tools, func(d Delta) {
			if d.Err != nil {
				logger.Error("[Chat][%s] upstream error in stream callback: %v", reqID, d.Err)
				return
			}
			if d.InputTokens > 0 || d.OutputTokens > 0 {
				totalInputTokens = d.InputTokens
				totalOutputTokens = d.OutputTokens
			}
			chunk := MakeChatChunk(reqId, created, model)
			choices := chunk["choices"].([]interface{})
			delta := choices[0].(map[string]interface{})["delta"].(map[string]interface{})
			if d.Content != "" {
				delta["role"] = "assistant"
				delta["content"] = d.Content
				streamFull.WriteString(d.Content)
			}
			if d.ToolCalls != nil {
				delta["tool_calls"] = d.ToolCalls
				toolCallBuf = append(toolCallBuf, d.ToolCalls...)
			}
			data, err := json.Marshal(chunk)
			if err != nil {
				logger.Error("[Chat][%s] marshal chunk failed: %v", reqID, err)
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			if flusher != nil {
				flusher.Flush()
			}
		})
		result.InputTokens = totalInputTokens
		result.OutputTokens = totalOutputTokens
		result.Output = streamFull.String()
		if err != nil {
			logger.Error("[Chat][%s] stream 请求失败: %v (耗时 %dms)", reqID, err, time.Since(startTime).Milliseconds())
			errMsg, errType := FriendlyError(err)
			errData, _ := json.Marshal(map[string]interface{}{
				"error": map[string]interface{}{"message": errMsg, "type": errType},
			})
			fmt.Fprintf(w, "data: %s\n\n", string(errData))
			if flusher != nil {
				flusher.Flush()
			}
			return result, err
		}
		finishReason := "stop"
		if len(toolCallBuf) > 0 {
			finishReason = "tool_calls"
		}
		done := MakeChatChunk(reqId, created, model)
		choices := done["choices"].([]interface{})
		ch := choices[0].(map[string]interface{})
		ch["finish_reason"] = finishReason
		ch["delta"] = map[string]interface{}{}
		if totalInputTokens > 0 || totalOutputTokens > 0 {
			done["usage"] = map[string]interface{}{
				"prompt_tokens":     totalInputTokens,
				"completion_tokens": totalOutputTokens,
				"total_tokens":      totalInputTokens + totalOutputTokens,
			}
		}
		data, _ := json.Marshal(done)
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", string(data))
		if flusher != nil {
			flusher.Flush()
		}
		logger.Info("[Chat][%s] stream 完成 finish=%s tool_calls=%d 耗时=%dms", reqID, finishReason, len(toolCallBuf), time.Since(startTime).Milliseconds())
		return result, nil
	} else {
		var full strings.Builder
		var toolCallBuf []interface{}
		var totalInputTokens, totalOutputTokens int
		err := b.CallQoder(ctx, InferAgent(model), messages, model, tools, func(d Delta) {
			if d.InputTokens > 0 || d.OutputTokens > 0 {
				totalInputTokens = d.InputTokens
				totalOutputTokens = d.OutputTokens
			}
			if d.Content != "" {
				full.WriteString(d.Content)
			}
			if d.ToolCalls != nil {
				toolCallBuf = append(toolCallBuf, d.ToolCalls...)
			}
		})
		result.InputTokens = totalInputTokens
		result.OutputTokens = totalOutputTokens
		result.Output = full.String()
		if err != nil {
			logger.Error("[Chat][%s] 请求失败: %v (耗时 %dms)", reqID, err, time.Since(startTime).Milliseconds())
			WriteErr(w, err)
			return result, err
		}
		finishReason := "stop"
		msg := map[string]interface{}{"role": "assistant", "content": full.String()}
		if len(toolCallBuf) > 0 {
			finishReason = "tool_calls"
			// Merge streamed tool-call fragments (all index 0, id+name on the
			// first chunk, arguments split across the rest) into whole calls.
			// Upstream qoder2api emits them raw here, which yields a fragmented
			// tool_calls array that OpenAI clients cannot parse — deviation from
			// verbatim to fix a real correctness bug (mirrors the claude path).
			merged := map[int]map[string]interface{}{}
			MergeToolCallChunks(merged, toolCallBuf)
			calls := SortedToolCalls(merged)
			out := make([]interface{}, len(calls))
			for i, c := range calls {
				out[i] = c
			}
			msg["tool_calls"] = out
			if full.Len() == 0 {
				msg["content"] = nil
			}
		}
		resp := map[string]interface{}{
			"id": reqId, "object": "chat.completion",
			"created": created, "model": model,
			"choices": []interface{}{
				map[string]interface{}{"index": 0, "message": msg, "finish_reason": finishReason},
			},
			"usage": map[string]interface{}{"prompt_tokens": totalInputTokens, "completion_tokens": totalOutputTokens, "total_tokens": totalInputTokens + totalOutputTokens},
		}
		logger.Info("[Chat][%s] 完成 finish=%s content_len=%d tool_calls=%d 耗时=%dms", reqID, finishReason, full.Len(), len(toolCallBuf), time.Since(startTime).Milliseconds())
		logger.Debug("[Chat][%s] 响应体: %s", reqID, func() string { d, _ := json.Marshal(resp); return string(d) }())
		WriteJSON(w, resp)
		return result, nil
	}
}

func ExtractLatestUserPrompt(msgs []interface{}) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		m, _ := msgs[i].(map[string]interface{})
		if m["role"] == "user" {
			return NormalizeMessageContent(m)
		}
	}
	return ""
}

func MakeChatChunk(id string, created int64, model string) map[string]interface{} {
	return map[string]interface{}{
		"id": id, "object": "chat.completion.chunk",
		"created": created, "model": model,
		"choices": []interface{}{
			map[string]interface{}{"index": 0, "delta": map[string]interface{}{}, "finish_reason": nil},
		},
	}
}

func (b *Bridge) templateMessages() []interface{} {
	if msgs, ok := b.templateBase["messages"].([]interface{}); ok {
		return msgs
	}
	return nil
}

func WriteJSON(w http.ResponseWriter, v interface{}) {
	data, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	w.Write(data)
}

func WriteErr(w http.ResponseWriter, err error) {
	// 结构化上游错误 → 友好中文消息 + 分类类型（内容审核/瞬时/普通）
	errMsg, errType := FriendlyError(err)
	if errType == "" {
		errType = "qoder_error"
	}
	body, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{"message": errMsg, "type": errType},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ErrorStatus(err))
	w.Write(body)
}
