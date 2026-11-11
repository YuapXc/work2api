package chatstream

import (
	"encoding/json"
	"maps"
	"sort"
	"strings"
	"time"

	"work2api/internal/core/protocol"
	"work2api/internal/identity"
	"work2api/internal/tokenusage"
)

// ResponsesStreamConverter converts a Chat SSE stream into a Responses API
// semantic event stream.
type ResponsesStreamConverter struct {
	toolIndexes      protocol.ToolCallIndexes
	nativeBlockKinds map[int]string
	nativeMessages   bool
	nativeThinking   map[int]*nativeThinkingItem
	respID           string
	msgID            string
	model            string
	createdAt        int64

	nonstream                                               bool
	emittedCreated                                          bool
	emittedMsgItem                                          bool
	emittedContentPart                                      bool
	emittedRefusalPart                                      bool
	nextContentIndex, textContentIndex, refusalContentIndex int
	refusal                                                 strings.Builder

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
		respID:      identity.RandomID("resp", 12),
		msgID:       identity.RandomID("msg", 12),
		model:       model,
		createdAt:   time.Now().Unix(),
		toolCalls:   map[int]*respToolCall{},
		reasoningID: identity.RandomID("rs", 12), reasoningIdx: -1, messageIdx: -1,
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
	return c.FeedChunk(chunk)
}

func (c *ResponsesStreamConverter) FeedChunk(chunk map[string]any) string {
	if chunk == nil {
		return ""
	}
	if c.nativeMessages {
		filtered := maps.Clone(chunk)
		if choices, ok := chunk["choices"].([]any); ok {
			next := make([]any, 0, len(choices))
			for _, raw := range choices {
				choice, _ := raw.(map[string]any)
				copy := maps.Clone(choice)
				copy["delta"] = map[string]any{}
				next = append(next, copy)
			}
			filtered["choices"] = next
		}
		return c.processChunk(filtered)
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
			"item_id": c.msgID, "output_index": c.messageIdx, "content_index": c.textContentIndex, "text": c.content.String(),
		})
		emit("response.content_part.done", map[string]any{
			"item_id": c.msgID, "output_index": c.messageIdx, "content_index": c.textContentIndex,
			"part": map[string]any{"type": "output_text", "text": c.content.String(), "annotations": []any{}},
		})
	}
	if c.emittedRefusalPart {
		emit("response.refusal.done", map[string]any{"item_id": c.msgID, "output_index": c.messageIdx, "content_index": c.refusalContentIndex, "refusal": c.refusal.String()})
		emit("response.content_part.done", map[string]any{"item_id": c.msgID, "output_index": c.messageIdx, "content_index": c.refusalContentIndex, "part": map[string]any{"type": "refusal", "refusal": c.refusal.String()}})
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
	c.toolIndexes.NormalizeChunk(chunk)
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
		c.usage = tokenusage.Merge(c.usage, u)
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
			if reasoning, ok := delta["reasoning_content"].(string); ok && reasoning != "" && !c.nativeMessages {
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
					c.textContentIndex = c.nextContentIndex
					c.nextContentIndex++
					events.WriteString(c.evt("response.content_part.added", map[string]any{
						"item_id": c.msgID, "output_index": c.messageIdx, "content_index": c.textContentIndex,
						"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
					}))
					c.emittedContentPart = true
				}
				c.content.WriteString(content)
				events.WriteString(c.evt("response.output_text.delta", map[string]any{
					"item_id": c.msgID, "output_index": c.messageIdx, "content_index": c.textContentIndex, "delta": content,
				}))
			}
			if refusal, ok := delta["refusal"].(string); ok && refusal != "" {
				if !c.emittedMsgItem {
					c.messageIdx = len(c.items)
					c.items = append(c.items, responseOutputItem{kind: "message"})
					events.WriteString(c.evt("response.output_item.added", map[string]any{"output_index": c.messageIdx, "item": c.msgItem("in_progress", true)}))
					c.emittedMsgItem = true
				}
				if !c.emittedRefusalPart {
					c.refusalContentIndex = c.nextContentIndex
					c.nextContentIndex++
					events.WriteString(c.evt("response.content_part.added", map[string]any{"item_id": c.msgID, "output_index": c.messageIdx, "content_index": c.refusalContentIndex, "part": map[string]any{"type": "refusal", "refusal": ""}}))
					c.emittedRefusalPart = true
				}
				c.refusal.WriteString(refusal)
				events.WriteString(c.evt("response.refusal.delta", map[string]any{"item_id": c.msgID, "output_index": c.messageIdx, "content_index": c.refusalContentIndex, "delta": refusal}))
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
					slot = &respToolCall{fcID: identity.RandomID("fc", 12), id: identity.RandomID("call", 12), outputIdx: len(c.items)}
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
	content := []any{}
	if !empty {
		if c.emittedContentPart {
			content = append(content, map[string]any{"type": "output_text", "text": c.content.String(), "annotations": []any{}})
		}
		if c.emittedRefusalPart {
			content = append(content, map[string]any{"type": "refusal", "refusal": c.refusal.String()})
		}
		if len(content) == 2 && c.refusalContentIndex < c.textContentIndex {
			content[0], content[1] = content[1], content[0]
		}
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
		case "native_reasoning":
			output = append(output, c.nativeThinking[item.tool].item(status, false))
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
		cached := upstreamCachedTokens(c.usage)
		if cached != nil {
			details["cached_tokens"] = *cached
		}
		inputTokens, outputTokens := tokenusage.Totals(c.usage)
		usageMap := map[string]any{
			"input_tokens":         inputTokens,
			"input_tokens_details": details,
			"output_tokens":        outputTokens,
			"total_tokens":         inputTokens + outputTokens,
		}
		// Codex treats this optional object as a complete structure: an empty
		// object fails to decode because reasoning_tokens is required. Preserve
		// unknown upstream usage by omitting the object rather than inventing 0.
		if reasoning := tokenusage.Reasoning(c.usage); reasoning != nil {
			usageMap["output_tokens_details"] = map[string]any{"reasoning_tokens": *reasoning}
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
func (c *ResponsesStreamConverter) TextContent() string {
	return c.content.String() + c.refusal.String()
}

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

type nativeThinkingItem struct {
	raw             map[string]any
	id              string
	output          int
	kind            string
	text, signature strings.Builder
	data            string
	closed          bool
}

func (n *nativeThinkingItem) item(status string, empty bool) map[string]any {
	summary := []any{}
	if !empty && n.text.Len() > 0 {
		summary = append(summary, map[string]any{"type": "summary_text", "text": n.text.String()})
	}
	item := map[string]any{"type": "reasoning", "id": n.id, "status": status, "summary": summary}
	if !empty {
		block := maps.Clone(n.raw)
		block["thinking"] = n.text.String()
		block["signature"] = n.signature.String()
		if n.kind == "redacted_thinking" {
			block = maps.Clone(n.raw)
			block["data"] = n.data
		}
		if sealed := protocol.SealAnthropicReasoning(block); sealed != "" {
			item["encrypted_content"] = sealed
		}
	}
	return item
}

// Native blocks preserve their boundaries; the accompanying Chat projection only supplies text/tools/usage.
func (c *ResponsesStreamConverter) FeedNativeMessagesEvent(event map[string]any) string {
	c.nativeMessages = true
	if message, ok := event["message"].(map[string]any); ok {
		if model, ok := message["model"].(string); ok {
			c.model = model
		}
	}
	var out strings.Builder
	out.WriteString(c.processChunk(map[string]any{}))
	index := 0
	switch n := event["index"].(type) {
	case float64:
		index = int(n)
	case int:
		index = n
	}
	switch event["type"] {
	case "content_block_start":
		block, _ := event["content_block"].(map[string]any)
		kind, _ := block["type"].(string)
		if c.nativeBlockKinds == nil {
			c.nativeBlockKinds = map[int]string{}
		}
		c.nativeBlockKinds[index] = kind
		if kind == "text" {
			if text, ok := block["text"].(string); ok && text != "" {
				out.WriteString(c.nativeChatDelta(map[string]any{"content": text}))
			}
			return out.String()
		}
		if kind == "tool_use" {
			arguments := ""
			if input, ok := block["input"].(map[string]any); ok && len(input) > 0 {
				raw, _ := json.Marshal(input)
				arguments = string(raw)
			}
			out.WriteString(c.nativeChatDelta(map[string]any{"tool_calls": []any{map[string]any{"index": index, "id": block["id"], "function": map[string]any{"name": block["name"], "arguments": arguments}}}}))
			return out.String()
		}
		if kind != "thinking" && kind != "redacted_thinking" {
			return out.String()
		}
		if c.nativeThinking == nil {
			c.nativeThinking = map[int]*nativeThinkingItem{}
		}
		n := &nativeThinkingItem{id: identity.RandomID("rs", 12), output: len(c.items), kind: kind, raw: block}
		c.nativeThinking[index] = n
		c.items = append(c.items, responseOutputItem{kind: "native_reasoning", tool: index})
		n.data, _ = block["data"].(string)
		if signature, ok := block["signature"].(string); ok {
			n.signature.WriteString(signature)
		}
		out.WriteString(c.evt("response.output_item.added", map[string]any{"output_index": n.output, "item": n.item("in_progress", true)}))
		if text, ok := block["thinking"].(string); ok && text != "" {
			n.text.WriteString(text)
			c.reasoning.WriteString(text)
			out.WriteString(c.nativeThinkingDelta(n, text))
		}
	case "content_block_delta":
		delta, _ := event["delta"].(map[string]any)
		if c.nativeBlockKinds[index] == "text" && delta["type"] == "text_delta" {
			out.WriteString(c.nativeChatDelta(map[string]any{"content": delta["text"]}))
			return out.String()
		}
		if c.nativeBlockKinds[index] == "tool_use" && delta["type"] == "input_json_delta" {
			out.WriteString(c.nativeChatDelta(map[string]any{"tool_calls": []any{map[string]any{"index": index, "function": map[string]any{"arguments": delta["partial_json"]}}}}))
			return out.String()
		}
		n := c.nativeThinking[index]
		if n == nil {
			return out.String()
		}
		if delta["type"] == "thinking_delta" {
			text, _ := delta["thinking"].(string)
			n.text.WriteString(text)
			c.reasoning.WriteString(text)
			out.WriteString(c.nativeThinkingDelta(n, text))
		}
		if delta["type"] == "signature_delta" {
			text, _ := delta["signature"].(string)
			n.signature.WriteString(text)
		}
	case "content_block_stop":
		if c.nativeBlockKinds[index] == "tool_use" {
			if tool := c.toolCalls[index]; tool != nil && tool.args.Len() == 0 {
				out.WriteString(c.nativeChatDelta(map[string]any{"tool_calls": []any{map[string]any{"index": index, "function": map[string]any{"arguments": "{}"}}}}))
			}
			return out.String()
		}
		n := c.nativeThinking[index]
		if n == nil || n.closed {
			return out.String()
		}
		n.closed = true
		if n.text.Len() > 0 {
			out.WriteString(c.evt("response.reasoning_summary_text.done", map[string]any{"item_id": n.id, "output_index": n.output, "summary_index": 0, "text": n.text.String()}))
			out.WriteString(c.evt("response.reasoning_summary_part.done", map[string]any{"item_id": n.id, "output_index": n.output, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": n.text.String()}}))
		}
		out.WriteString(c.evt("response.output_item.done", map[string]any{"output_index": n.output, "item": n.item("completed", false)}))
	}
	return out.String()
}
func (c *ResponsesStreamConverter) nativeThinkingDelta(n *nativeThinkingItem, text string) string {
	var out strings.Builder
	if n.text.Len() == len(text) {
		out.WriteString(c.evt("response.reasoning_summary_part.added", map[string]any{"item_id": n.id, "output_index": n.output, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}}))
	}
	out.WriteString(c.evt("response.reasoning_summary_text.delta", map[string]any{"item_id": n.id, "output_index": n.output, "summary_index": 0, "delta": text}))
	return out.String()
}

func (c *ResponsesStreamConverter) nativeChatDelta(delta map[string]any) string {
	return c.processChunk(map[string]any{"choices": []any{map[string]any{"delta": delta}}})
}
