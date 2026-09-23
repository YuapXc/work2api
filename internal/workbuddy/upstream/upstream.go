// Package upstream forwards requests to WorkBuddy/CodeBuddy's
// /v2/chat/completions. Ported from workbuddy_one/upstream.py.
//
// The upstream speaks standard OpenAI Chat Completions (native
// tools/tool_calls/SSE). Core job: inject auth headers, pass the body through,
// force stream=true, and strictly validate the stream. Non-streaming callers
// aggregate the SSE locally into a single response.
package upstream

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// passthroughBodyKeys is the whitelist of body fields forwarded upstream.
var passthroughBodyKeys = map[string]struct{}{
	"model": {}, "messages": {}, "tools": {}, "tool_choice": {}, "temperature": {},
	"max_tokens": {}, "max_completion_tokens": {}, "top_p": {}, "stream": {},
	"stream_options": {}, "stop": {}, "presence_penalty": {}, "frequency_penalty": {},
	"n": {}, "response_format": {}, "seed": {}, "user": {}, "reasoning_effort": {},
	"verbosity": {}, "reasoning_summary": {},
	"thinking": {}, // DeepSeek thinking switch
}

// UpstreamError carries a non-200 upstream status and its raw body.
type UpstreamError struct {
	StatusCode int
	Raw        []byte
}

func (e *UpstreamError) Error() string { return fmt.Sprintf("upstream HTTP %d", e.StatusCode) }

func invalidStream(message string) *UpstreamError {
	payload, _ := json.Marshal(map[string]any{
		"error": map[string]any{"message": message, "type": "upstream_error"},
	})
	return &UpstreamError{StatusCode: 502, Raw: payload}
}

// Client wraps a shared *http.Client tuned for upstream forwarding.
type Client struct {
	hc *http.Client
}

var (
	sharedOnce sync.Once
	shared     *Client
)

// Shared returns the process-wide upstream client (reuses TCP/TLS conns).
func Shared() *Client {
	sharedOnce.Do(func() {
		transport := &http.Transport{
			// short connect timeout: fail fast when upstream is unreachable
			// instead of holding a connection/goroutine.
			DialContext: (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:        50,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 15 * time.Second,
			// no ProxyFromEnvironment: mirrors trust_env=False, avoids picking
			// up a broken HTTP_PROXY.
			Proxy: nil,
		}
		shared = &Client{hc: &http.Client{Transport: transport}}
	})
	return shared
}

// BuildUpstreamBody constructs the upstream body: whitelist filter + force
// streaming.
func BuildUpstreamBody(payload map[string]any) map[string]any {
	body := make(map[string]any, len(payload))
	for k := range passthroughBodyKeys {
		if v, ok := payload[k]; ok {
			body[k] = v
		}
	}
	if _, ok := body["model"]; !ok {
		body["model"] = "auto"
	}
	body["stream"] = true
	if _, ok := body["stream_options"]; !ok {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	return body
}

// MergeUsage merges chunked usage; later chunks only override fields they
// explicitly provide.
func MergeUsage(current, incoming map[string]any) map[string]any {
	if incoming == nil {
		return current
	}
	result := map[string]any{}
	for k, v := range current {
		result[k] = v
	}
	for key, value := range incoming {
		if value == nil {
			continue
		}
		if sub, ok := value.(map[string]any); ok {
			cur, _ := result[key].(map[string]any)
			result[key] = MergeUsage(cur, sub)
		} else {
			result[key] = value
		}
	}
	return result
}

// LineFunc receives each raw SSE data line (including "data: [DONE]").
type LineFunc func(line string) error

// StreamUpstream forwards the upstream SSE stream, invoking yield for each raw
// data line. Strict validation mirrors the Python impl: a stream missing a
// finish reason or lacking any content/tool_calls is treated as invalid.
func (c *Client) StreamUpstream(ctx context.Context, headers map[string]string, body map[string]any, url string, yield LineFunc) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		raw := readAll(resp.Body)
		return &UpstreamError{StatusCode: resp.StatusCode, Raw: raw}
	}

	// Upstream sometimes answers a stream request with a single JSON object.
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json") {
		return c.handleJSONResponse(resp, yield)
	}

	return streamSSE(resp, yield)
}

// handleJSONResponse deals with a non-streaming JSON body returned for a stream
// request, normalizing it into two SSE lines.
func (c *Client) handleJSONResponse(resp *http.Response, yield LineFunc) error {
	raw := readAll(resp.Body)
	var chunk map[string]any
	if err := json.Unmarshal(stripBOM(raw), &chunk); err != nil {
		return invalidStream("上游返回了无效 JSON，无法读取模型响应")
	}
	choicesAny, _ := chunk["choices"].([]any)
	if len(choicesAny) == 0 {
		return &UpstreamError{StatusCode: 502, Raw: raw}
	}
	for _, ch := range choicesAny {
		choice, ok := ch.(map[string]any)
		if !ok {
			continue
		}
		if _, hasDelta := choice["delta"]; !hasDelta {
			if msg, ok := choice["message"].(map[string]any); ok {
				choice["delta"] = msg
			}
		}
		if empty(choice["finish_reason"]) {
			if delta, ok := choice["delta"].(map[string]any); ok {
				if _, hasTC := delta["tool_calls"]; hasTC {
					choice["finish_reason"] = "tool_calls"
				}
			}
		}
	}
	if !anyChoice(choicesAny, func(c map[string]any) bool { return !empty(c["finish_reason"]) }) {
		return invalidStream("上游 JSON 响应缺少结束原因，结果可能不完整")
	}
	if !anyChoice(choicesAny, func(c map[string]any) bool {
		delta, _ := c["delta"].(map[string]any)
		fr, _ := c["finish_reason"].(string)
		return (delta != nil && (!empty(delta["content"]) || !empty(delta["tool_calls"]))) ||
			fr == "length" || fr == "content_filter"
	}) {
		return invalidStream("上游 JSON 响应没有正文或工具调用")
	}
	out, _ := json.Marshal(chunk)
	if err := yield("data: " + string(out)); err != nil {
		return err
	}
	return yield("data: [DONE]")
}

// streamSSE consumes and validates a real SSE stream.
func streamSSE(resp *http.Response, yield LineFunc) error {
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	finished := false
	done := false
	sawToolCalls := false
	sawContent := false
	finishReason := ""

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(line[5:])
		if data == "[DONE]" {
			if !finished && sawToolCalls {
				if err := yield(`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`); err != nil {
					return err
				}
				finished = true
			}
			if !finished {
				return invalidStream("上游流缺少结束原因，结果可能不完整")
			}
			if !(sawContent || sawToolCalls || finishReason == "length" || finishReason == "content_filter") {
				return invalidStream("上游流没有正文或工具调用")
			}
			done = true
			if err := yield(line); err != nil {
				return err
			}
			break
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return invalidStream("上游返回了无效 SSE 数据")
		}
		if _, isErr := chunk["error"]; isErr {
			return &UpstreamError{StatusCode: 502, Raw: []byte(data)}
		}
		choices, _ := chunk["choices"].([]any)
		if !codeOK(chunk["code"]) && len(choices) == 0 {
			return &UpstreamError{StatusCode: 502, Raw: []byte(data)}
		}
		for _, ch := range choices {
			choice, ok := ch.(map[string]any)
			if !ok {
				continue
			}
			if !empty(choice["finish_reason"]) {
				finished = true
				if fr, ok := choice["finish_reason"].(string); ok {
					finishReason = fr
				}
			}
			if delta, ok := choice["delta"].(map[string]any); ok {
				if !empty(delta["content"]) {
					sawContent = true
				}
				if !empty(delta["tool_calls"]) {
					sawToolCalls = true
				}
			}
		}
		if err := yield(line); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !finished {
		return invalidStream("上游流提前结束，未收到结束原因")
	}
	if !(sawContent || sawToolCalls || finishReason == "length" || finishReason == "content_filter") {
		return invalidStream("上游流没有正文或工具调用")
	}
	if !done {
		return yield("data: [DONE]")
	}
	return nil
}

// CollectStream aggregates a standard OpenAI SSE stream into a single
// chat.completion object. producer drives the stream via a LineFunc.
func CollectStream(fallbackModel string, produce func(yield LineFunc) error) (map[string]any, error) {
	var contentParts, reasoningParts strings.Builder
	toolCalls := map[int]map[string]any{}
	var order []int
	model := ""
	finishReason := ""
	var usage map[string]any

	err := produce(func(line string) error {
		if !strings.HasPrefix(line, "data:") {
			return nil
		}
		data := strings.TrimSpace(line[5:])
		if data == "[DONE]" {
			return errDone
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			return nil
		}
		if m, ok := chunk["model"].(string); ok && m != "" {
			model = m
		}
		if u, ok := chunk["usage"].(map[string]any); ok && u != nil {
			usage = MergeUsage(usage, u)
		}
		choices, _ := chunk["choices"].([]any)
		for _, ch := range choices {
			choice, ok := ch.(map[string]any)
			if !ok {
				continue
			}
			if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
				finishReason = fr
			}
			delta, _ := choice["delta"].(map[string]any)
			if delta == nil {
				continue
			}
			if s, ok := delta["content"].(string); ok {
				contentParts.WriteString(s)
			}
			if s, ok := delta["reasoning_content"].(string); ok {
				reasoningParts.WriteString(s)
			}
			tcs, _ := delta["tool_calls"].([]any)
			for _, t := range tcs {
				tc, ok := t.(map[string]any)
				if !ok {
					continue
				}
				idx := 0
				if n, ok := toInt(tc["index"]); ok {
					idx = n
				}
				slot, exists := toolCalls[idx]
				if !exists {
					slot = map[string]any{"index": idx, "id": nil, "type": "function",
						"function": map[string]any{"name": "", "arguments": ""}}
					toolCalls[idx] = slot
					order = append(order, idx)
				}
				if id, ok := tc["id"].(string); ok && id != "" {
					slot["id"] = id
				}
				fn, _ := tc["function"].(map[string]any)
				sfn := slot["function"].(map[string]any)
				if fn != nil {
					if name, ok := fn["name"].(string); ok && name != "" {
						sfn["name"] = name
					}
					if args, ok := fn["arguments"].(string); ok && args != "" {
						sfn["arguments"] = sfn["arguments"].(string) + args
					}
				}
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errDone) {
		return nil, err
	}
	if finishReason == "" {
		return nil, invalidStream("上游流缺少结束原因，结果可能不完整")
	}

	message := map[string]any{"role": "assistant", "content": contentParts.String()}
	if reasoningParts.Len() > 0 {
		message["reasoning_content"] = reasoningParts.String()
	}
	if len(order) > 0 {
		sortInts(order)
		calls := make([]any, 0, len(order))
		for _, idx := range order {
			calls = append(calls, toolCalls[idx])
		}
		message["tool_calls"] = calls
	}
	if model == "" {
		model = fallbackModel
	}
	result := map[string]any{
		"id":      "chatcmpl-workbuddy",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finishReason}},
	}
	if usage != nil {
		result["usage"] = usage
	}
	return result, nil
}

var errDone = errors.New("sse done")

// --- helpers ---

func empty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	default:
		return false
	}
}

func anyChoice(choices []any, pred func(map[string]any) bool) bool {
	for _, ch := range choices {
		if c, ok := ch.(map[string]any); ok && pred(c) {
			return true
		}
	}
	return false
}

// codeOK reports whether an SSE chunk's "code" field is an OK sentinel
// (None/0/"0"/200/"200").
func codeOK(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case float64:
		return x == 0 || x == 200
	case int:
		return x == 0 || x == 200
	case string:
		return x == "0" || x == "200"
	default:
		return false
	}
}

func readAll(r interface{ Read([]byte) (int, error) }) []byte {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return buf
}

func stripBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	default:
		return 0, false
	}
}

func sortInts(s []int) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
