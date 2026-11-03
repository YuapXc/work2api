package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"work2api/internal/jsonutil"
)

// PrepareCustomTools preserves Responses free-text tools across function-only
// providers. The adaptation is scoped to this request, never a global catalog.
func PrepareCustomTools(body map[string]any) (map[string]any, map[string]bool, error) {
	names := map[string]bool{}
	functions := map[string]bool{}
	needs := false
	for _, raw := range jsonutil.SliceAt(body, "tools") {
		tool, _ := raw.(map[string]any)
		if jsonutil.StringAt(tool, "type") == "function" {
			functions[jsonutil.FirstString(jsonutil.StringAt(tool, "name"), jsonutil.StringAt(tool, "function", "name"))] = true
		}
		if jsonutil.StringAt(tool, "type") == "custom" {
			name := jsonutil.StringAt(tool, "name")
			if strings.TrimSpace(name) == "" || names[name] {
				return nil, nil, fmt.Errorf("custom 工具必须指定唯一 name")
			}
			names[name] = true
		}
	}
	for name := range names {
		if functions[name] {
			return nil, nil, fmt.Errorf("custom 与 function 工具名称不得冲突: %s", name)
		}
	}
	for _, raw := range jsonutil.SliceAt(body, "input") {
		item, _ := raw.(map[string]any)
		if jsonutil.StringAt(item, "type") == "custom_tool_call" {
			needs = true
			if name := jsonutil.StringAt(item, "name"); name != "" {
				if !functions[name] {
					names[name] = true
				}
			} else {
				return nil, nil, fmt.Errorf("custom_tool_call 必须指定 name")
			}
		}
		needs = needs || jsonutil.StringAt(item, "type") == "custom_tool_call_output"
	}
	if len(names) == 0 && !needs {
		return body, nil, nil
	}
	// Copy only rewritten maps; do not serialize large conversation histories.
	out := copyCustomMap(body)
	for _, key := range []string{"tools", "input"} {
		if items, ok := body[key].([]any); ok {
			var cloned []any
			for i, raw := range items {
				item, ok := raw.(map[string]any)
				kind := jsonutil.StringAt(item, "type")
				if ok && (key == "tools" && kind == "custom" || key == "input" && (kind == "custom_tool_call" || kind == "custom_tool_call_output")) {
					if cloned == nil {
						cloned = append([]any(nil), items...)
					}
					cloned[i] = copyCustomMap(item)
				}
			}
			if cloned != nil {
				out[key] = cloned
			}
		}
	}
	if choice, ok := body["tool_choice"].(map[string]any); ok && jsonutil.StringAt(choice, "type") == "custom" {
		out["tool_choice"] = copyCustomMap(choice)
	}
	for _, raw := range jsonutil.SliceAt(out, "tools") {
		tool, _ := raw.(map[string]any)
		name := jsonutil.StringAt(tool, "name")
		if jsonutil.StringAt(tool, "type") != "custom" {
			if names[name] {
				return nil, nil, fmt.Errorf("custom 与 function 工具名称不得冲突: %s", name)
			}
			continue
		}
		description := jsonutil.StringAt(tool, "description") + "\nPass the complete raw tool input as the input string, unchanged."
		if format := jsonutil.MapAt(tool, "format"); jsonutil.StringAt(format, "type") == "grammar" {
			description += "\nThe input must follow this " + jsonutil.StringAt(format, "syntax") + " grammar:\n" + jsonutil.StringAt(format, "definition")
		}
		delete(tool, "format")
		tool["type"] = "function"
		tool["description"] = description
		tool["strict"] = true
		tool["parameters"] = map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}, "required": []any{"input"}, "additionalProperties": false}
	}
	for _, raw := range jsonutil.SliceAt(out, "input") {
		item, _ := raw.(map[string]any)
		switch jsonutil.StringAt(item, "type") {
		case "custom_tool_call":
			input, ok := item["input"].(string)
			if !ok {
				return nil, nil, fmt.Errorf("custom_tool_call.input 必须为字符串")
			}
			args, _ := json.Marshal(map[string]any{"input": input})
			item["type"], item["arguments"] = "function_call", string(args)
			delete(item, "input")
		case "custom_tool_call_output":
			item["type"] = "function_call_output"
		}
	}
	if choice := jsonutil.MapAt(out, "tool_choice"); jsonutil.StringAt(choice, "type") == "custom" {
		choice["type"] = "function"
	}
	return out, names, nil
}

func copyCustomMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

// CustomToolWriter restores the client-facing native tool type. Arguments are
// released only after the complete JSON wrapper validates, so malformed model
// output can never become an executable free-text tool call.
type CustomToolWriter struct {
	http.ResponseWriter
	names          map[string]bool
	namespaces     map[string]NamespaceName
	items          map[string]bool
	delivered      map[string]bool
	buffer         bytes.Buffer
	limit          int
	status         int
	committed      bool
	sequence       int
	failure        error
	responseID     string
	responseLength int
}

func NewCustomToolWriter(w http.ResponseWriter, names map[string]bool, limit int) *CustomToolWriter {
	if limit <= 0 {
		limit = 32 << 20
	}
	return &CustomToolWriter{ResponseWriter: w, names: names, items: map[string]bool{}, delivered: map[string]bool{}, limit: limit}
}

func NewClientToolWriter(w http.ResponseWriter, names map[string]bool, namespaces map[string]NamespaceName, limit int) *CustomToolWriter {
	writer := NewCustomToolWriter(w, names, limit)
	writer.namespaces = namespaces
	return writer
}

func (w *CustomToolWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *CustomToolWriter) WriteHeader(status int) {
	if w.status == 0 && status >= 200 {
		w.status = status
	}
}
func (w *CustomToolWriter) stream() bool {
	return strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream")
}
func (w *CustomToolWriter) commit() {
	if !w.committed {
		w.Header().Del("Content-Length")
		if w.responseLength > 0 && !w.stream() {
			w.Header().Set("Content-Length", strconv.Itoa(w.responseLength))
		}
		status := w.status
		if status == 0 {
			status = http.StatusOK
		}
		w.ResponseWriter.WriteHeader(status)
		w.committed = true
	}
}
func (w *CustomToolWriter) Write(p []byte) (int, error) {
	if w.failure != nil {
		return len(p), nil
	}
	if w.buffer.Len()+len(p) > w.limit {
		return 0, w.fail(fmt.Errorf("custom 工具响应超过缓冲上限"))
	}
	w.buffer.Write(p)
	if !w.stream() {
		return len(p), nil
	}
	for {
		data := w.buffer.Bytes()
		index, width := bytes.Index(data, []byte("\n\n")), 2
		if cr := bytes.Index(data, []byte("\r\n\r\n")); cr >= 0 && (index < 0 || cr < index) {
			index, width = cr, 4
		}
		if index < 0 {
			break
		}
		frame := data[:index]
		var event map[string]any
		var fields strings.Builder
		for _, line := range bytes.Split(frame, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				fields.Write(bytes.TrimSpace(line[5:]))
				fields.WriteByte('\n')
			}
		}
		if payload := strings.TrimSpace(fields.String()); payload != "" && payload != "[DONE]" {
			if err := json.Unmarshal([]byte(payload), &event); err != nil {
				return 0, w.fail(err)
			}
		}
		if event != nil {
			events, err := w.restoreEvent(event)
			if err != nil {
				return 0, w.fail(err)
			}
			for _, e := range events {
				e["sequence_number"] = w.sequence
				w.sequence++
				data, err := json.Marshal(e)
				if err != nil {
					return 0, err
				}
				w.commit()
				if _, err := w.ResponseWriter.Write(append(append([]byte("data: "), data...), '\n', '\n')); err != nil {
					return 0, err
				}
			}
		} else if bytes.HasPrefix(frame, []byte(":")) {
			// Preserve heartbeat comments without inventing model progress.
			w.commit()
			if _, err := w.ResponseWriter.Write(data[:index+width]); err != nil {
				return 0, err
			}
		}
		w.buffer.Next(index + width)
	}
	return len(p), nil
}
func (w *CustomToolWriter) Flush() {
	if w.committed {
		if f, ok := w.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
	}
}
func (w *CustomToolWriter) Finish() error {
	if w.failure != nil {
		return w.failure
	}
	if w.stream() {
		if len(bytes.TrimSpace(w.buffer.Bytes())) > 0 {
			return w.fail(fmt.Errorf("custom 工具响应流不完整"))
		}
		return nil
	}
	if w.buffer.Len() == 0 {
		return nil
	}
	var response map[string]any
	if err := json.Unmarshal(w.buffer.Bytes(), &response); err != nil {
		return err
	}
	if err := w.restoreOutput(response); err != nil {
		return err
	}
	data, err := json.Marshal(response)
	if err != nil {
		return err
	}
	w.responseLength = len(data)
	w.commit()
	_, err = w.ResponseWriter.Write(data)
	return err
}

func (w *CustomToolWriter) fail(err error) error {
	w.failure = err
	w.buffer.Reset()
	if w.stream() {
		data, _ := json.Marshal(map[string]any{"type": "response.failed", "sequence_number": w.sequence, "response": map[string]any{"id": w.responseID, "object": "response", "status": "failed", "output": []any{}, "error": map[string]any{"code": "invalid_tool_arguments", "message": err.Error()}}})
		w.commit()
		_, _ = w.ResponseWriter.Write(append(append([]byte("data: "), data...), '\n', '\n'))
		w.Flush()
	}
	return err
}
func customInput(arguments any) (string, error) {
	text, ok := arguments.(string)
	if !ok {
		return "", fmt.Errorf("custom 工具包装参数必须为 JSON 字符串")
	}
	var args map[string]any
	if json.Unmarshal([]byte(text), &args) != nil || len(args) != 1 {
		return "", fmt.Errorf("custom 工具包装参数无效")
	}
	input, ok := args["input"].(string)
	if !ok {
		return "", fmt.Errorf("custom 工具 input 必须为字符串")
	}
	return input, nil
}
func (w *CustomToolWriter) restoreItem(item map[string]any, added bool) error {
	if jsonutil.StringAt(item, "type") != "function_call" {
		return nil
	}
	name := jsonutil.StringAt(item, "name")
	defer func() {
		if original, ok := w.namespaces[name]; ok {
			item["name"], item["namespace"] = original.Name, original.Namespace
		}
	}()
	if !w.names[name] {
		return nil
	}
	input := ""
	if !added {
		var err error
		input, err = customInput(item["arguments"])
		if err != nil {
			return err
		}
	}
	item["type"], item["input"] = "custom_tool_call", input
	delete(item, "arguments")
	delete(item, "status")
	return nil
}
func (w *CustomToolWriter) restoreOutput(response map[string]any) error {
	for _, raw := range jsonutil.SliceAt(response, "output") {
		item, _ := raw.(map[string]any)
		if err := w.restoreItem(item, false); err != nil {
			return err
		}
	}
	return nil
}
func (w *CustomToolWriter) inputEvents(id string, index any, input string) []map[string]any {
	w.delivered[id] = true
	return []map[string]any{
		{"type": "response.custom_tool_call_input.delta", "item_id": id, "output_index": index, "delta": input},
		{"type": "response.custom_tool_call_input.done", "item_id": id, "output_index": index, "input": input},
	}
}
func (w *CustomToolWriter) restoreEvent(event map[string]any) ([]map[string]any, error) {
	events := []map[string]any{}
	id := jsonutil.StringAt(event, "item_id")
	switch jsonutil.StringAt(event, "type") {
	case "response.created":
		w.responseID = jsonutil.StringAt(event, "response", "id")
	case "response.output_item.added", "response.output_item.done":
		item := jsonutil.MapAt(event, "item")
		custom := jsonutil.StringAt(item, "type") == "function_call" && w.names[jsonutil.StringAt(item, "name")]
		added := event["type"] == "response.output_item.added"
		if custom {
			id = jsonutil.StringAt(item, "id")
			w.items[id] = true
			if err := w.restoreItem(item, added); err != nil {
				return nil, err
			}
			if !added && !w.delivered[id] {
				events = append(events, w.inputEvents(id, event["output_index"], jsonutil.StringAt(item, "input"))...)
			}
		} else if err := w.restoreItem(item, added); err != nil {
			return nil, err
		}
	case "response.function_call_arguments.delta":
		if w.items[id] {
			return nil, nil
		}
	case "response.function_call_arguments.done":
		if w.items[id] {
			input, err := customInput(event["arguments"])
			if err != nil {
				return nil, err
			}
			if w.delivered[id] {
				return nil, nil
			}
			return w.inputEvents(id, event["output_index"], input), nil
		}
	case "response.completed", "response.incomplete":
		if err := w.restoreOutput(jsonutil.MapAt(event, "response")); err != nil {
			return nil, err
		}
	}
	return append(events, event), nil
}
