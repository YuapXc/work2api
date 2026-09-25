package adapters

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"work2api/internal/workbuddy/upstream"
)

// AnthropicStreamConverter converts an OpenAI Chat SSE stream into an Anthropic
// Messages SSE event stream in real time.
type AnthropicStreamConverter struct {
	msgID     string
	model     string
	createdAt int64

	emittedStart bool

	textContent   strings.Builder
	textBlockOpen bool
	textBlockIdx  int

	toolUses     map[int]*toolUseSlot
	toolOrder    []int
	nextBlockIdx int

	finishReason string
	usage        map[string]any
}

type toolUseSlot struct {
	id       string
	name     string
	args     strings.Builder
	blockIdx int
	open     bool
}

// NewAnthropicStreamConverter creates a converter for the given model.
func NewAnthropicStreamConverter(model string) *AnthropicStreamConverter {
	if model == "" {
		model = "unknown"
	}
	return &AnthropicStreamConverter{
		msgID:     randID("msg_"),
		model:     model,
		createdAt: time.Now().Unix(),
		toolUses:  map[int]*toolUseSlot{},
	}
}

// FeedLine processes one SSE line and returns Anthropic SSE event text.
func (c *AnthropicStreamConverter) FeedLine(line string) string {
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

// Finish emits the closing events.
func (c *AnthropicStreamConverter) Finish() string {
	var events strings.Builder
	if c.textBlockOpen {
		events.WriteString(c.evt("content_block_stop", map[string]any{"index": c.textBlockIdx}))
		c.textBlockOpen = false
	}
	for _, idx := range c.toolOrder {
		tc := c.toolUses[idx]
		if tc.open {
			events.WriteString(c.evt("content_block_stop", map[string]any{"index": tc.blockIdx}))
			tc.open = false
		}
	}
	stopReason := mapStopReason(c.finishReason)
	delta := map[string]any{"stop_reason": stopReason, "stop_sequence": nil}
	var usage any
	if c.usage != nil {
		usage = map[string]any{
			"input_tokens":  intOr(c.usage, "prompt_tokens"),
			"output_tokens": intOr(c.usage, "completion_tokens"),
		}
	}
	events.WriteString(c.evt("message_delta", map[string]any{"delta": delta, "usage": usage}))
	events.WriteString(c.evt("message_stop", map[string]any{}))
	return events.String()
}

// GetNonstreamResponse returns the full non-streaming Message response object.
func (c *AnthropicStreamConverter) GetNonstreamResponse() map[string]any {
	content := c.buildContentBlocks()
	resp := map[string]any{
		"id":            c.msgID,
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         c.model,
		"stop_reason":   mapStopReason(c.finishReason),
		"stop_sequence": nil,
	}
	if c.usage != nil {
		resp["usage"] = map[string]any{
			"input_tokens":  intOr(c.usage, "prompt_tokens"),
			"output_tokens": intOr(c.usage, "completion_tokens"),
		}
	}
	return resp
}

func (c *AnthropicStreamConverter) processChunk(chunk map[string]any) string {
	var events strings.Builder

	if m, ok := chunk["model"].(string); ok && m != "" {
		c.model = m
	}
	if !c.emittedStart {
		events.WriteString(c.evt("message_start", map[string]any{
			"message": map[string]any{
				"id":      c.msgID,
				"type":    "message",
				"role":    "assistant",
				"content": []any{},
				"model":   c.model,
				"usage":   map[string]any{"input_tokens": 0, "output_tokens": 0},
			},
		}))
		c.emittedStart = true
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
				c.textContent.WriteString(content)
				if !c.textBlockOpen {
					c.textBlockIdx = c.nextBlockIdx
					c.nextBlockIdx++
					events.WriteString(c.evt("content_block_start", map[string]any{
						"index":         c.textBlockIdx,
						"content_block": map[string]any{"type": "text", "text": ""},
					}))
					c.textBlockOpen = true
				}
				events.WriteString(c.evt("content_block_delta", map[string]any{
					"index": c.textBlockIdx,
					"delta": map[string]any{"type": "text_delta", "text": content},
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
				slot, exists := c.toolUses[idx]
				if !exists {
					slot = &toolUseSlot{blockIdx: c.nextBlockIdx}
					c.nextBlockIdx++
					c.toolUses[idx] = slot
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
				if !slot.open {
					events.WriteString(c.evt("content_block_start", map[string]any{
						"index":         slot.blockIdx,
						"content_block": map[string]any{"type": "tool_use", "id": slot.id, "name": slot.name, "input": map[string]any{}},
					}))
					slot.open = true
				}
				if fn != nil {
					if args, ok := fn["arguments"].(string); ok && args != "" {
						slot.args.WriteString(args)
						events.WriteString(c.evt("content_block_delta", map[string]any{
							"index": slot.blockIdx,
							"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
						}))
					}
				}
			}
		}

		if finish != "" {
			c.finishReason = finish
			if c.textBlockOpen {
				events.WriteString(c.evt("content_block_stop", map[string]any{"index": c.textBlockIdx}))
				c.textBlockOpen = false
			}
			for _, i := range c.toolOrder {
				tc := c.toolUses[i]
				if tc.open {
					events.WriteString(c.evt("content_block_stop", map[string]any{"index": tc.blockIdx}))
					tc.open = false
				}
			}
		}
	}
	return events.String()
}

func (c *AnthropicStreamConverter) evt(eventType string, data map[string]any) string {
	payload := map[string]any{"type": eventType}
	for k, v := range data {
		payload[k] = v
	}
	b, _ := json.Marshal(payload)
	return "event: " + eventType + "\ndata: " + string(b) + "\n\n"
}

func (c *AnthropicStreamConverter) buildContentBlocks() []any {
	var blocks []any
	if c.textContent.Len() > 0 || c.textBlockOpen {
		blocks = append(blocks, map[string]any{"type": "text", "text": c.textContent.String()})
	}
	idxs := make([]int, len(c.toolOrder))
	copy(idxs, c.toolOrder)
	sort.Ints(idxs)
	for _, i := range idxs {
		tc := c.toolUses[i]
		block := map[string]any{"type": "tool_use", "id": tc.id, "name": tc.name, "input": map[string]any{}}
		var parsed any
		if json.Unmarshal([]byte(tc.args.String()), &parsed) == nil {
			block["input"] = parsed
		} else {
			block["input"] = tc.args.String()
		}
		blocks = append(blocks, block)
	}
	return blocks
}

// ToolsSummary returns a compact tool-call summary for usage logging.
func (c *AnthropicStreamConverter) ToolsSummary() string {
	idxs := make([]int, len(c.toolOrder))
	copy(idxs, c.toolOrder)
	sort.Ints(idxs)
	var parts strings.Builder
	for _, i := range idxs {
		tc := c.toolUses[i]
		name := tc.name
		if name == "" {
			name = "?"
		}
		parts.WriteString("<tool_call:" + name + " " + tc.args.String() + ">")
	}
	return parts.String()
}

// TextContent returns the accumulated assistant text (for usage logging).
func (c *AnthropicStreamConverter) TextContent() string { return c.textContent.String() }

// Usage returns the merged usage map (may be nil).
func (c *AnthropicStreamConverter) Usage() map[string]any { return c.usage }

func mapStopReason(sr string) string {
	switch sr {
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

func intOr(m map[string]any, key string) int {
	if n, ok := toInt(m[key]); ok {
		return n
	}
	return 0
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
