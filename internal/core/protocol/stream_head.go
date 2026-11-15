package protocol

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"work2api/internal/jsonutil"
	"work2api/internal/streamwatch"
)

// StreamHeadError represents an upstream in-band rejection before downstream
// output. Unknown business errors remain 400, not speculative account retries.
type StreamHeadError struct {
	Status int
	Body   []byte
}

func (e *StreamHeadError) Error() string { return "upstream rejected stream before output" }

// ReadStreamHead retains all prefix bytes and waits for an answer or a true
// terminal event. It imposes no new time limit; the caller owns cancellation.
func ReadStreamHead(reader *bufio.Reader, from Protocol) ([]byte, error) {
	parser := &bridgeStreamParser{protocol: from, tools: map[string]bool{}, toolIDs: map[string]string{}, toolNames: map[string]string{}, responseArgs: map[string]bool{}, responseReasoning: map[string]bool{}}
	var prefix, line []byte
	var event string
	var data []string
	for {
		part, err := reader.ReadSlice('\n')
		if len(prefix)+len(part) > streamwatch.DefaultMaxLineBytes {
			return nil, fmt.Errorf("upstream SSE prelude exceeds byte protection")
		}
		prefix = append(prefix, part...)
		line = append(line, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			return nil, err
		}
		text := strings.TrimSuffix(strings.TrimSuffix(string(line), "\n"), "\r")
		line = line[:0]
		if strings.HasPrefix(text, "event:") {
			event = strings.TrimSpace(strings.TrimPrefix(text, "event:"))
		}
		if strings.HasPrefix(text, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(text, "data:")))
		}
		if text != "" {
			continue
		}
		if len(data) == 0 {
			event = ""
			continue
		}
		payload := strings.Join(data, "\n")
		events, err := parser.Parse(event, payload)
		if err != nil {
			return nil, err
		}
		ready := false
		for _, ev := range events {
			if ev.Kind == "error" {
				status := 400
				var raw map[string]any
				_ = json.Unmarshal([]byte(payload), &raw)
				kind := jsonutil.FirstString(jsonutil.StringAt(raw, "error", "type"), jsonutil.StringAt(raw, "error", "code"), jsonutil.StringAt(raw, "response", "error", "code"))
				switch kind {
				case "rate_limit_error", "rate_limit_exceeded":
					status = 429
				case "authentication_error", "invalid_api_key":
					status = 401
				case "overloaded_error", "server_error", "upstream_error":
					status = 502
				}
				for _, value := range []any{jsonutil.MapAt(raw, "error")["status"], raw["status"]} {
					if n, ok := value.(float64); ok && n >= 400 && n <= 599 && n == float64(int(n)) {
						status = int(n)
					}
				}
				body, _ := json.Marshal(map[string]any{"error": map[string]any{"message": ev.Error, "type": ev.ErrorType}})
				// Keep explicit business codes and correlation fields instead of
				// replacing a structured rejection with a generic message.
				if _, ok := raw["error"].(map[string]any); ok {
					body, _ = json.Marshal(raw)
				}
				return nil, &StreamHeadError{Status: status, Body: body}
			}
			if ev.Kind == "done" || (ev.Kind == "text" || ev.Kind == "reasoning") && ev.Text != "" || ev.Kind == "tool_start" && (ev.ToolName != "" || ev.ToolID != "") || ev.Kind == "tool_delta" && ev.Text != "" {
				ready = true
			}
		}
		if ready {
			return prefix, nil
		}
		event = ""
		data = data[:0]
	}
}

// EmptyReplyProbe delays only a bounded SSE prelude until output is known.
type EmptyReplyProbe struct {
	parser             *bridgeStreamParser
	line               []byte
	data               string
	event              string
	stop               string
	Done, Empty, Ready bool
}

func NewEmptyReplyProbe(from Protocol) *EmptyReplyProbe {
	return &EmptyReplyProbe{parser: &bridgeStreamParser{protocol: from, tools: map[string]bool{}, toolIDs: map[string]string{}, toolNames: map[string]string{}, responseArgs: map[string]bool{}, responseReasoning: map[string]bool{}}}
}
func (p *EmptyReplyProbe) Observe(data []byte) {
	for _, b := range data {
		if p.Ready {
			return
		}
		if b != '\n' {
			p.line = append(p.line, b)
			continue
		}
		line := strings.TrimSuffix(string(p.line), "\r")
		p.line = p.line[:0]
		if strings.HasPrefix(line, "event:") {
			p.event = strings.TrimSpace(line[6:])
		}
		if strings.HasPrefix(line, "data:") {
			if p.data != "" {
				p.data += "\n"
			}
			p.data += strings.TrimSpace(line[5:])
		}
		if line != "" {
			continue
		}
		if p.data == "" {
			p.event = ""
			continue
		}
		if p.parser.protocol == Responses {
			var terminal struct {
				Type     string          `json:"type"`
				Response json.RawMessage `json:"response"`
			}
			if json.Unmarshal([]byte(p.data), &terminal) == nil && terminal.Type == "response.completed" {
				if !CompleteEmptyResponse(Responses, terminal.Response) {
					p.Ready = true
					return
				}
			}
		}
		events, err := p.parser.Parse(p.event, p.data)
		p.event, p.data = "", ""
		if err != nil {
			p.Ready = true
			return
		}
		for _, ev := range events {
			switch ev.Kind {
			case "text", "reasoning":
				if ev.Text != "" || ev.Encrypted != "" || ev.NativeThinking != nil {
					p.Ready = true
				}
			case "tool_start", "tool_delta", "error":
				p.Ready = true
			case "finish":
				p.stop = ev.Stop
				if ev.Stop != "stop" {
					p.Ready = true
				}
			case "done":
				p.Done = true
				p.Empty = !p.Ready && p.stop == "stop"
				p.Ready = p.Ready || !p.Empty
			}
		}
	}
}
func CompleteEmptyResponse(from Protocol, body []byte) bool {
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return false
	}
	if from == Responses && doc["status"] != "completed" {
		return false
	}
	r, err := decodeBridgeResponse(from, doc)
	return err == nil && r.Stop == "stop" && r.Error == "" && r.Text == "" && len(r.Tools) == 0 && len(r.Reasoning) == 0
}
