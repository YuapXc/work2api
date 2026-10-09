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
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"work2api/internal/streamwatch"
)

// passthroughBodyKeys is the whitelist of body fields forwarded upstream.
var passthroughBodyKeys = map[string]struct{}{
	"model": {}, "messages": {}, "tools": {}, "tool_choice": {}, "temperature": {},
	"max_tokens": {}, "max_completion_tokens": {}, "top_p": {}, "stream": {},
	"stream_options": {}, "stop": {}, "presence_penalty": {}, "frequency_penalty": {},
	"n": {}, "response_format": {}, "seed": {}, "user": {}, "reasoning_effort": {},
	"verbosity": {}, "reasoning_summary": {},
	"thinking":            {}, // DeepSeek thinking switch
	"parallel_tool_calls": {},
}

// UpstreamError carries a non-200 upstream status and its raw body.
type UpstreamError struct {
	StatusCode int
	Raw        []byte
	// Header carries the upstream response headers (Retry-After 等) for error
	// classification. Nil for synthesized errors (invalid stream / bad JSON).
	Header http.Header
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
			DialContext:         (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			MaxIdleConns:        50,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 15 * time.Second,
			// 首字节（响应头）等待上限：上游"接了连接却迟迟不回"时快速失败，
			// 而不是把 goroutine 挂死等到客户端断开。只约束到首个响应头的等待，
			// 不影响已开始的流式响应体（长思考流不会被腰斩）。
			ResponseHeaderTimeout: 120 * time.Second,
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
	if err := ValidateChoiceCount(body); err != nil {
		return err
	}
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
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	streamwatch.AttemptHeaders(ctx, status)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		raw, err := streamwatch.ReadBody(ctx, resp.Body, 1<<20)
		if err != nil {
			return err
		}
		return &UpstreamError{StatusCode: resp.StatusCode, Raw: raw, Header: resp.Header}
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
	raw, err := streamwatch.ReadBody(resp.Request.Context(), resp.Body, 0)
	if err != nil {
		return err
	}
	var chunk map[string]any
	if err := json.Unmarshal(stripBOM(raw), &chunk); err != nil {
		return invalidStream("上游返回了无效 JSON，无法读取模型响应")
	}
	choices, _ := chunk["choices"].([]any)
	for _, v := range choices {
		choice, _ := v.(map[string]any)
		if choice == nil {
			continue
		}
		if _, exists := choice["delta"]; !exists {
			choice["delta"] = choice["message"]
		}
		if delta, ok := choice["delta"].(map[string]any); ok {
			if calls, ok := delta["tool_calls"].([]any); ok {
				for i, v := range calls {
					if call, ok := v.(map[string]any); ok {
						call["index"] = i
					}
				}
			}
		}
	}
	state := completionState{}
	if err := state.observe(chunk); err != nil {
		return err
	}
	if err := state.complete(true); err != nil {
		return err
	}
	if state.finish == "" {
		choices[0].(map[string]any)["finish_reason"] = "tool_calls"
	}
	out, _ := json.Marshal(chunk)
	if err := yield("data: " + string(out)); err != nil {
		return err
	}
	return yield("data: [DONE]")
}

// streamSSE consumes and validates a real SSE stream.
func streamSSE(resp *http.Response, yield LineFunc) error {
	watch := streamwatch.NewWatch(resp.Body, resp.Request.Context(), 0, 0)
	defer watch.Close()
	scanner := bufio.NewScanner(watch.Reader(resp.Body))
	scanner.Split(scanEventLine)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	state := completionState{}
	var event strings.Builder
	var terminal []string
	terminalBytes := 0
	done := false
	process := func() error {
		if event.Len() == 0 {
			return nil
		}
		data := strings.TrimSuffix(event.String(), "\n")
		event.Reset()
		if strings.TrimSpace(data) == "[DONE]" {
			if err := state.complete(true); err != nil {
				return err
			}
			if state.finish == "" {
				terminal = append(terminal, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`)
			}
			done = true
			return nil
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(data), &chunk) != nil {
			return invalidStream("上游返回了无效 SSE 数据")
		}
		if err := state.observe(chunk); err != nil {
			return err
		}
		raw, _ := json.Marshal(chunk) // Multi-line SSE data becomes one adapter-safe line.
		line := "data: " + string(raw)
		if state.finish != "" {
			terminalBytes += len(line)
			if terminalBytes > 8*1024*1024 {
				return invalidStream("上游结束事件过大")
			}
			terminal = append(terminal, line)
			return nil
		}
		return yield(line)
	}
	firstLine := true
	for scanner.Scan() {
		line := scanner.Text()
		if firstLine {
			line = strings.TrimPrefix(line, "\ufeff")
			firstLine = false
		}
		if line == "" {
			if err := process(); err != nil {
				return err
			}
			if done {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			if event.Len()+len(value)+1 > 8*1024*1024 {
				return invalidStream("上游 SSE 事件过大")
			}
			event.WriteString(value)
			event.WriteByte('\n')
		} else if line == "data" {
			event.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		if watch.Err() != nil {
			return watch.Err()
		}
		return err
	}
	// Compatibility: a complete terminal JSON event may precede EOF without a
	// final blank line. Never infer successful completion from partial text.
	if !done {
		if err := process(); err != nil {
			return err
		}
	}
	if err := state.complete(done); err != nil {
		return err
	}
	for _, line := range terminal {
		if err := yield(line); err != nil {
			return err
		}
	}
	return yield("data: [DONE]")
}

// SSE accepts LF, CRLF and CR, and joins data fields at the event boundary.
func scanEventLine(data []byte, atEOF bool) (int, []byte, error) {
	for i, c := range data {
		if c == '\n' {
			return i + 1, data[:i], nil
		}
		if c == '\r' {
			if i+1 == len(data) && !atEOF {
				return 0, nil, nil
			}
			advance := i + 1
			if advance < len(data) && data[advance] == '\n' {
				advance++
			}
			return advance, data[:i], nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
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
	state := completionState{}

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
			return invalidStream("上游返回了无效 SSE 数据")
		}
		if err := state.observe(chunk); err != nil {
			return err
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
	if err := state.complete(false); err != nil {
		return nil, err
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

func stripBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) || math.Trunc(x) != x || x < 0 || x > 1024 {
			return 0, false
		}
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
