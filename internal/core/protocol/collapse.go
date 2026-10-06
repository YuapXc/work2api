package protocol

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
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
	parser := &bridgeStreamParser{
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
			return nil, Usage{}, false, streamErr
		}
		return nil, Usage{}, false, readErr
	}
	if !terminated {
		if streamErr != nil {
			return nil, Usage{}, false, streamErr
		}
		return nil, Usage{}, false, errSSEUnexpectedEOF
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
	doc := encodeBridgeResponse(protocol, acc.response)
	if !acc.reported {
		delete(doc, "usage")
	}
	body, err := json.Marshal(doc)
	return body, acc.response.Usage, acc.reported, err
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
	case "reasoning":
		if event.Text == "" && event.Encrypted == "" && event.Signature == "" {
			break
		}
		acc.reasoningBlock()
		appendReasoningDelta(&acc.reasonText, len(acc.response.Reasoning)-1, event.Text)
		if event.Encrypted != "" {
			acc.splitReasoning()
			acc.reasoningBlock().Encrypted += event.Encrypted
			acc.reason = nil
		}
	case "reasoning_signature":
		acc.reasoningBlock()
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
