package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"work2api/internal/core/protocol"
	"work2api/internal/streamwatch"
)

// NativeEvent is internal metadata, removed before returning Chat responses.
const NativeEvent = "_workbuddy_messages_event"

type messagesWriter struct {
	header http.Header
	buffer strings.Builder
	yield  LineFunc
	fault  *UpstreamError
}

func (w *messagesWriter) Header() http.Header { return w.header }
func (w *messagesWriter) WriteHeader(int)     {}
func (w *messagesWriter) Flush()              {}
func (w *messagesWriter) Write(p []byte) (int, error) {
	if w.fault != nil {
		return len(p), nil
	}
	w.buffer.Write(p)
	for {
		value := w.buffer.String()
		i := strings.Index(value, "\n\n")
		if i < 0 {
			break
		}
		frame, tail := value[:i], value[i+2:]
		w.buffer.Reset()
		w.buffer.WriteString(tail)
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data:") {
				if err := w.yield(line); err != nil {
					return 0, err
				}
			}
		}
	}
	return len(p), nil
}

func (c *Client) StreamMessages(ctx context.Context, headers map[string]string, body map[string]any, endpoint string, yield LineFunc) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := c.hc.Do(req)
	streamwatch.AttemptHTTP(ctx, resp)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, err := streamwatch.ReadBody(ctx, resp.Body, 1<<20)
		if err != nil {
			return err
		}
		return (&UpstreamError{StatusCode: resp.StatusCode, HTTPStatus: resp.StatusCode, Raw: raw, Header: resp.Header}).Normalized()
	}
	w := &messagesWriter{header: make(http.Header), yield: yield}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json") {
		raw, err := streamwatch.ReadBody(ctx, resp.Body, 0)
		if err != nil {
			return err
		}
		var message map[string]any
		if json.Unmarshal(raw, &message) != nil {
			return invalidStream("上游 Messages JSON 无效")
		}
		if message["error"] != nil || ParseBusinessFault(raw).Code != "" {
			return (&UpstreamError{StatusCode: 502, HTTPStatus: 200, Raw: raw}).Normalized()
		}
		if message["type"] != "message" || message["role"] != "assistant" || message["stop_reason"] == nil {
			return invalidStream("上游 Messages 响应未完整结束")
		}
		converted, err := protocol.ConvertResponse(protocol.Anthropic, protocol.Chat, raw)
		if err != nil {
			return err
		}
		var chat map[string]any
		json.Unmarshal(converted, &chat)
		choices, _ := chat["choices"].([]any)
		for _, v := range choices {
			ch, _ := v.(map[string]any)
			ch["delta"] = ch["message"]
			delete(ch, "message")
		}
		start := make(map[string]any, len(message))
		for k, v := range message {
			start[k] = v
		}
		start["content"] = []any{}
		start["stop_reason"] = nil
		events := []map[string]any{{"type": "message_start", "message": start}}
		if blocks, ok := message["content"].([]any); ok {
			for i, b := range blocks {
				events = append(events, map[string]any{"type": "content_block_start", "index": i, "content_block": b}, map[string]any{"type": "content_block_stop", "index": i})
			}
		}
		events = append(events, map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": message["stop_reason"], "stop_sequence": message["stop_sequence"]}, "usage": message["usage"]}, map[string]any{"type": "message_stop"})
		for _, event := range events {
			if err := yieldNativeEvent(yield, event); err != nil {
				return err
			}
		}
		encoded, _ := json.Marshal(chat)
		if err := yield("data: " + string(encoded)); err != nil {
			return err
		}
		return yield("data: [DONE]")
	}
	started, finished := false, false
	blocks := map[int]string{}
	open := map[int]bool{}
	arguments := map[int]*strings.Builder{}
	watch := streamwatch.NewWatch(resp.Body, ctx, 0, 0)
	defer watch.Close()
	model, _ := body["model"].(string)
	_, _, err = protocol.TranscodeStreamWithRewrite(ctx, w, watch.Reader(resp.Body), protocol.Anthropic, protocol.Chat, model, func(event map[string]any) error {
		if event == nil {
			return nil
		}
		if event["type"] == "error" || event["error"] != nil {
			raw, _ := json.Marshal(event)
			w.fault = (&UpstreamError{StatusCode: 502, HTTPStatus: 200, Raw: raw}).Normalized()
			return w.fault
		}
		if event["type"] == "ping" {
			return nil
		}
		fail := func() error {
			w.fault = invalidStream("上游 Messages 事件顺序或结束状态无效")
			return w.fault
		}
		if event["type"] == "message_start" {
			message, ok := event["message"].(map[string]any)
			if started || !ok || message["role"] != "assistant" {
				return fail()
			}
			started = true
		} else if !started {
			return fail()
		}
		switch event["type"] {
		case "content_block_start":
			n, ok := event["index"].(float64)
			if !ok || n != float64(len(blocks)) {
				return fail()
			}
			i := int(n)
			block, ok := event["content_block"].(map[string]any)
			if !ok {
				return fail()
			}
			kind, _ := block["type"].(string)
			if kind == "" {
				return fail()
			}
			blocks[i], open[i] = kind, true
		case "content_block_delta", "content_block_stop":
			n, ok := event["index"].(float64)
			i := int(n)
			if !ok || float64(i) != n || !open[i] {
				return fail()
			}
			if event["type"] == "content_block_stop" {
				if args := arguments[i]; args != nil {
					var value map[string]any
					if json.Unmarshal([]byte(args.String()), &value) != nil || value == nil {
						return fail()
					}
				}
				delete(open, i)
			} else {
				delta, ok := event["delta"].(map[string]any)
				if !ok {
					return fail()
				}
				if delta["type"] == "input_json_delta" {
					if blocks[i] != "tool_use" && blocks[i] != "server_tool_use" {
						return fail()
					}
					if arguments[i] == nil {
						arguments[i] = &strings.Builder{}
					}
					part, ok := delta["partial_json"].(string)
					if !ok {
						return fail()
					}
					arguments[i].WriteString(part)
				}
			}
		}
		if event["type"] == "message_delta" {
			delta, _ := event["delta"].(map[string]any)
			stop, _ := delta["stop_reason"].(string)
			finished = finished || stop != ""
			if finished && len(open) > 0 {
				return fail()
			}
		}
		if event["type"] == "message_stop" && !finished {
			return fail()
		}
		return yieldNativeEvent(yield, event)
	})
	if w.fault != nil {
		return w.fault
	}
	if watch.Err() != nil {
		return watch.Err()
	}
	return err
}

func yieldNativeEvent(yield LineFunc, event map[string]any) error {
	chunk := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}}}, NativeEvent: event}
	raw, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	return yield("data: " + string(raw))
}
