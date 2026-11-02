package opencode

import (
	"encoding/json"
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
		out, names := aliasClientTools(body, proto, true)
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
		_, privateNames := aliasClientTools(body, proto)
		if len(privateNames) != 0 {
			t.Fatal("paid tool contract changed")
		}
	}
}
