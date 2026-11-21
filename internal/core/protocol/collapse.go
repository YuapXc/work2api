package protocol

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"work2api/internal/identity"
	"work2api/internal/jsonutil"
)

// CollapseStream consumes one complete upstream SSE stream and synthesizes
// the equivalent single (non-streaming) response document in the same
// protocol.
//
// It exists for upstream lanes that only serve streaming responses: the
// gateway forces stream:true on the wire and collapses the events back so
// downstream clients that asked for a plain JSON reply keep working.
func CollapseStream(reader io.Reader, protocol Protocol, model string) ([]byte, error) {
	body, _, _, err := CollapseStreamWithUsage(reader, protocol, model)
	return body, err
}

// CollapseStreamWithUsage retains whether the upstream actually supplied usage;
// synthesized zero counters in the encoded response are not an observation.
func CollapseStreamWithUsage(reader io.Reader, protocol Protocol, model string) ([]byte, Usage, bool, error) {
	return CollapseStreamWithUsageContext(context.Background(), reader, protocol, model)
}

// CollapseStreamWithUsageContext observes upstream generation even when the
// client requests a single JSON response. It does not time JSON serialization.
func CollapseStreamWithUsageContext(ctx context.Context, reader io.Reader, protocol Protocol, model string) ([]byte, Usage, bool, error) {
	return CollapseStreamWithRewrite(ctx, reader, protocol, model, nil)
}

// CollapseStreamWithRewrite normalizes upstream events before accumulating them.
func CollapseStreamWithRewrite(ctx context.Context, reader io.Reader, protocol Protocol, model string, rewrite func(map[string]any) error) ([]byte, Usage, bool, error) {
	return collapseStream(ctx, reader, protocol, protocol, model, rewrite)
}

// CollapseChatStreamAs encodes a normalized Chat stream directly as the client protocol.
func CollapseChatStreamAs(ctx context.Context, reader io.Reader, target Protocol, model string, rewrite func(map[string]any) error) ([]byte, Usage, bool, error) {
	switch target {
	case Chat, Anthropic, Responses:
		return collapseStream(ctx, reader, Chat, target, model, rewrite)
	default:
		return nil, Usage{}, false, fmt.Errorf("unsupported response protocol %q", target)
	}
}

func collapseStream(ctx context.Context, reader io.Reader, protocol, target Protocol, model string, rewrite func(map[string]any) error) ([]byte, Usage, bool, error) {
	parser := &bridgeStreamParser{
		generationCtx:     ctx,
		rewrite:           rewrite,
		protocol:          protocol,
		tools:             map[string]bool{},
		toolIDs:           map[string]string{},
		toolNames:         map[string]string{},
		responseArgs:      map[string]bool{},
		responseReasoning: map[string]bool{},
	}
	acc := &collapseAccumulator{tools: map[string]int{}}
	acc.response.Created = time.Now().Unix()
	acc.response.Model = model
	var terminalResponse map[string]any
	var outputSlots responseOutputSlots
	if protocol == Responses {
		parser.captureResponsesEvent = outputSlots.observe
		parser.captureResponsesTerminal = func(response map[string]any) {
			terminalResponse = response
		}
	}
	terminated := false
	streamErr := error(nil)
	readErr := readSSE(reader, func(eventName, data string) error {
		events, err := parser.Parse(eventName, data)
		if err != nil {
			streamErr = err
			return errStreamUpstreamFailure
		}
		for _, event := range events {
			switch event.Kind {
			case "done":
				terminated = true
				return errStreamNormalTermination
			case "error":
				streamErr = fmt.Errorf("%s", firstNonEmpty(event.Error, "upstream stream failed"))
				return errStreamUpstreamFailure
			default:
				acc.apply(event)
			}
		}
		return nil
	})
	if readErr != nil && readErr != errStreamNormalTermination {
		if streamErr != nil {
			return nil, acc.response.Usage, acc.reported, streamErr
		}
		return nil, acc.response.Usage, acc.reported, readErr
	}
	if !terminated {
		if streamErr != nil {
			return nil, acc.response.Usage, acc.reported, streamErr
		}
		return nil, acc.response.Usage, acc.reported, errSSEUnexpectedEOF
	}
	if protocol == Responses && terminalResponse != nil {
		output, _ := terminalResponse["output"].([]any)
		if len(output) == 0 {
			output = outputSlots.output()
		}
		if len(output) == 0 {
			terminalResponse = nil
		} else {
			// The terminal snapshot has the native item types, IDs and order.
			response := acc.response
			response.Tools = nil
			response.Reasoning = nil
			doc := encodeBridgeResponse(protocol, response)
			doc["output"] = output
			if id := jsonutil.StringAt(terminalResponse, "id"); id != "" {
				doc["id"] = id
			}
			return marshalCollapsedResponse(doc, response.Usage, acc.reported)
		}
	}
	acc.response.Text = acc.text.String()
	for index, builder := range acc.arguments {
		acc.response.Tools[index].ArgumentsJSON = builder.String()
	}
	for index, builder := range acc.reasonText {
		acc.response.Reasoning[index].Text = builder.String()
	}
	for index, builder := range acc.reasonSignatures {
		acc.response.Reasoning[index].Signature = builder.String()
	}
	if target != protocol {
		if acc.response.Stop == "error" {
			return nil, acc.response.Usage, acc.reported, fmt.Errorf("upstream response failed")
		}
		// Preserve the response ID projection previously applied by Chat encoding.
		if acc.response.ID == "" {
			acc.response.ID = identity.RandomID("resp", 12)
		}
		acc.response.ID = asPrefix(acc.response.ID, "chatcmpl")
	}
	doc := encodeBridgeResponse(target, acc.response)
	if target == Anthropic && target != protocol && !acc.reported {
		// Messages requires a usage object. Keep wire placeholders separate from observed usage.
		body, err := json.Marshal(doc)
		return body, acc.response.Usage, false, err
	}
	return marshalCollapsedResponse(doc, acc.response.Usage, acc.reported)
}

func marshalCollapsedResponse(doc map[string]any, usage Usage, reported bool) ([]byte, Usage, bool, error) {
	if !reported {
		delete(doc, "usage")
	}
	body, err := json.Marshal(doc)
	return body, usage, reported, err
}

type collapseAccumulator struct {
	response         bridgeResponse
	tools            map[string]int
	reason           *bridgeBlock
	text             strings.Builder
	arguments        map[int]*strings.Builder
	reported         bool
	reasonText       map[int]*strings.Builder
	reasonSignatures map[int]*strings.Builder
}

func (acc *collapseAccumulator) apply(event bridgeStreamEvent) {
	switch event.Kind {
	case "start":
		if event.ResponseID != "" {
			acc.response.ID = event.ResponseID
		}
		if event.Model != "" {
			acc.response.Model = event.Model
		}
	case "text":
		acc.text.WriteString(event.Text)
	case "reasoning_start":
		acc.splitReasoning()
		acc.reasoningBlock().SealSource = event.SealSource
		acc.reasoningBlock().NativeThinking = event.NativeThinking
	case "reasoning_end":
		acc.splitReasoning()
	case "reasoning_state":
		block := acc.reasoningBlock()
		block.SealSource = event.SealSource
		block.NativeThinking = event.NativeThinking
		block.Encrypted = event.Encrypted
		appendReasoningDelta(&acc.reasonText, len(acc.response.Reasoning)-1, event.Text)
		appendReasoningDelta(&acc.reasonSignatures, len(acc.response.Reasoning)-1, event.Signature)
	case "reasoning":
		if event.Text == "" && event.Encrypted == "" && event.Signature == "" {
			break
		}
		acc.reasoningBlock().SealSource = event.SealSource
		appendReasoningDelta(&acc.reasonText, len(acc.response.Reasoning)-1, event.Text)
		if event.Encrypted != "" {
			acc.reasoningBlock().Encrypted += event.Encrypted
			if event.SealSource != Anthropic {
				acc.reason = nil
			}
		}
	case "reasoning_signature":
		acc.reasoningBlock().SealSource = event.SealSource
		appendReasoningDelta(&acc.reasonSignatures, len(acc.response.Reasoning)-1, event.Signature)
	case "tool_start":
		acc.toolBlock(event.ToolKey, event.ToolID, event.ToolName)
	case "tool_delta":
		acc.toolBlock(event.ToolKey, event.ToolID, event.ToolName)
		index := acc.tools[event.ToolKey]
		if acc.arguments == nil {
			acc.arguments = map[int]*strings.Builder{}
		}
		if acc.arguments[index] == nil {
			acc.arguments[index] = &strings.Builder{}
		}
		acc.arguments[index].WriteString(event.Text)
	case "usage":
		if event.Usage != nil {
			acc.reported = true
			mergeBridgeUsage(&acc.response.Usage, *event.Usage)
		}
	case "finish":
		acc.response.Stop = event.Stop
		acc.response.Usage.FinishReason = event.Stop
	}
}

// reasoningBlock returns the reasoning block deltas accumulate into,
// starting a fresh one when the previous block already carries encrypted
// content (a different item on the wire).
func (acc *collapseAccumulator) reasoningBlock() *bridgeBlock {
	if acc.reason == nil {
		acc.response.Reasoning = append(acc.response.Reasoning, bridgeBlock{Kind: "reasoning"})
		acc.reason = &acc.response.Reasoning[len(acc.response.Reasoning)-1]
	}
	return acc.reason
}

func (acc *collapseAccumulator) splitReasoning() {
	acc.reason = nil
}

func (acc *collapseAccumulator) toolBlock(key, id, name string) *bridgeBlock {
	if index, ok := acc.tools[key]; ok {
		block := &acc.response.Tools[index]
		if block.ID == "" {
			block.ID = id
		}
		if block.Name == "" {
			block.Name = name
		}
		return block
	}
	acc.response.Tools = append(acc.response.Tools, bridgeBlock{Kind: "tool_call", ID: id, Name: name})
	acc.tools[key] = len(acc.response.Tools) - 1
	return &acc.response.Tools[len(acc.response.Tools)-1]
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func appendReasoningDelta(builders *map[int]*strings.Builder, index int, delta string) {
	if delta == "" {
		return
	}
	if *builders == nil {
		*builders = map[int]*strings.Builder{}
	}
	if (*builders)[index] == nil {
		(*builders)[index] = &strings.Builder{}
	}
	(*builders)[index].WriteString(delta)
}

type responseOutputSlot struct {
	id          string
	kind        string
	index       int
	indexed     bool
	added, done map[string]any
	parts       map[int]*responseOutputPart
	args        strings.Builder
	argsDone    string
}
type responseOutputPart struct {
	kind string
	text strings.Builder
}
type responseOutputSlots struct {
	slots     []*responseOutputSlot
	ids       map[string]*responseOutputSlot
	indexes   map[int]*responseOutputSlot
	unindexed map[string]*responseOutputSlot
}

func (o *responseOutputSlots) observe(kind string, event map[string]any) {
	switch kind {
	case "response.output_item.added", "response.output_item.done",
		"response.output_text.delta", "response.refusal.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta",
		"response.function_call_arguments.delta", "response.custom_tool_call_input.delta",
		"response.function_call_arguments.done", "response.custom_tool_call_input.done":
	default:
		return
	}
	item := jsonutil.MapAt(event, "item")
	id := jsonutil.FirstString(jsonutil.StringAt(item, "id"), jsonutil.StringAt(event, "item_id"))
	index, indexed := event["output_index"].(float64)

	itemKind := jsonutil.StringAt(item, "type")
	if itemKind == "" {
		switch {
		case strings.Contains(kind, "reasoning"):
			itemKind = "reasoning"
		case strings.Contains(kind, "custom_tool_call"):
			itemKind = "custom_tool_call"
		case strings.Contains(kind, "function_call"):
			itemKind = "function_call"
		default:
			itemKind = "message"
		}
	}
	if o.ids == nil {
		o.ids = map[string]*responseOutputSlot{}
		o.indexes = map[int]*responseOutputSlot{}
		o.unindexed = map[string]*responseOutputSlot{}
	}
	var slot *responseOutputSlot
	if id != "" {
		slot = o.ids[id]
	}
	if slot == nil && kind != "response.output_item.added" {
		candidate := o.unindexed[itemKind]
		if indexed {
			candidate = o.indexes[int(index)]
		}
		if candidate != nil && candidate.kind == itemKind && (id == "" || candidate.id == "" || candidate.id == id) {
			slot = candidate
		}
	}
	if slot == nil {
		slot = &responseOutputSlot{id: id, kind: itemKind, index: int(index), indexed: indexed}
		o.slots = append(o.slots, slot)
	}
	if slot.id == "" {
		slot.id = id
	}
	if slot.id != "" {
		o.ids[slot.id] = slot
	}
	if indexed {
		slot.index, slot.indexed = int(index), true
		o.indexes[int(index)] = slot
	} else {
		o.unindexed[itemKind] = slot
	}
	switch kind {
	case "response.output_item.added":
		slot.added = item
	case "response.output_item.done":
		slot.done = item
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		slot.args.WriteString(jsonutil.StringAt(event, "delta"))
	case "response.function_call_arguments.done":
		slot.argsDone = jsonutil.StringAt(event, "arguments")
	case "response.custom_tool_call_input.done":
		slot.argsDone = jsonutil.StringAt(event, "input")
	default:
		field := "content_index"
		partKind := "output_text"
		if kind == "response.refusal.delta" {
			partKind = "refusal"
		}
		if strings.Contains(kind, "reasoning") {
			field = "summary_index"
			partKind = "summary_text"
		}
		n, _ := event[field].(float64)
		if slot.parts == nil {
			slot.parts = map[int]*responseOutputPart{}
		}
		part := slot.parts[int(n)]
		if part == nil {
			part = &responseOutputPart{kind: partKind}
			slot.parts[int(n)] = part
		}
		part.text.WriteString(jsonutil.StringAt(event, "delta"))
	}
}
func (o *responseOutputSlots) output() []any {
	slots := append([]*responseOutputSlot(nil), o.slots...)
	sort.SliceStable(slots, func(i, j int) bool {
		if slots[i].indexed && slots[j].indexed {
			return slots[i].index < slots[j].index
		}
		return slots[i].indexed && !slots[j].indexed
	})
	var output []any
	for _, slot := range slots {

		item := make(map[string]any, len(slot.added)+len(slot.done)+3)
		for key, value := range slot.added {
			item[key] = value
		}
		for key, value := range slot.done {
			item[key] = value
		}
		if slot.id != "" && item["id"] == nil {
			item["id"] = slot.id
		}
		if item["type"] == nil {
			item["type"] = slot.kind
			if slot.kind == "message" {
				item["role"] = "assistant"
			}
		}
		if item["type"] == "function_call" || item["type"] == "custom_tool_call" {
			field := "arguments"
			if item["type"] == "custom_tool_call" {
				field = "input"
			}
			if jsonutil.StringAt(item, field) == "" {
				value := slot.argsDone
				if value == "" {
					value = slot.args.String()
				}
				item[field] = value
			}
		}
		if len(slot.parts) > 0 {
			field := "content"
			if item["type"] == "reasoning" {
				field = "summary"
			}
			keys := make([]int, 0, len(slot.parts))
			for key := range slot.parts {
				keys = append(keys, key)
			}
			sort.Ints(keys)
			existing := jsonutil.SliceAt(item, field)
			parts := append([]any(nil), existing...)
			for _, key := range keys {
				part := slot.parts[key]
				textField := "text"
				if part.kind == "refusal" {
					textField = "refusal"
				}
				value := part.text.String()
				if key < len(parts) && key >= 0 {
					previous, _ := parts[key].(map[string]any)
					if jsonutil.StringAt(previous, textField) != "" {
						continue
					}
					merged := map[string]any{}
					for k, v := range previous {
						merged[k] = v
					}
					merged["type"] = part.kind
					merged[textField] = value
					parts[key] = merged
				} else {
					parts = append(parts, map[string]any{"type": part.kind, textField: value})
				}
			}
			item[field] = parts
		}
		if jsonutil.StringAt(item, "type") != "" {
			output = append(output, item)
		}
	}
	return output
}
