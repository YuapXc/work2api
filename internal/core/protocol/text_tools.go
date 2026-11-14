package protocol

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"work2api/internal/streamwatch"
)

type TextToolPart struct {
	Text  string
	Calls []any
}
type TextToolParser struct {
	prefix             string
	headerPosition     int
	pending            []byte
	candidate          bool
	position, depth    int
	quoted, escaped    bool
	start, initialized bool
	fence, inline      bool
	ticks              int
	next               int
	Seen               bool
}

func (p *TextToolParser) Feed(text string, final bool) ([]TextToolPart, error) {
	if !p.initialized {
		p.start = true
		p.initialized = true
	}
	var parts []TextToolPart
	var plain strings.Builder
	flush := func() {
		if plain.Len() > 0 {
			parts = append(parts, TextToolPart{Text: plain.String()})
			plain.Reset()
		}
	}
	emit := func(data []byte) {
		plain.Write(data)
		for _, b := range data {
			if b == '`' {
				p.start = false
				p.ticks++
				continue
			}
			if p.ticks >= 3 {
				if (p.ticks/3)%2 == 1 {
					p.fence = !p.fence
				}
				p.inline = false
				if p.ticks%3 == 1 {
					p.inline = true
				}
			} else if p.ticks == 1 {
				p.inline = !p.inline
			}
			p.ticks = 0
			if b == '\n' {
				p.inline = false
				p.start = true
			} else if b != ' ' && b != '\t' && b != '\r' {
				p.start = false
			}
		}
	}
	if len(p.pending) == 0 && !strings.ContainsAny(text, "{`") {
		emit([]byte(text))
		flush()
		return parts, nil
	}
	const header = `{"tool_calls":`
	for i := 0; i < len(text); i++ {
		b := text[i]
		if len(p.pending) == 0 && (!p.start || p.fence || p.inline || b != '{') {
			emit([]byte{b})
			continue
		}
		p.pending = append(p.pending, b)
		if len(p.pending) > streamwatch.DefaultMaxLineBytes {
			return parts, errors.New("text tool call exceeds the existing event size protection")
		}
		if !p.candidate {
			if b == ' ' || b == '\t' || b == '\r' || b == '\n' {
				continue
			}
			if p.headerPosition >= len(header) || b != header[p.headerPosition] {
				emit(p.pending)
				p.pending = nil
				p.headerPosition = 0
				continue
			}
			p.headerPosition++
			if p.headerPosition != len(header) {
				continue
			}
			p.candidate = true
			p.position = 0
			p.depth = 0
			p.quoted = false
			p.escaped = false
		}
		end := false
		for p.position < len(p.pending) {
			c := p.pending[p.position]
			p.position++
			if p.quoted {
				if p.escaped {
					p.escaped = false
				} else if c == '\\' {
					p.escaped = true
				} else if c == '"' {
					p.quoted = false
				}
				continue
			}
			switch c {
			case '"':
				p.quoted = true
			case '{', '[':
				p.depth++
			case '}', ']':
				p.depth--
				if p.depth == 0 {
					end = true
				}
			}
			if end {
				break
			}
		}
		if !end {
			continue
		}
		calls, err := p.calls(p.pending)
		if err != nil {
			return parts, err
		}
		if len(calls) == 0 {
			emit(p.pending)
		} else {
			flush()
			parts = append(parts, TextToolPart{Calls: calls})
			p.Seen = true
			p.start = false
		}
		p.pending = nil
		p.candidate = false
		p.headerPosition = 0
	}
	if final && len(p.pending) > 0 {
		if p.candidate {
			return parts, errors.New("model tool call written as text was cut short")
		}
		emit(p.pending)
		p.pending = nil
	}
	flush()
	return parts, nil
}

func (p *TextToolParser) calls(raw []byte) ([]any, error) {
	fail := func() ([]any, error) { return nil, errors.New("model tool call written as text cannot be read") }
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return fail()
	}
	var list []json.RawMessage
	if json.Unmarshal(doc["tool_calls"], &list) != nil || len(list) == 0 {
		return nil, nil
	}
	calls := make([]any, 0, len(list))
	for _, entry := range list {
		var fn struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Function  json.RawMessage `json:"function"`
		}
		if json.Unmarshal(entry, &fn) != nil {
			return fail()
		}
		if len(fn.Function) > 0 && string(fn.Function) != "null" {
			rawFunction := fn.Function
			fn.Name, fn.Arguments, fn.Function = "", nil, nil
			if json.Unmarshal(rawFunction, &fn) != nil {
				return fail()
			}
		}
		args := bytes.TrimSpace(fn.Arguments)
		if len(args) > 0 && args[0] == '"' {
			var value string
			if json.Unmarshal(args, &value) != nil {
				return fail()
			}
			args = []byte(value)
		}
		var object map[string]json.RawMessage
		if strings.TrimSpace(fn.Name) == "" || json.Unmarshal(args, &object) != nil || object == nil {
			return fail()
		}
		if p.prefix == "" {
			var id [12]byte
			if _, err := rand.Read(id[:]); err != nil {
				return nil, err
			}
			p.prefix = hex.EncodeToString(id[:])
		}
		calls = append(calls, map[string]any{"index": p.next, "id": fmt.Sprintf("call_%s_%d", p.prefix, p.next), "type": "function", "function": map[string]any{"name": strings.TrimSpace(fn.Name), "arguments": string(args)}})
		p.next++
	}
	return calls, nil
}
