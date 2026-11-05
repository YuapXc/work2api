package opencode

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"work2api/internal/core/protocol"
)

func TestAnonymousOpaqueToolNamesPreserveContract(t *testing.T) {
	for _, proto := range []protocol.Protocol{protocol.Chat, protocol.Responses, protocol.Anthropic} {
		var defs []any
		for _, name := range []string{"ReportFindings", "ScheduleWakeup"} {
			tool := map[string]any{"type": "function", "name": name, "description": "Must preserve instructions", "parameters": map[string]any{"type": "object", "required": []any{"argument"}}}
			if proto == protocol.Chat {
				tool = map[string]any{"type": "function", "function": tool}
			} else if proto == protocol.Anthropic {
				tool["input_schema"] = tool["parameters"]
				delete(tool, "parameters")
				delete(tool, "type")
			}
			defs = append(defs, tool)
		}
		choice := map[string]any{"type": "function", "name": "ReportFindings"}
		if proto == protocol.Chat {
			choice = map[string]any{"type": "function", "function": map[string]any{"name": "ReportFindings"}}
		} else if proto == protocol.Anthropic {
			choice["type"] = "tool"
		}
		body := map[string]any{"tools": defs, "tool_choice": choice}
		before, _ := json.Marshal(body)
		out, names := aliasClientTools(body, proto, true, true)
		if len(names) != 2 {
			t.Fatal("not all tools aliased")
		}
		selected := out["tool_choice"].(map[string]any)
		if proto == protocol.Chat {
			selected = selected["function"].(map[string]any)
		}
		if names[selected["name"].(string)] != "ReportFindings" {
			t.Fatal("forced tool choice lost")
		}
		for alias, name := range names {
			frame, _ := json.Marshal(map[string]any{"type": "response.output_item.added", "item": map[string]any{"type": "function_call", "name": alias, "arguments": "{}"}})
			w := httptest.NewRecorder()
			w.Header().Set("Content-Type", "text/event-stream")
			writer := &toolAliasWriter{ResponseWriter: w, names: names}
			for _, b := range []byte("data: " + string(frame) + "\n\n") {
				if _, err := writer.Write([]byte{b}); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Finish(); err != nil || !strings.Contains(w.Body.String(), name) || strings.Contains(w.Body.String(), alias) {
				t.Fatal("stream alias restoration failed", err)
			}
		}
		raw, _ := json.Marshal(out["tools"])
		if strings.Contains(string(raw), "ReportFindings") || strings.Contains(string(raw), "ScheduleWakeup") || !strings.Contains(string(raw), "Must preserve instructions") {
			t.Fatal(string(raw))
		}
		after, _ := json.Marshal(body)
		if string(before) != string(after) {
			t.Fatal("caller request mutated")
		}
		for alias, name := range names {
			response := map[string]any{"type": "function_call", "name": alias, "arguments": `{"name":"ReportFindings"}`}
			if !restoreToolNames(response, names) || response["name"] != name || response["arguments"] != `{"name":"ReportFindings"}` {
				t.Fatal(response)
			}
		}
		_, privateNames := aliasClientTools(body, proto, false, false)
		if len(privateNames) != 0 {
			t.Fatal("paid tool contract changed")
		}
	}
}

func TestClientToolPrefixDoesNotCollideWithReservedTool(t *testing.T) {
	body := map[string]any{"tools": []any{
		map[string]any{"type": "function", "function": map[string]any{"name": "she"}},
		map[string]any{"type": "function", "function": map[string]any{"name": "other"}},
	}}
	out, names := aliasClientTools(body, protocol.Chat, false, true)
	alias := anonymousToolName(protocol.Chat, out["tools"].([]any)[0])
	if len(names) != 1 || names[alias] != "she" || anonymousToolName(protocol.Chat, out["tools"].([]any)[1]) != "other" {
		t.Fatal("client tool prefix not isolated", names)
	}
	writer := &toolAliasWriter{names: names, blocked: map[string]bool{"shell": true, "she": true}}
	frame := []byte(fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":%q,"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, alias))
	if got := writer.rewrite(frame); !strings.Contains(string(got), `"name":"she"`) || writer.Finish() != nil {
		t.Fatal("client tool not restored", string(got), writer.Finish())
	}
	writer.rewrite([]byte(`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"shell","arguments":"{}"}}]}}]}`))
	if writer.Finish() == nil {
		t.Fatal("reserved tool became executable")
	}
	if _, privateNames := aliasClientTools(body, protocol.Chat, false, false); len(privateNames) != 0 {
		t.Fatal("paid route aliased non-conflicting names", privateNames)
	}
}

func TestInterleavedAliasArgumentsAcrossStreamAndCollapse(t *testing.T) {
	frames := []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"w2a_","arguments":"{\"a\":"}},{"index":1,"id":"b","function":{"name":"w2a_beta","arguments":"{\"b\":2}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"alpha","arguments":"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":3}}`,
		"[DONE]",
	}
	var wire strings.Builder
	for _, frame := range frames {
		wire.WriteString("data: " + frame + "\n\n")
	}
	makeWriter := func() *toolAliasWriter {
		return &toolAliasWriter{names: map[string]string{"w2a_alpha": "Read", "w2a_beta": "Lookup"}}
	}
	for _, target := range []protocol.Protocol{protocol.Anthropic, protocol.Responses} {
		w := httptest.NewRecorder()
		a := makeWriter()
		usage, known, err := protocol.TranscodeStreamWithRewrite(t.Context(), w, strings.NewReader(wire.String()), protocol.Chat, target, "fixture", a.rewriteEvent)
		body := w.Body.String()
		if err != nil || !known || usage.Input != 4 || !strings.Contains(body, `"name":"Read"`) || !strings.Contains(body, `"name":"Lookup"`) || !strings.Contains(body, `{\"a\":1}`) || !strings.Contains(body, `{\"b\":2}`) || strings.Contains(body, "w2a_") {
			t.Fatal(target, body, usage, known, err)
		}
	}
	a := makeWriter()
	body, usage, known, err := protocol.CollapseStreamWithRewrite(t.Context(), strings.NewReader(wire.String()), protocol.Chat, "fixture", a.rewriteEvent)
	if err != nil || !known || usage.Output != 3 || !strings.Contains(string(body), `"name":"Read"`) || !strings.Contains(string(body), `{\"a\":1}`) {
		t.Fatal(string(body), usage, known, err)
	}
}
