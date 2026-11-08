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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"work2api/internal/streamwatch"
	"work2api/internal/tokenusage"
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
	HTTPStatus int // Original transport status; StatusCode may describe an SSE business failure.
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

// NewClient uses the supplied reusable HTTP client for provider transports.
func NewClient(client *http.Client) *Client {
	if client == nil {
		return Shared()
	}
	return &Client{hc: client}
}

// Shared returns the process-wide upstream client (reuses TCP/TLS conns).
func Shared() *Client {
	sharedOnce.Do(func() {
		transport := &http.Transport{
			// short connect timeout: fail fast when upstream is unreachable
			// instead of holding a connection/goroutine.
			DialContext:         (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:   true,
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

// LineFunc receives each raw SSE data line (including "data: [DONE]").
type LineFunc func(line string) error

// StreamUpstream forwards the upstream SSE stream, invoking yield for each raw
// data line. Strict validation mirrors the Python impl: a stream missing a
// finish reason or lacking any content/tool_calls is treated as invalid.
func (c *Client) StreamUpstream(ctx context.Context, headers map[string]string, body map[string]any, url string, yield LineFunc) error {
	prepared, err := PrepareStreamBody(body)
	if err != nil {
		return err
	}
	return c.StreamPrepared(ctx, headers, prepared, url, yield)
}

// PreparedStreamBody is owned by one dispatch and may be reused serially on authorization renewal.
type PreparedStreamBody struct {
	payload   []byte
	toolNames map[string]string
	gzipWire  []byte
}

func PrepareStreamBody(body map[string]any) (*PreparedStreamBody, error) {
	if err := ValidateChoiceCount(body); err != nil {
		return nil, err
	}
	body, toolNames, err := prepareToolNames(body)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &PreparedStreamBody{payload: payload, toolNames: toolNames}, nil
}

func (c *Client) StreamPrepared(ctx context.Context, headers map[string]string, body *PreparedStreamBody, url string, yield LineFunc) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body.payload))
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	wire, err := body.wire(ctx, req.Header)
	if err != nil {
		return err
	}
	if len(wire) != len(body.payload) {
		req.Body = io.NopCloser(bytes.NewReader(wire))
		req.ContentLength = int64(len(wire))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(wire)), nil }
	}
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

	// Upstream sometimes answers a stream request with a single JSON object.
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json") {
		return c.handleJSONResponse(resp, yield, body.toolNames)
	}

	return streamSSE(resp, yield, body.toolNames)
}

func (b *PreparedStreamBody) wire(ctx context.Context, headers http.Header) ([]byte, error) {
	if !canGzipRequest(headers, b.payload) {
		return b.payload, ctx.Err()
	}
	if b.gzipWire == nil {
		wire, err := encodeRequestBody(ctx, headers, b.payload)
		if err != nil {
			return nil, err
		}
		b.gzipWire = wire
	}
	if len(b.gzipWire) != len(b.payload) {
		headers.Set("Content-Encoding", "gzip")
	}
	return b.gzipWire, ctx.Err()
}

// handleJSONResponse deals with a non-streaming JSON body returned for a stream
// request, normalizing it into two SSE lines.
func (c *Client) handleJSONResponse(resp *http.Response, yield LineFunc, names map[string]string) error {
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
	restore := toolNameRestorer{reverse: names}
	restore.restore(chunk)
	if len(restore.pending) > 0 {
		return invalidStream("上游工具名称未完整返回")
	}
	out, _ := json.Marshal(chunk)
	if err := yield("data: " + string(out)); err != nil {
		return err
	}
	return yield("data: [DONE]")
}

// streamSSE consumes and validates a real SSE stream.
func streamSSE(resp *http.Response, yield LineFunc, names ...map[string]string) error {
	watch := streamwatch.NewWatch(resp.Body, resp.Request.Context(), 0, 0)
	defer watch.Close()
	scanner := bufio.NewScanner(watch.Reader(resp.Body))
	scanner.Split(scanEventLine)
	scanner.Buffer(make([]byte, 0, 64*1024), streamwatch.DefaultMaxLineBytes)
	state := completionState{}
	var event strings.Builder
	var terminal []string
	terminalBytes := 0
	done := false
	var restore toolNameRestorer
	if len(names) > 0 {
		restore.reverse = names[0]
	}
	process := func() error {
		if event.Len() == 0 {
			return nil
		}
		data := strings.TrimSuffix(event.String(), "\n")
		event.Reset()
		if strings.TrimSpace(data) == "[DONE]" {
			if len(restore.pending) > 0 {
				return invalidStream("上游工具名称未完整返回")
			}
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
		renamed := restore.restore(chunk)
		if err := state.observe(chunk); err != nil {
			return err
		}
		if streamwatch.ChatGeneration(chunk) {
			streamwatch.Generation(resp.Request.Context())
		}
		// Keep ordinary single-line payloads; only multiline SSE needs folding.
		line := "data: " + data
		if renamed || strings.Contains(data, "\n") {
			raw, _ := json.Marshal(chunk)
			line = "data: " + string(raw)
		}
		RememberFrame(resp.Request.Context(), line, chunk)
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
	var contentParts, reasoningParts, refusalParts, signatureParts strings.Builder
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
			usage = tokenusage.Merge(usage, u)
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
			if s, ok := delta["reasoning_signature"].(string); ok {
				signatureParts.WriteString(s)
			}
			if s, ok := delta["reasoning_content"].(string); ok {
				reasoningParts.WriteString(s)
			}
			if s, ok := delta["refusal"].(string); ok {
				refusalParts.WriteString(s)
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errDone) {
		return map[string]any{"usage": usage}, err
	}
	if err := state.complete(false); err != nil {
		return map[string]any{"usage": usage}, err
	}

	message := map[string]any{"role": "assistant", "content": contentParts.String()}
	if refusalParts.Len() > 0 {
		message["refusal"] = refusalParts.String()
	}
	if reasoningParts.Len() > 0 {
		message["reasoning_content"] = reasoningParts.String()
	}
	if signatureParts.Len() > 0 {
		message["reasoning_signature"] = signatureParts.String()
	}
	if len(state.tools) > 0 {
		order := make([]int, 0, len(state.tools))
		for idx := range state.tools {
			order = append(order, idx)
		}
		sortInts(order)
		calls := make([]any, 0, len(order))
		for _, idx := range order {
			slot := state.tools[idx]
			var id any
			if slot.id != "" {
				id = slot.id
			}
			calls = append(calls, map[string]any{"index": idx, "id": id, "type": "function",
				"function": map[string]any{"name": slot.name, "arguments": slot.args.String()}})
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
