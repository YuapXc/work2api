package protocol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash"
	"strings"

	"work2api/internal/jsonutil"
	"work2api/internal/streamwatch"
)

type bridgeStreamParser struct {
	responseReasonHashes     map[string]hash.Hash
	responseReasoningDone    map[string]bool
	anthropicThinking        map[string]bool
	generationCtx            context.Context
	rewrite                  func(map[string]any) error
	protocol                 Protocol
	started                  bool
	tools                    map[string]bool
	toolIDs                  map[string]string
	toolNames                map[string]string
	toolOrder                []string
	responseArgs             map[string]bool
	responseReasoning        map[string]bool
	captureResponsesTerminal func(map[string]any)
}

func (parser *bridgeStreamParser) Parse(eventName, data string) ([]bridgeStreamEvent, error) {
	if data == "[DONE]" {
		if parser.protocol != Chat {
			return nil, fmt.Errorf("unexpected [DONE] terminator for %s stream", parser.protocol)
		}
		if parser.rewrite != nil {
			if err := parser.rewrite(nil); err != nil {
				return nil, err
			}
		}
		return []bridgeStreamEvent{{Kind: "done"}}, nil
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(data), &value); err != nil {
		return nil, fmt.Errorf("invalid upstream SSE JSON: %w", err)
	}
	parser.observeGeneration(eventName, value)
	if parser.rewrite != nil {
		if err := parser.rewrite(value); err != nil {
			return nil, err
		}
	}
	var events []bridgeStreamEvent
	var err error
	switch parser.protocol {
	case Chat:
		events = parser.parseChatEvent(eventName, value)
	case Anthropic:
		events, err = parser.parseAnthropicEvent(eventName, value)
	case Responses:
		events = parser.parseResponses(eventName, value)
	default:
		return nil, fmt.Errorf("unsupported stream protocol %q", parser.protocol)
	}
	for i := range events {
		if events[i].SealSource == "" {
			events[i].SealSource = parser.protocol
		}
	}
	return events, err
}

func (parser *bridgeStreamParser) observeGeneration(eventName string, value map[string]any) {
	if parser.generationCtx == nil {
		return
	}
	generated := false
	kind := jsonutil.FirstString(jsonutil.StringAt(value, "type"), eventName)
	switch parser.protocol {
	case Chat:
		generated = streamwatch.ChatGeneration(value)
	case Anthropic:
		if kind == "content_block_delta" {
			delta := jsonutil.MapAt(value, "delta")
			for _, key := range []string{"text", "thinking", "partial_json"} {
				generated = generated || jsonutil.StringAt(delta, key) != ""
			}
		}
	case Responses:
		switch kind {
		case "response.output_text.delta", "response.refusal.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
			generated = jsonutil.StringAt(value, "delta") != ""
		}
	}
	if generated {
		streamwatch.Generation(parser.generationCtx)
	}
}

func (parser *bridgeStreamParser) parseChat(value map[string]any) []bridgeStreamEvent {
	return parser.parseChatEvent("", value)
}

func (parser *bridgeStreamParser) parseChatEvent(eventName string, value map[string]any) []bridgeStreamEvent {
	events := make([]bridgeStreamEvent, 0, 4)
	if eventName == "error" {
		return []bridgeStreamEvent{{Kind: "error", Error: streamErrorMessage(value, "upstream Chat stream error"), ErrorType: streamErrorType(value)}}
	}
	if rawError, exists := value["error"]; exists && rawError != nil {
		return []bridgeStreamEvent{{Kind: "error", Error: streamErrorMessage(value, "upstream Chat stream error"), ErrorType: streamErrorType(value)}}
	}
	if !parser.started {
		if id := jsonutil.StringAt(value, "id"); id != "" {
			parser.started = true
			events = append(events, bridgeStreamEvent{Kind: "start", ResponseID: id, Model: jsonutil.StringAt(value, "model")})
		}
	}
	if usageMap := jsonutil.MapAt(value, "usage"); len(usageMap) > 0 {
		usage := decodeOpenAIUsage(usageMap)
		events = append(events, bridgeStreamEvent{Kind: "usage", Usage: &usage})
	}
	for _, raw := range jsonutil.SliceAt(value, "choices") {
		choice, _ := raw.(map[string]any)
		delta := jsonutil.MapAt(choice, "delta")
		reasoning := jsonutil.StringAt(delta, "reasoning_content")
		if reasoning == "" {
			reasoning = jsonutil.StringAt(delta, "reasoning")
		}
		if reasoning != "" {
			events = append(events, bridgeStreamEvent{Kind: "reasoning", Text: reasoning})
		}
		if text := jsonutil.StringAt(delta, "content"); text != "" {
			events = append(events, bridgeStreamEvent{Kind: "text", Text: text})
		}
		if refusal := jsonutil.StringAt(delta, "refusal"); refusal != "" {
			events = append(events, bridgeStreamEvent{Kind: "text", Text: refusal})
		}
		for _, rawCall := range jsonutil.SliceAt(delta, "tool_calls") {
			call, _ := rawCall.(map[string]any)
			key := fmt.Sprint(jsonutil.FirstAny(call["index"], jsonutil.StringAt(call, "id")))
			function := jsonutil.MapAt(call, "function")
			id := jsonutil.StringAt(call, "id")
			if _, seen := parser.tools[key]; !seen {
				parser.tools[key] = false
				parser.toolOrder = append(parser.toolOrder, key)
			}
			if id != "" {
				parser.toolIDs[key] = id
			}
			if name := jsonutil.StringAt(function, "name"); name != "" {
				parser.toolNames[key] = mergeToolName(parser.toolNames[key], name)
			}
			if arguments := jsonutil.StringAt(function, "arguments"); arguments != "" {
				if !parser.tools[key] && parser.toolNames[key] != "" {
					parser.tools[key] = true
					events = append(events, bridgeStreamEvent{Kind: "tool_start", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: parser.toolNames[key]})
				}
				events = append(events, bridgeStreamEvent{Kind: "tool_delta", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: parser.toolNames[key], Text: arguments})
			}
		}
		if stop := jsonutil.StringAt(choice, "finish_reason"); stop != "" {
			if isStreamErrorFinish(stop) {
				events = append(events, bridgeStreamEvent{Kind: "error", Error: jsonutil.FirstString(jsonutil.StringAt(choice, "error", "message"), "upstream Chat stream failed"), ErrorType: "upstream_error"})
				continue
			}
			for _, key := range parser.toolOrder {
				if !parser.tools[key] && parser.toolNames[key] != "" {
					parser.tools[key] = true
					events = append(events, bridgeStreamEvent{Kind: "tool_start", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: parser.toolNames[key]})
				}
			}
			events = append(events, bridgeStreamEvent{Kind: "finish", Stop: stop})
		}
	}
	return events
}

func (parser *bridgeStreamParser) parseAnthropic(value map[string]any) ([]bridgeStreamEvent, error) {
	return parser.parseAnthropicEvent("", value)
}

func (parser *bridgeStreamParser) parseAnthropicEvent(eventName string, value map[string]any) ([]bridgeStreamEvent, error) {
	typeName := jsonutil.StringAt(value, "type")
	if typeName == "" {
		typeName = eventName
	}
	switch typeName {
	case "message_start":
		message := jsonutil.MapAt(value, "message")
		events := []bridgeStreamEvent{{Kind: "start", ResponseID: jsonutil.StringAt(message, "id"), Model: jsonutil.StringAt(message, "model")}}
		if usageMap := jsonutil.MapAt(message, "usage"); len(usageMap) > 0 {
			usage := decodeAnthropicUsage(usageMap)
			events = append(events, bridgeStreamEvent{Kind: "usage", Usage: &usage})
		}
		return events, nil
	case "content_block_start":
		block := jsonutil.MapAt(value, "content_block")
		key := fmt.Sprint(value["index"])
		switch jsonutil.StringAt(block, "type") {
		case "thinking":
			if parser.anthropicThinking == nil {
				parser.anthropicThinking = map[string]bool{}
			}
			parser.anthropicThinking[key] = true
			events := []bridgeStreamEvent{{Kind: "reasoning_start", NativeThinking: block}}
			if thinking := jsonutil.StringAt(block, "thinking"); thinking != "" {
				events = append(events, bridgeStreamEvent{Kind: "reasoning", Text: thinking})
			}
			if signature := jsonutil.StringAt(block, "signature"); signature != "" {
				events = append(events, bridgeStreamEvent{Kind: "reasoning_signature", Signature: signature})
			}
			return events, nil
		case "redacted_thinking":
			if parser.anthropicThinking == nil {
				parser.anthropicThinking = map[string]bool{}
			}
			parser.anthropicThinking[key] = true
			return []bridgeStreamEvent{{Kind: "reasoning_start", NativeThinking: block}, {Kind: "reasoning", Encrypted: jsonutil.StringAt(block, "data")}}, nil
		case "tool_use":
			parser.tools[key] = true
			return []bridgeStreamEvent{{Kind: "tool_start", ToolKey: key, ToolID: jsonutil.StringAt(block, "id"), ToolName: jsonutil.StringAt(block, "name")}}, nil
		case "text":
			if text := jsonutil.StringAt(block, "text"); text != "" {
				return []bridgeStreamEvent{{Kind: "text", Text: text}}, nil
			}
		}
	case "content_block_delta":
		delta := jsonutil.MapAt(value, "delta")
		key := fmt.Sprint(value["index"])
		switch jsonutil.StringAt(delta, "type") {
		case "thinking_delta":
			return []bridgeStreamEvent{{Kind: "reasoning", Text: jsonutil.StringAt(delta, "thinking")}}, nil
		case "signature_delta":
			return []bridgeStreamEvent{{Kind: "reasoning_signature", Signature: jsonutil.StringAt(delta, "signature")}}, nil
		case "text_delta":
			return []bridgeStreamEvent{{Kind: "text", Text: jsonutil.StringAt(delta, "text")}}, nil
		case "input_json_delta":
			return []bridgeStreamEvent{{Kind: "tool_delta", ToolKey: key, Text: jsonutil.StringAt(delta, "partial_json")}}, nil
		}
	case "content_block_stop":
		key := fmt.Sprint(value["index"])
		if parser.anthropicThinking[key] {
			delete(parser.anthropicThinking, key)
			return []bridgeStreamEvent{{Kind: "reasoning_end"}}, nil
		}
	case "message_delta":
		events := make([]bridgeStreamEvent, 0, 2)
		if usageMap := jsonutil.MapAt(value, "usage"); len(usageMap) > 0 {
			usage := decodeAnthropicUsage(usageMap)
			events = append(events, bridgeStreamEvent{Kind: "usage", Usage: &usage})
		}
		if stop := jsonutil.StringAt(value, "delta", "stop_reason"); stop != "" {
			if isStreamErrorFinish(stop) {
				events = append(events, bridgeStreamEvent{Kind: "error", Error: "upstream Anthropic stream failed", ErrorType: "upstream_error"})
				return events, nil
			}
			events = append(events, bridgeStreamEvent{Kind: "finish", Stop: stop})
		}
		return events, nil
	case "message_stop":
		return []bridgeStreamEvent{{Kind: "done"}}, nil
	case "error":
		message := jsonutil.FirstString(jsonutil.StringAt(value, "error", "message"), "upstream Anthropic stream error")
		return []bridgeStreamEvent{{Kind: "error", Error: message, ErrorType: jsonutil.FirstString(jsonutil.StringAt(value, "error", "type"), "upstream_error")}}, nil
	}
	return nil, nil
}

func (parser *bridgeStreamParser) parseResponses(eventName string, value map[string]any) []bridgeStreamEvent {
	typeName := jsonutil.StringAt(value, "type")
	if typeName == "" {
		typeName = eventName
	}
	switch typeName {
	case "error":
		return []bridgeStreamEvent{{Kind: "error", Error: streamErrorMessage(value, "upstream Responses stream error"), ErrorType: streamErrorType(value)}}
	case "response.created":
		response := jsonutil.MapAt(value, "response")
		return []bridgeStreamEvent{{Kind: "start", ResponseID: jsonutil.StringAt(response, "id"), Model: jsonutil.StringAt(response, "model")}}
	case "response.output_text.delta":
		return []bridgeStreamEvent{{Kind: "text", Text: jsonutil.StringAt(value, "delta")}}
	case "response.refusal.delta":
		return []bridgeStreamEvent{{Kind: "text", Text: jsonutil.StringAt(value, "delta")}}
	case "response.reasoning_summary_text.delta":
		key := responseToolKey(value, nil)
		parser.responseReasoning[key] = true
		text := jsonutil.StringAt(value, "delta")
		if parser.responseReasonHashes == nil {
			parser.responseReasonHashes = map[string]hash.Hash{}
		}
		if parser.responseReasonHashes[key] == nil {
			parser.responseReasonHashes[key] = sha256.New()
		}
		_, _ = parser.responseReasonHashes[key].Write([]byte(text))
		return []bridgeStreamEvent{{Kind: "reasoning", Text: text}}
	case "response.output_item.added":
		item := jsonutil.MapAt(value, "item")
		if jsonutil.StringAt(item, "type") == "function_call" {
			key := responseToolKey(value, item)
			parser.rememberTool(key, jsonutil.StringAt(item, "call_id"), jsonutil.StringAt(item, "name"))
			if parser.toolNames[key] != "" {
				parser.tools[key] = true
				return []bridgeStreamEvent{{Kind: "tool_start", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: parser.toolNames[key]}}
			}
		}
	case "response.function_call_arguments.delta":
		key := responseToolKey(value, nil)
		parser.responseArgs[key] = true
		return []bridgeStreamEvent{{Kind: "tool_delta", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: parser.toolNames[key], Text: jsonutil.StringAt(value, "delta")}}
	case "response.function_call_arguments.done":
		key := responseToolKey(value, nil)
		if !parser.responseArgs[key] {
			parser.responseArgs[key] = true
			return []bridgeStreamEvent{{Kind: "tool_delta", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: parser.toolNames[key], Text: jsonutil.StringAt(value, "arguments")}}
		}
	case "response.output_item.done":
		item := jsonutil.MapAt(value, "item")
		if jsonutil.StringAt(item, "type") == "reasoning" {
			key := responseToolKey(value, item)
			if parser.responseReasoningDone[key] {
				return nil
			}
			if parser.responseReasoningDone == nil {
				parser.responseReasoningDone = map[string]bool{}
			}
			parser.responseReasoningDone[key] = true
			blocks := decodeResponsesReasoning(item)
			if len(blocks) > 0 {
				text := blocks[0].Text
				var events []bridgeStreamEvent
				if parser.responseReasoning[key] {
					different := false
					if blocks[0].SealSource == Anthropic && blocks[0].Signature != "" {
						expected := sha256.Sum256([]byte(text))
						seen := parser.responseReasonHashes[key]
						different = seen == nil || !bytes.Equal(seen.Sum(nil), expected[:])
					}
					if different {
						events = append(events, bridgeStreamEvent{Kind: "reasoning_start"})
					} else {
						text = ""
					}
				}
				delete(parser.responseReasonHashes, key)
				return append(events, bridgeStreamEvent{Kind: "reasoning_state", Text: text, Encrypted: blocks[0].Encrypted, Signature: blocks[0].Signature, SealSource: blocks[0].SealSource, NativeThinking: blocks[0].NativeThinking}, bridgeStreamEvent{Kind: "reasoning_end"})
			}
			return nil
		}
		if jsonutil.StringAt(item, "type") == "function_call" {
			key := responseToolKey(value, item)
			parser.rememberTool(key, jsonutil.StringAt(item, "call_id"), jsonutil.StringAt(item, "name"))
			events := make([]bridgeStreamEvent, 0, 2)
			if !parser.tools[key] {
				parser.tools[key] = true
				events = append(events, bridgeStreamEvent{Kind: "tool_start", ToolKey: key, ToolID: parser.toolIDs[key], ToolName: parser.toolNames[key]})
			}
			if !parser.responseArgs[key] {
				if arguments := jsonutil.StringAt(item, "arguments"); arguments != "" {
					events = append(events, bridgeStreamEvent{Kind: "tool_delta", ToolKey: key, Text: arguments})
				}
			}
			return events
		}
	case "response.completed", "response.incomplete", "response.failed", "response.done":
		response := jsonutil.MapAt(value, "response")
		usageMap := jsonutil.MapAt(response, "usage")
		if typeName == "response.failed" || jsonutil.StringAt(response, "status") == "failed" {
			message := jsonutil.FirstString(jsonutil.StringAt(response, "error", "message"), jsonutil.StringAt(value, "error", "message"), "upstream Responses request failed")
			events := []bridgeStreamEvent{{Kind: "error", Error: message, ErrorType: jsonutil.FirstString(jsonutil.StringAt(response, "error", "code"), "upstream_error")}}
			if len(usageMap) > 0 {
				usage := decodeOpenAIUsage(usageMap)
				events = append([]bridgeStreamEvent{{Kind: "usage", Usage: &usage}}, events...)
			}
			return events
		}
		if parser.captureResponsesTerminal != nil {
			parser.captureResponsesTerminal(response)
		}
		var stop string
		if typeName == "response.incomplete" || jsonutil.StringAt(response, "status") == "incomplete" {
			stop = canonicalResponsesIncomplete(jsonutil.StringAt(response, "incomplete_details", "reason"))
		} else {
			stop = parser.responsesCompletedStop(response)
		}
		events := make([]bridgeStreamEvent, 0, 3)
		for i, raw := range jsonutil.SliceAt(response, "output") {
			item, _ := raw.(map[string]any)
			if item["type"] == "reasoning" {
				events = append(events, parser.parseResponses("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})...)
			}
		}
		if len(usageMap) > 0 {
			usage := decodeOpenAIUsage(usageMap)
			events = append(events, bridgeStreamEvent{Kind: "usage", Usage: &usage})
		}
		return append(events, bridgeStreamEvent{Kind: "finish", Stop: stop}, bridgeStreamEvent{Kind: "done"})
	}
	return nil
}

// responsesCompletedStop decides how a finished Responses turn is reported
// downstream. The completed event carries no stop reason, so the function_call
// items in its output — or the tool calls observed earlier on the stream — are
// the only signal that the turn ended waiting for tool results. Defaulting to
// "stop" made every tool step terminate the downstream turn instead of chaining
// inside it.
func (parser *bridgeStreamParser) responsesCompletedStop(response map[string]any) string {
	for _, raw := range jsonutil.SliceAt(response, "output") {
		item, _ := raw.(map[string]any)
		kind := jsonutil.StringAt(item, "type")
		if kind == "function_call" || (kind == "custom_tool_call" && parser.captureResponsesTerminal != nil) {
			return "tool_calls"
		}
	}
	if len(parser.toolOrder) > 0 {
		return "tool_calls"
	}
	return "stop"
}

func streamErrorMessage(value map[string]any, fallback string) string {
	if message := jsonutil.StringAt(value, "error", "message"); message != "" {
		return message
	}
	if message := jsonutil.StringAt(value, "message"); message != "" {
		return message
	}
	if message, ok := value["error"].(string); ok && message != "" {
		return message
	}
	return fallback
}

func streamErrorType(value map[string]any) string {
	return jsonutil.FirstString(jsonutil.StringAt(value, "error", "type"), jsonutil.StringAt(value, "error", "code"), "upstream_error")
}

func isStreamErrorFinish(stop string) bool {
	switch strings.ToLower(strings.TrimSpace(stop)) {
	case "error", "failed", "failure", "network_error", "server_error":
		return true
	default:
		return false
	}
}

func (parser *bridgeStreamParser) rememberTool(key, id, name string) {
	if _, seen := parser.tools[key]; !seen {
		parser.tools[key] = false
		parser.toolOrder = append(parser.toolOrder, key)
	}
	if id != "" {
		parser.toolIDs[key] = id
	}
	if name != "" {
		parser.toolNames[key] = name
	}
}

func mergeToolName(current, fragment string) string {
	if current == "" || fragment == current {
		return fragment
	}
	if strings.HasPrefix(fragment, current) {
		return fragment
	}
	return current + fragment
}

func responseToolKey(event map[string]any, item map[string]any) string {
	if item != nil {
		if id := jsonutil.StringAt(item, "id"); id != "" {
			return id
		}
	}
	if id := jsonutil.StringAt(event, "item_id"); id != "" {
		return id
	}
	return fmt.Sprint(event["output_index"])
}
