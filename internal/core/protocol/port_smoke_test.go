package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"work2api/internal/jsonutil"
)

func TestCustomToolsRoundTrip(t *testing.T) {
	patch := "*** Begin Patch\n*** Add File: hello.txt\n+你好\\world\n*** End Patch"
	body := map[string]any{
		"tools":       []any{map[string]any{"type": "custom", "name": "apply_patch", "format": map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: PATCH"}}, map[string]any{"type": "function", "name": "shell", "parameters": map[string]any{"type": "object"}}},
		"tool_choice": map[string]any{"type": "custom", "name": "apply_patch"},
		"input":       []any{map[string]any{"type": "custom_tool_call", "id": "ctc_1", "call_id": "call_1", "name": "apply_patch", "input": patch}, map[string]any{"type": "custom_tool_call_output", "call_id": "call_1", "output": "success"}},
	}
	before, _ := json.Marshal(body)
	prepared, names, err := PrepareCustomTools(body)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := json.Marshal(body)
	if !bytes.Equal(before, after) {
		t.Fatal("custom preparation mutated client request")
	}
	tool := jsonutil.SliceAt(prepared, "tools")[0].(map[string]any)
	if tool["type"] != "function" || !strings.Contains(jsonutil.StringAt(tool, "description"), "start: PATCH") || jsonutil.StringAt(prepared, "tool_choice", "type") != "function" {
		t.Fatal("custom tool definition/choice lost", prepared)
	}
	history := jsonutil.SliceAt(prepared, "input")
	if history[0].(map[string]any)["type"] != "function_call" || history[1].(map[string]any)["type"] != "function_call_output" {
		t.Fatal("custom history not adapted")
	}
	args := history[0].(map[string]any)["arguments"]
	if input, err := customInput(args); err != nil || input != patch {
		t.Fatal("tool input changed", input, err)
	}
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", streaming), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			w := NewCustomToolWriter(recorder, names, 1<<20)
			item := map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "apply_patch", "arguments": args, "status": "completed"}
			response := map[string]any{"id": "resp_1", "status": "completed", "output": []any{item, map[string]any{"type": "function_call", "id": "fc_2", "call_id": "call_2", "name": "shell", "arguments": "{}"}}}
			if streaming {
				w.Header().Set("Content-Type", "text/event-stream")
				events := []map[string]any{
					{"type": "response.created", "response": map[string]any{"id": "resp_1"}},
					{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "apply_patch", "arguments": ""}},
					{"type": "response.function_call_arguments.delta", "item_id": "fc_1", "output_index": 0, "delta": args},
					{"type": "response.function_call_arguments.done", "item_id": "fc_1", "output_index": 0, "arguments": args},
					{"type": "response.output_item.done", "output_index": 0, "item": item},
					{"type": "response.completed", "response": response},
				}
				for _, event := range events {
					data, _ := json.Marshal(event)
					frame := append(append([]byte("event: ignored\r\ndata: "), data...), '\r', '\n', '\r', '\n')
					for _, b := range frame {
						if _, err := w.Write([]byte{b}); err != nil {
							t.Fatal(err)
						}
					}
					w.Flush()
				}
			} else {
				w.Header().Set("Content-Type", "application/json")
				data, _ := json.Marshal(response)
				w.Header().Set("Content-Length", "99999")
				if _, err := w.Write(data); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.Finish(); err != nil {
				t.Fatal(err)
			}
			if recorder.Header().Get("Content-Length") != "" {
				t.Fatal("stale content length")
			}
			if streaming {
				seq := 0
				deltas := 0
				if err := readSSE(strings.NewReader(recorder.Body.String()), func(_ string, data string) error {
					var event map[string]any
					if err := json.Unmarshal([]byte(data), &event); err != nil {
						return err
					}
					if jsonutil.IntAt(event, "sequence_number") != seq {
						t.Fatal("sequence changed", event)
					}
					seq++
					if event["type"] == "response.custom_tool_call_input.delta" {
						deltas++
						if event["delta"] != patch {
							t.Fatal("custom text changed")
						}
					}
					if event["type"] == "response.completed" {
						response = jsonutil.MapAt(event, "response")
					}
					if strings.HasPrefix(jsonutil.StringAt(event, "type"), "response.function_call_arguments") {
						t.Fatal("wrapper arguments leaked")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if deltas != 1 {
					t.Fatal("custom input duplicated or lost", deltas)
				}
			} else {
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
			}
			output := jsonutil.SliceAt(response, "output")
			if jsonutil.StringAt(output[0].(map[string]any), "input") != patch || output[0].(map[string]any)["type"] != "custom_tool_call" || output[1].(map[string]any)["type"] != "function_call" {
				t.Fatal("restored output incorrect", response)
			}
		})
	}
}

func TestCustomToolsRejectMalformedInput(t *testing.T) {
	for _, args := range []string{`{"input":7}`, `{"input":"patch","extra":1}`, `{"input":`} {
		recorder := httptest.NewRecorder()
		w := NewCustomToolWriter(recorder, map[string]bool{"apply_patch": true}, 1<<20)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []map[string]any{
			{"type": "response.output_item.added", "item": map[string]any{"type": "function_call", "id": "fc_1", "name": "apply_patch"}},
			{"type": "response.function_call_arguments.done", "item_id": "fc_1", "arguments": args},
		} {
			data, _ := json.Marshal(event)
			_, _ = w.Write(append(append([]byte("data: "), data...), '\n', '\n'))
		}
		if w.Finish() == nil || !strings.Contains(recorder.Body.String(), `"type":"response.failed"`) || strings.Contains(recorder.Body.String(), `"type":"response.custom_tool_call_input.done"`) {
			t.Fatal("invalid input became executable", recorder.Body.String())
		}
	}
}

func TestRefusalSurvivesCrossProtocolConversion(t *testing.T) {
	for _, from := range []Protocol{Chat, Responses} {
		var body []byte
		var stream string
		if from == Chat {
			body = []byte(`{"choices":[{"message":{"role":"assistant","content":null,"refusal":"Cannot comply."},"finish_reason":"stop"}]}`)
			stream = "data: {\"choices\":[{\"delta\":{\"refusal\":\"Cannot comply.\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		} else {
			body = []byte(`{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"Cannot comply."}]}]}`)
			stream = "data: {\"type\":\"response.refusal.delta\",\"delta\":\"Cannot comply.\"}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\"}}\n\n"
		}
		converted, err := ConvertResponse(from, Anthropic, body)
		if err != nil || !bytes.Contains(converted, []byte("Cannot comply.")) {
			t.Fatal("refusal response lost", string(converted), err)
		}
		recorder := httptest.NewRecorder()
		if err := transcodeStream(recorder, strings.NewReader(stream), from, Anthropic, "m"); err != nil || !strings.Contains(recorder.Body.String(), "Cannot comply.") {
			t.Fatal("refusal stream lost", recorder.Body.String(), err)
		}
	}
}

// TestConvertRequestChatToAnthropic verifies the ported conversion layer wires
// up: an OpenAI Chat request should transcode into an Anthropic Messages body
// with system split out and messages preserved.
func TestConvertRequestChatToAnthropic(t *testing.T) {
	in := map[string]any{
		"model": "claude-x",
		"messages": []any{
			map[string]any{"role": "system", "content": "be brief"},
			map[string]any{"role": "user", "content": "hello"},
		},
		"max_tokens": float64(64),
	}
	out, err := ConvertRequest(Chat, Anthropic, in)
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	if got := jsonutil.StringAt(out, "system"); got != "be brief" {
		// system may be represented as string or blocks depending on impl;
		// accept either as long as the text survives.
		if blocks := jsonutil.SliceAt(out, "system"); len(blocks) == 0 {
			t.Fatalf("system prompt lost: %v", out["system"])
		}
	}
	msgs := jsonutil.SliceAt(out, "messages")
	if len(msgs) == 0 {
		t.Fatalf("messages lost: %v", out["messages"])
	}
	b, _ := json.Marshal(out)
	t.Logf("anthropic body: %s", b)
}

// TestConvertRequestSameProtocolClones ensures a same-protocol request is
// preserved (not dropped) through PrepareRequest.
func TestConvertRequestSameProtocolClones(t *testing.T) {
	in := map[string]any{
		"model":    "gpt-x",
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	}
	out, err := PrepareRequest(Chat, Chat, in, "https://example.test/v1/chat/completions")
	if err != nil {
		t.Fatalf("PrepareRequest: %v", err)
	}
	if jsonutil.StringAt(out, "model") != "gpt-x" {
		t.Fatalf("model lost: %v", out)
	}
}

func TestResponsesCompletedReusesFinishedItems(t *testing.T) {
	var wire bytes.Buffer
	emitter := newBridgeStreamEmitter(&wire, httptest.NewRecorder(), Responses, "test-model")
	for _, event := range []bridgeStreamEvent{{Kind: "reasoning", Encrypted: "encrypted-state"}, {Kind: "tool_start", ToolKey: "one", ToolID: "call1", ToolName: "lookup"}, {Kind: "tool_delta", ToolKey: "one", Text: `{"q":"test"}`}, {Kind: "text", Text: "answer"}, {Kind: "finish", Stop: "tool_calls"}, {Kind: "done"}} {
		if err := emitter.Emit(event); err != nil {
			t.Fatal(err)
		}
	}
	finished := map[int]any{}
	var completed []any
	err := readSSE(strings.NewReader(wire.String()), func(name, data string) error {
		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return err
		}
		if name == "response.output_item.done" {
			finished[int(event["output_index"].(float64))] = event["item"]
		}
		if name == "response.completed" {
			completed = event["response"].(map[string]any)["output"].([]any)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 3 || len(finished) != 3 {
		t.Fatalf("missing output items: %s", wire.String())
	}
	for index, item := range completed {
		if !reflect.DeepEqual(item, finished[index]) {
			t.Fatalf("completed item %d changed identity/content", index)
		}
	}
	if completed[0].(map[string]any)["encrypted_content"] != "encrypted-state" {
		t.Fatal("encrypted reasoning lost")
	}
}

func TestCacheUnknownAndObservedZeroRemainDistinct(t *testing.T) {
	for _, target := range []Protocol{Chat, Responses, Anthropic} {
		for _, known := range []bool{false, true} {
			response := bridgeResponse{Usage: Usage{Input: 10, Output: 2, Total: 12, CachedKnown: known}}
			doc := encodeBridgeResponse(target, response)
			usage := doc["usage"].(map[string]any)
			// Every detail object here is optional and describes an observation.
			// An unreported count omits the object instead of claiming zero:
			// Codex decodes the Responses detail objects as complete structures
			// and rejects one without cached_tokens.
			container, key := "prompt_tokens_details", "cached_tokens"
			switch target {
			case Anthropic:
				container, key = "", "cache_read_input_tokens"
			case Responses:
				container = "input_tokens_details"
			}
			var cached any
			if container == "" {
				cached = usage[key]
			} else {
				details, present := usage[container].(map[string]any)
				if present != known {
					t.Fatalf("%s cache known=%v, details=%v", target, known, usage[container])
				}
				cached = details[key]
			}
			if (cached != nil) != known {
				t.Fatalf("%s cached known=%v, value=%v", target, known, cached)
			}
		}
	}
}

// Codex advertises tool groups the Chat/Anthropic lanes cannot express
// (namespace bundles and web_search) next to its function tools. Those entries
// must be skipped so the request stays convertible, while every function tool
// survives with its schema intact.
func TestConvertRequestSkipsUnsupportedToolTypes(t *testing.T) {
	body := map[string]any{
		"model": "trae/m",
		"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}}},
		"tools": []any{
			map[string]any{"type": "function", "name": "exec_command", "description": "run", "parameters": map[string]any{"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}}},
			map[string]any{"type": "namespace", "name": "multi_agent_v1", "description": "agents", "tools": []any{
				map[string]any{"type": "function", "name": "spawn_agent", "parameters": map[string]any{"type": "object"}},
			}},
			map[string]any{"type": "web_search", "external_web_access": false},
			map[string]any{"type": "function", "name": "view_image", "parameters": map[string]any{"type": "object"}},
		},
		"tool_choice": "auto",
	}
	out, err := ConvertRequest(Responses, Chat, body)
	if err != nil {
		t.Fatalf("Codex tool set rejected: %v", err)
	}
	tools, _ := out["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("expected only function tools to survive, got %v", tools)
	}
	names := []string{}
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		function, _ := tool["function"].(map[string]any)
		names = append(names, jsonutil.StringAt(function, "name"))
	}
	if len(names) != 2 || names[0] != "exec_command" || names[1] != "view_image" {
		t.Fatalf("function tools lost or reordered: %v", names)
	}
}

// Codex aborts the whole turn with "failed to parse ResponseCompleted: missing
// field `cached_tokens`" when Responses usage carries an empty
// input_tokens_details, which is exactly what an upstream without cache
// reporting (Trae) used to produce. Unreported detail objects must be absent.
func TestResponsesUsageOmitsUnreportedDetails(t *testing.T) {
	doc := encodeBridgeResponse(Responses, bridgeResponse{Usage: Usage{Input: 5, Output: 2, Total: 7}})
	usage := doc["usage"].(map[string]any)
	if _, present := usage["input_tokens_details"]; present {
		t.Fatalf("empty input_tokens_details emitted: %v", usage)
	}
	if _, present := usage["output_tokens_details"]; present {
		t.Fatalf("unreported reasoning detail emitted: %v", usage)
	}
	if usage["input_tokens"] != 5 || usage["output_tokens"] != 2 || usage["total_tokens"] != 7 {
		t.Fatalf("token totals changed: %v", usage)
	}

	observed := encodeBridgeResponse(Responses, bridgeResponse{Usage: Usage{Input: 5, Output: 2, Total: 7, CachedKnown: true, ReasoningKnown: true}})
	observedUsage := observed["usage"].(map[string]any)
	if observedUsage["input_tokens_details"].(map[string]any)["cached_tokens"] != 0 {
		t.Fatalf("observed zero cache dropped: %v", observedUsage)
	}
	if observedUsage["output_tokens_details"].(map[string]any)["reasoning_tokens"] != 0 {
		t.Fatalf("observed zero reasoning dropped: %v", observedUsage)
	}
}

func TestCollapseDoesNotInventMissingUsage(t *testing.T) {
	wire := `data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n" + "data: [DONE]\n\n"
	body, usage, known, err := CollapseStreamWithUsage(strings.NewReader(wire), Chat, "test")
	if err != nil || known {
		t.Fatal("unexpected usage observation", known, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["usage"]; ok {
		t.Fatal("synthetic usage exposed as observed", string(body))
	}
	if usage.FinishReason != "stop" {
		t.Fatalf("finish lost without usage: %+v", usage)
	}
	for _, target := range []Protocol{Chat, Anthropic, Responses} {
		for _, reason := range []string{"stop", "length", "tool_calls", "content_filter"} {
			stream := strings.Replace(wire, `"finish_reason":"stop"`, `"finish_reason":"`+reason+`"`, 1)
			w := httptest.NewRecorder()
			got, reported, err := TranscodeStream(t.Context(), w, strings.NewReader(stream), Chat, target, "test")
			if err != nil || reported || got.FinishReason != reason {
				t.Fatalf("%s/%s finish=%q known=%v err=%v", target, reason, got.FinishReason, reported, err)
			}
			single, collapsed, reported, err := CollapseStreamWithUsage(strings.NewReader(stream), Chat, "test")
			if err != nil || reported || collapsed.FinishReason != reason {
				t.Fatalf("collapse %s: %+v %v", reason, collapsed, err)
			}
			_ = single
			got, reported = ResponseUsage(Chat, []byte(`{"choices":[{"finish_reason":"`+reason+`"}]}`))
			if reported || got.FinishReason != reason {
				t.Fatalf("response %s: %+v", reason, got)
			}
		}
	}
}
