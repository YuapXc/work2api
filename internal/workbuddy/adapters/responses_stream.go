package adapters

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"work2api/internal/workbuddy/upstream"
)

// ResponsesStreamConverter converts a Chat SSE stream into a Responses API
// semantic event stream.
type ResponsesStreamConverter struct {
	respID    string
	msgID     string
	model     string
	createdAt int64

	emittedCreated     bool
	emittedMsgItem     bool
	emittedContentPart bool

	content   strings.Builder
	toolCalls map[int]*respToolCall
	toolOrder []int

	finishReason string
	usage        map[string]any
	errObj       map[string]any
}

type respToolCall struct {
	id        string
	name      string
	args      strings.Builder
	fcID      string
	outputIdx int
	emitted   bool
}

// NewResponsesStreamConverter creates a converter for the given model.
func NewResponsesStreamConverter(model string) *ResponsesStreamConverter {
	if model == "" {
		model = "unknown"
	}
	return &ResponsesStreamConverter{
		respID:    randID("resp_"),
		msgID:     randID("msg_"),
		model:     model,
		createdAt: time.Now().Unix(),
		toolCalls: map[int]*respToolCall{},
	}
}

// FeedLine processes one SSE line and returns Responses event text.
func (c *ResponsesStreamConverter) FeedLine(line string) string {
	line = strings.TrimSpace(line)
	if line == "" || !strings.HasPrefix(line, "data:") {
		return ""
	}
	data := strings.TrimSpace(line[5:])
	if data == "[DONE]" {
		return ""
	}
	var chunk map[string]any
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return ""
	}
	return c.processChunk(chunk)
}

// Finish emits closing events (done + completed).
func (c *ResponsesStreamConverter) Finish() string {
	var events strings.Builder
	if c.emittedContentPart {
		events.WriteString(c.evt("response.output_text.done", map[string]any{
			"output_index": 0, "content_index": 0, "text": c.content.String(),
		}))
		events.WriteString(c.evt("response.content_part.done", map[string]any{
			"output_index": 0, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": c.content.String(), "annotations": []any{}},
		}))
	}
	if c.emittedMsgItem {
		events.WriteString(c.evt("response.output_item.done", map[string]any{
			"output_index": 0, "item": c.msgItem("completed", false),
		}))
	}
	for _, idx := range sortedKeys(c.toolOrder) {
		tc := c.toolCalls[idx]
		if tc.emitted {
			events.WriteString(c.evt("response.function_call_arguments.done", map[string]any{
				"output_index": tc.outputIdx, "arguments": tc.args.String(),
			}))
			events.WriteString(c.evt("response.output_item.done", map[string]any{
				"output_index": tc.outputIdx, "item": c.fcItem(tc, "completed"),
			}))
		}
	}
	events.WriteString(c.evt("response.completed", map[string]any{"response": c.responseObj("completed")}))
	return events.String()
}

// GetNonstreamResponse returns the full non-streaming Response object.
func (c *ResponsesStreamConverter) GetNonstreamResponse() map[string]any {
	return c.responseObj("completed")
}

// Fail ends the stream as failed per the Responses protocol.
func (c *ResponsesStreamConverter) Fail(message string, code int) string {
	c.errObj = map[string]any{"type": "upstream_error", "code": itoa(code), "message": message}
	return c.evt("response.failed", map[string]any{"response": c.responseObj("failed")})
}

func (c *ResponsesStreamConverter) processChunk(chunk map[string]any) string {
	var events strings.Builder

	if m, ok := chunk["model"].(string); ok && m != "" {
		c.model = m
	}
	if !c.emittedCreated {
		resp := c.responseObj("in_progress")
		events.WriteString(c.evt("response.created", map[string]any{"response": resp}))
		events.WriteString(c.evt("response.in_progress", map[string]any{"response": resp}))
		c.emittedCreated = true
	}
	if u, ok := chunk["usage"].(map[string]any); ok && u != nil {
		c.usage = upstream.MergeUsage(c.usage, u)
	}

	choices, _ := chunk["choices"].([]any)
	for _, ch := range choices {
		choice, ok := ch.(map[string]any)
		if !ok {
			continue
		}
		delta, _ := choice["delta"].(map[string]any)
		finish, _ := choice["finish_reason"].(string)

		if delta != nil {
			if content, ok := delta["content"].(string); ok && content != "" {
				if !c.emittedMsgItem {
					events.WriteString(c.evt("response.output_item.added", map[string]any{
						"output_index": 0, "item": c.msgItem("in_progress", true),
					}))
					c.emittedMsgItem = true
				}
				if !c.emittedContentPart {
					events.WriteString(c.evt("response.content_part.added", map[string]any{
						"output_index": 0, "content_index": 0,
						"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
					}))
					c.emittedContentPart = true
				}
				c.content.WriteString(content)
				events.WriteString(c.evt("response.output_text.delta", map[string]any{
					"output_index": 0, "content_index": 0, "delta": content,
				}))
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
				slot, exists := c.toolCalls[idx]
				if !exists {
					base := 0
					if c.emittedMsgItem || c.content.Len() > 0 {
						base = 1
					}
					slot = &respToolCall{fcID: randID("fc_"), outputIdx: base + len(c.toolCalls)}
					c.toolCalls[idx] = slot
					c.toolOrder = append(c.toolOrder, idx)
				}
				if id, ok := tc["id"].(string); ok && id != "" {
					slot.id = id
				}
				fn, _ := tc["function"].(map[string]any)
				if fn != nil {
					if name, ok := fn["name"].(string); ok && name != "" {
						slot.name = name
					}
				}
				if !slot.emitted {
					events.WriteString(c.evt("response.output_item.added", map[string]any{
						"output_index": slot.outputIdx, "item": c.fcItem(slot, "in_progress"),
					}))
					slot.emitted = true
				}
				if fn != nil {
					if args, ok := fn["arguments"].(string); ok && args != "" {
						slot.args.WriteString(args)
						events.WriteString(c.evt("response.function_call_arguments.delta", map[string]any{
							"output_index": slot.outputIdx, "delta": args,
						}))
					}
				}
			}
		}
		if finish != "" {
			c.finishReason = finish
		}
	}
	return events.String()
}

func (c *ResponsesStreamConverter) evt(eventType string, data map[string]any) string {
	payload := map[string]any{"type": eventType}
	for k, v := range data {
		payload[k] = v
	}
	b, _ := json.Marshal(payload)
	return "data: " + string(b) + "\n\n"
}

func (c *ResponsesStreamConverter) msgItem(status string, empty bool) map[string]any {
	var content []any
	if !empty {
		content = []any{map[string]any{"type": "output_text", "text": c.content.String(), "annotations": []any{}}}
	} else {
		content = []any{}
	}
	return map[string]any{
		"type":    "message",
		"id":      c.msgID,
		"status":  status,
		"role":    "assistant",
		"content": content,
	}
}

func (c *ResponsesStreamConverter) fcItem(tc *respToolCall, status string) map[string]any {
	return map[string]any{
		"type":      "function_call",
		"id":        tc.fcID,
		"call_id":   tc.id,
		"name":      tc.name,
		"arguments": tc.args.String(),
		"status":    status,
	}
}

func (c *ResponsesStreamConverter) responseObj(status string) map[string]any {
	var output []any
	if c.emittedMsgItem || c.content.Len() > 0 {
		output = append(output, c.msgItem(status, false))
	}
	for _, idx := range sortedKeys(c.toolOrder) {
		tc := c.toolCalls[idx]
		if tc.emitted {
			output = append(output, c.fcItem(tc, status))
		}
	}
	var usage any
	if c.usage != nil {
		usage = map[string]any{
			"input_tokens":          intOrAny(c.usage, "prompt_tokens", "input_tokens"),
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
			"output_tokens":         intOrAny(c.usage, "completion_tokens", "output_tokens"),
			"output_tokens_details": map[string]any{"reasoning_tokens": 0},
			"total_tokens":          intOr(c.usage, "total_tokens"),
		}
	}
	response := map[string]any{
		"id":                   c.respID,
		"object":               "response",
		"created_at":           c.createdAt,
		"status":               status,
		"model":                c.model,
		"output":               output,
		"parallel_tool_calls":  true,
		"usage":                usage,
	}
	if c.errObj != nil {
		response["error"] = c.errObj
	}
	return response
}

// ToolsSummary returns a compact tool-call summary for usage logging.
func (c *ResponsesStreamConverter) ToolsSummary() string {
	var parts strings.Builder
	for _, idx := range sortedKeys(c.toolOrder) {
		tc := c.toolCalls[idx]
		name := tc.name
		if name == "" {
			name = "?"
		}
		parts.WriteString("<tool_call:" + name + " " + tc.args.String() + ">")
	}
	return parts.String()
}

// TextContent returns the accumulated assistant text (for usage logging).
func (c *ResponsesStreamConverter) TextContent() string { return c.content.String() }

// Usage returns the merged usage map (may be nil).
func (c *ResponsesStreamConverter) Usage() map[string]any { return c.usage }

func sortedKeys(order []int) []int {
	out := make([]int, len(order))
	copy(out, order)
	sort.Ints(out)
	return out
}

func intOrAny(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if n, ok := toInt(m[k]); ok {
			return n
		}
	}
	return 0
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [12]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
