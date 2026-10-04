package adapters

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"work2api/internal/tokenusage"
	"work2api/internal/workbuddy/upstream"
)

// ResponsesStreamConverter converts a Chat SSE stream into a Responses API
// semantic event stream.
type ResponsesStreamConverter struct {
	respID    string
	msgID     string
	model     string
	createdAt int64

	nonstream          bool
	emittedCreated     bool
	emittedMsgItem     bool
	emittedContentPart bool

	content   strings.Builder
	toolCalls map[int]*respToolCall
	toolOrder []int

	// thinking 追踪：上游 reasoning_content 增量 → Responses reasoning 项，
	// 按首次出现顺序分配稳定 ID/索引，以 summary text 形态输出。
	reasoning                strings.Builder
	reasoningID              string
	reasoningIdx, messageIdx int
	items                    []responseOutputItem
	sequence                 int

	finishReason string
	usage        map[string]any
	errObj       map[string]any
}

type responseOutputItem struct {
	kind string
	tool int
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
		respID:      randID("resp_"),
		msgID:       randID("msg_"),
		model:       model,
		createdAt:   time.Now().Unix(),
		toolCalls:   map[int]*respToolCall{},
		reasoningID: randID("rs_"), reasoningIdx: -1, messageIdx: -1,
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

// Finish emits closing events (done + completed/incomplete).
func (c *ResponsesStreamConverter) Finish() string {
	var events strings.Builder
	_ = c.FinishEvents(func(event map[string]any) error {
		b, _ := json.Marshal(event)
		events.WriteString("data: ")
		events.Write(b)
		events.WriteString("\n\n")
		return nil
	})
	return events.String()
}

// FinishEvents emits each full protocol event separately. Production callers
// encode directly to their writer, avoiding one large concatenated SSE string.
func (c *ResponsesStreamConverter) FinishEvents(send func(map[string]any) error) error {
	status := c.CompletionStatus()
	var err error
	emit := func(kind string, data map[string]any) {
		if err != nil {
			return
		}
		c.eventPayload(kind, data)
		err = send(data)
	}
	if c.reasoningIdx >= 0 {
		emit("response.reasoning_summary_text.done", map[string]any{"item_id": c.reasoningID, "output_index": c.reasoningIdx, "summary_index": 0, "text": c.reasoning.String()})
		emit("response.reasoning_summary_part.done", map[string]any{"item_id": c.reasoningID, "output_index": c.reasoningIdx, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": c.reasoning.String()}})
		emit("response.output_item.done", map[string]any{"output_index": c.reasoningIdx, "item": c.reasoningItem(status, false)})
	}
	if c.emittedContentPart {
		emit("response.output_text.done", map[string]any{
			"item_id": c.msgID, "output_index": c.messageIdx, "content_index": 0, "text": c.content.String(),
		})
		emit("response.content_part.done", map[string]any{
			"item_id": c.msgID, "output_index": c.messageIdx, "content_index": 0,
			"part": map[string]any{"type": "output_text", "text": c.content.String(), "annotations": []any{}},
		})
	}
	if c.emittedMsgItem {
		emit("response.output_item.done", map[string]any{
			"output_index": c.messageIdx, "item": c.msgItem(status, false),
		})
	}
	for _, idx := range sortedKeys(c.toolOrder) {
		tc := c.toolCalls[idx]
		if tc.emitted {
			emit("response.function_call_arguments.done", map[string]any{
				"item_id": tc.fcID, "output_index": tc.outputIdx, "arguments": tc.args.String(),
			})
			emit("response.output_item.done", map[string]any{
				"output_index": tc.outputIdx, "item": c.fcItem(tc, status),
			})
		}
	}
	emit("response."+status, map[string]any{"response": c.responseObj(status)})
	return err
}

// SetNonstream suppresses event serialization while preserving accumulated state.
func (c *ResponsesStreamConverter) SetNonstream() { c.nonstream = true }

// GetNonstreamResponse returns the full non-streaming Response object.
func (c *ResponsesStreamConverter) GetNonstreamResponse() map[string]any {
	return c.responseObj(c.CompletionStatus())
}

func completionStatus(reason string) string {
	if reason == "length" || reason == "content_filter" {
		return "incomplete"
	}
	return "completed"
}

func (c *ResponsesStreamConverter) CompletionStatus() string { return completionStatus(c.finishReason) }

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
			if reasoning, ok := delta["reasoning_content"].(string); ok && reasoning != "" {
				if c.reasoningIdx < 0 {
					c.reasoningIdx = len(c.items)
					c.items = append(c.items, responseOutputItem{kind: "reasoning"})
					events.WriteString(c.evt("response.output_item.added", map[string]any{"output_index": c.reasoningIdx, "item": c.reasoningItem("in_progress", true)}))
					events.WriteString(c.evt("response.reasoning_summary_part.added", map[string]any{"item_id": c.reasoningID, "output_index": c.reasoningIdx, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}}))
				}
				c.reasoning.WriteString(reasoning)
				events.WriteString(c.evt("response.reasoning_summary_text.delta", map[string]any{
					"item_id": c.reasoningID, "output_index": c.reasoningIdx, "summary_index": 0, "delta": reasoning,
				}))
			}
			if content, ok := delta["content"].(string); ok && content != "" {
				if !c.emittedMsgItem {
					c.messageIdx = len(c.items)
					c.items = append(c.items, responseOutputItem{kind: "message"})
					events.WriteString(c.evt("response.output_item.added", map[string]any{
						"output_index": c.messageIdx, "item": c.msgItem("in_progress", true),
					}))
					c.emittedMsgItem = true
				}
				if !c.emittedContentPart {
					events.WriteString(c.evt("response.content_part.added", map[string]any{
						"item_id": c.msgID, "output_index": c.messageIdx, "content_index": 0,
						"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
					}))
					c.emittedContentPart = true
				}
				c.content.WriteString(content)
				events.WriteString(c.evt("response.output_text.delta", map[string]any{
					"item_id": c.msgID, "output_index": c.messageIdx, "content_index": 0, "delta": content,
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
					slot = &respToolCall{fcID: randID("fc_"), id: randID("call_"), outputIdx: len(c.items)}
					c.items = append(c.items, responseOutputItem{kind: "function_call", tool: idx})
					c.toolCalls[idx] = slot
					c.toolOrder = append(c.toolOrder, idx)
				}
				if id, ok := tc["id"].(string); ok && id != "" && !slot.emitted {
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
							"item_id": slot.fcID, "output_index": slot.outputIdx, "delta": args,
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
	if c.nonstream {
		return ""
	}
	c.eventPayload(eventType, data)
	b, _ := json.Marshal(data)
	return "data: " + string(b) + "\n\n"
}

func (c *ResponsesStreamConverter) eventPayload(kind string, data map[string]any) {
	data["type"] = kind
	data["sequence_number"] = c.sequence
	c.sequence++
}

func (c *ResponsesStreamConverter) reasoningItem(status string, empty bool) map[string]any {
	summary := []any{}
	if !empty {
		summary = append(summary, map[string]any{"type": "summary_text", "text": c.reasoning.String()})
	}
	return map[string]any{"type": "reasoning", "id": c.reasoningID, "summary": summary, "status": status}
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
	output := []any{}
	for _, item := range c.items {
		switch item.kind {
		case "reasoning":
			output = append(output, c.reasoningItem(status, false))
		case "message":
			output = append(output, c.msgItem(status, false))
		case "function_call":
			output = append(output, c.fcItem(c.toolCalls[item.tool], status))
		}
	}
	var usage any
	if c.usage != nil {
		// cached_tokens 用上游真实值（未上报则省略该字段）——硬编码 0 会让
		// 客户端把每次请求都当成全量 miss。
		details := map[string]any{}
		outputDetails := map[string]any{}
		if reasoning := tokenusage.Reasoning(c.usage); reasoning != nil {
			outputDetails["reasoning_tokens"] = *reasoning
		}
		cached := upstreamCachedTokens(c.usage)
		if cached != nil {
			details["cached_tokens"] = *cached
		}
		inputTokens, outputTokens := tokenusage.Totals(c.usage)
		usageMap := map[string]any{
			"input_tokens":          inputTokens,
			"input_tokens_details":  details,
			"output_tokens":         outputTokens,
			"output_tokens_details": outputDetails,
			"total_tokens":          inputTokens + outputTokens,
		}
		if cached != nil {
			usageMap["cache_read_input_tokens"] = *cached
		}
		usage = usageMap
	}
	response := map[string]any{
		"id":                  c.respID,
		"object":              "response",
		"created_at":          c.createdAt,
		"status":              status,
		"model":               c.model,
		"output":              output,
		"parallel_tool_calls": true,
		"usage":               usage,
	}
	if c.errObj != nil {
		response["error"] = c.errObj
	}
	if status == "incomplete" {
		reason := "max_output_tokens"
		if c.finishReason == "content_filter" {
			reason = "content_filter"
		}
		response["incomplete_details"] = map[string]any{"reason": reason}
	}
	return response
}

// ToolsSummary returns a compact tool-call summary for usage logging.
func (c *ResponsesStreamConverter) ToolsSummary() string { return c.ToolsSummaryLimited(0) }

func (c *ResponsesStreamConverter) ToolsSummaryLimited(limit int) string {
	var parts strings.Builder
	for _, idx := range sortedKeys(c.toolOrder) {
		tc := c.toolCalls[idx]
		name := tc.name
		if name == "" {
			name = "?"
		}
		for _, part := range []string{"<tool_call:", name, " ", tc.args.String(), ">"} {
			if limit > 0 {
				remaining := limit - parts.Len()
				if remaining <= 0 {
					return parts.String()
				}
				if len(part) > remaining {
					part = part[:remaining]
				}
			}
			parts.WriteString(part)
		}
	}
	return parts.String()
}

// TextContent returns the accumulated assistant text (for usage logging).
func (c *ResponsesStreamConverter) TextContent() string { return c.content.String() }

// Reasoning returns the accumulated thinking text (for usage logging).
func (c *ResponsesStreamConverter) Reasoning() string { return c.reasoning.String() }

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

// upstreamCachedTokens returns only valid observations; absence stays unknown.
func upstreamCachedTokens(u map[string]any) *int {
	read, _ := tokenusage.ValidCache(u)
	return read
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
