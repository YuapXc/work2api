package opencode

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"work2api/internal/core/protocol"
	"work2api/internal/jsonutil"
)

// Tool definitions from jasonxu114514/opencode2api a6601da and the OpenCode CLI.
//
//go:embed opencode_tools.json
var officialToolJSON []byte
var officialTools = func() map[string]struct {
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
} {
	var definitions map[string]struct {
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	}
	if err := json.Unmarshal(officialToolJSON, &definitions); err != nil {
		panic(err)
	}
	return definitions
}()

// Alias only declared conflicting client tools. Schemas and tool argument
// strings are opaque; never rewrite them or replace them with official schemas.
func aliasClientTools(input map[string]any, proto protocol.Protocol, anonymous ...bool) (map[string]any, map[string]string) {
	forward, reverse := map[string]string{}, map[string]string{}
	used := map[string]bool{}
	for _, raw := range jsonutil.SliceAt(input, "tools") {
		used[anonymousToolName(proto, raw)] = true
	}
	for name := range used {
		if _, known := officialTools[name]; !known && name != "bash" && !(len(anonymous) > 0 && anonymous[0]) {
			continue
		}
		sum := sha256.Sum256([]byte("opencode-client-tool\x00" + name))
		alias := "w2a_" + hex.EncodeToString(sum[:12])
		for used[alias] {
			alias += "_"
		}
		forward[name], reverse[alias] = alias, name
	}
	if len(forward) == 0 {
		return input, nil
	}
	out := copyAliasMap(input)
	tools := append([]any(nil), jsonutil.SliceAt(input, "tools")...)
	out["tools"] = tools
	rename := func(obj map[string]any) {
		if name := jsonutil.StringAt(obj, "name"); forward[name] != "" {
			obj["name"] = forward[name]
		}
	}
	for i, raw := range tools {
		tool, _ := raw.(map[string]any)
		tool = copyAliasMap(tool)
		tools[i] = tool
		if proto == protocol.Chat {
			fn := copyAliasMap(jsonutil.MapAt(tool, "function"))
			tool["function"] = fn
			rename(fn)
		} else {
			rename(tool)
		}
	}
	choice := copyAliasMap(jsonutil.MapAt(input, "tool_choice"))
	if choice != nil {
		out["tool_choice"] = choice
		if fn := jsonutil.MapAt(choice, "function"); fn != nil {
			choice["function"] = copyAliasMap(fn)
		}
	}
	if proto == protocol.Chat {
		rename(jsonutil.MapAt(choice, "function"))
	} else {
		rename(choice)
	}
	allowed := jsonutil.SliceAt(choice, "tools")
	if allowed != nil {
		allowed = append([]any(nil), allowed...)
		choice["tools"] = allowed
	}
	for i, raw := range allowed {
		tool, _ := raw.(map[string]any)
		tool = copyAliasMap(tool)
		allowed[i] = tool
		if fn := jsonutil.MapAt(tool, "function"); fn != nil {
			tool["function"] = copyAliasMap(fn)
		}
		rename(tool)
		rename(jsonutil.MapAt(tool, "function"))
	}
	if subset := jsonutil.MapAt(choice, "allowed_tools"); subset != nil {
		subset = copyAliasMap(subset)
		choice["allowed_tools"] = subset
		tools := append([]any(nil), jsonutil.SliceAt(subset, "tools")...)
		subset["tools"] = tools
		for i, raw := range tools {
			tool, _ := raw.(map[string]any)
			tool = copyAliasMap(tool)
			tools[i] = tool
			if fn := jsonutil.MapAt(tool, "function"); fn != nil {
				tool["function"] = copyAliasMap(fn)
			}
			rename(tool)
			rename(jsonutil.MapAt(tool, "function"))
		}
	}
	for _, field := range []string{"messages", "input"} {
		items, ok := input[field].([]any)
		if !ok {
			continue
		}
		items = append([]any(nil), items...)
		out[field] = items
		for i, raw := range items {
			item, _ := raw.(map[string]any)
			if item == nil {
				continue
			}
			item = copyAliasMap(item)
			items[i] = item
			if item["type"] == "function_call" {
				rename(item)
			}
			calls := jsonutil.SliceAt(item, "tool_calls")
			if calls != nil {
				calls = append([]any(nil), calls...)
				item["tool_calls"] = calls
			}
			for j, raw := range calls {
				call, _ := raw.(map[string]any)
				call = copyAliasMap(call)
				calls[j] = call
				if fn := jsonutil.MapAt(call, "function"); fn != nil {
					call["function"] = copyAliasMap(fn)
				}
				rename(jsonutil.MapAt(call, "function"))
			}
			content := jsonutil.SliceAt(item, "content")
			if content != nil {
				content = append([]any(nil), content...)
				item["content"] = content
			}
			for j, raw := range content {
				block, _ := raw.(map[string]any)
				if block["type"] == "tool_use" {
					block = copyAliasMap(block)
					content[j] = block
					rename(block)
				}
			}
		}
	}
	return out, reverse
}

// Visit only protocol-defined tool names, never text, schemas or arguments.
func visitToolNames(obj map[string]any, visit func(map[string]any)) {
	item := func(value map[string]any) {
		if value["type"] == "function_call" || value["type"] == "tool_use" {
			visit(value)
		}
	}
	item(obj)
	item(jsonutil.MapAt(obj, "item"))
	item(jsonutil.MapAt(obj, "content_block"))
	for _, field := range []string{"output", "content"} {
		for _, raw := range jsonutil.SliceAt(obj, field) {
			v, _ := raw.(map[string]any)
			item(v)
		}
	}
	if response := jsonutil.MapAt(obj, "response"); response != nil {
		visitToolNames(response, visit)
	}
	for _, raw := range jsonutil.SliceAt(obj, "choices") {
		choice, _ := raw.(map[string]any)
		for _, field := range []string{"delta", "message"} {
			message := jsonutil.MapAt(choice, field)
			visit(jsonutil.MapAt(message, "function_call"))
			for _, raw := range jsonutil.SliceAt(message, "tool_calls") {
				call, _ := raw.(map[string]any)
				visit(jsonutil.MapAt(call, "function"))
			}
		}
	}
}

func restoreToolNames(obj map[string]any, names map[string]string) bool {
	changed := false
	visitToolNames(obj, func(item map[string]any) {
		if original := names[jsonutil.StringAt(item, "name")]; original != "" {
			item["name"] = original
			changed = true
		}
	})
	return changed
}

type toolAliasWriter struct {
	http.ResponseWriter
	names        map[string]string
	blocked      map[string]bool
	pending      bytes.Buffer
	failure      error
	partialNames map[string]string
}

func (w *toolAliasWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *toolAliasWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (w *toolAliasWriter) rewrite(raw []byte) []byte {
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return raw
	}
	changed := false
	// Chat permits a function name to arrive in several deltas. Hold only an
	// alias prefix, not the event or its arguments; emit the original name as
	// soon as the full alias is known.
	for _, rawChoice := range jsonutil.SliceAt(obj, "choices") {
		choice, _ := rawChoice.(map[string]any)
		for _, rawCall := range jsonutil.SliceAt(jsonutil.MapAt(choice, "delta"), "tool_calls") {
			call, _ := rawCall.(map[string]any)
			fn := jsonutil.MapAt(call, "function")
			fragment, ok := fn["name"].(string)
			if !ok || fragment == "" {
				continue
			}
			key := fmt.Sprintf("%d:%d", jsonutil.IntAt(choice, "index"), jsonutil.IntAt(call, "index"))
			candidate := w.partialNames[key] + fragment
			if w.blocked[candidate] {
				w.failure = fmt.Errorf("upstream selected a non-executable compatibility tool")
				return raw
			}
			if original := w.names[candidate]; original != "" {
				fn["name"] = candidate
				delete(w.partialNames, key)
				changed = true
				continue
			}
			prefix := false
			for alias := range w.names {
				if strings.HasPrefix(alias, candidate) {
					prefix = true
					break
				}
			}
			if !prefix {
				for name := range w.blocked {
					if strings.HasPrefix(name, candidate) {
						prefix = true
						break
					}
				}
			}
			if prefix {
				if w.partialNames == nil {
					w.partialNames = map[string]string{}
				}
				w.partialNames[key] = candidate
				fn["name"] = ""
				changed = true
			} else if w.partialNames[key] != "" {
				fn["name"] = candidate
				delete(w.partialNames, key)
				changed = true
			}
		}
	}
	visitToolNames(obj, func(item map[string]any) {
		if w.blocked[jsonutil.StringAt(item, "name")] {
			w.failure = fmt.Errorf("upstream selected a non-executable compatibility tool")
		}
	})
	if w.failure != nil {
		return raw
	}
	changed = restoreToolNames(obj, w.names) || changed
	if !changed {
		return raw
	}
	encoded, err := json.Marshal(obj)
	if err != nil {
		return raw
	}
	return encoded
}
func (w *toolAliasWriter) Write(p []byte) (int, error) {
	if w.failure != nil {
		return 0, w.failure
	}
	w.Header().Del("Content-Length")
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		rewritten := w.rewrite(p)
		if w.failure != nil {
			return 0, w.failure
		}
		_, err := w.ResponseWriter.Write(rewritten)
		return len(p), err
	}
	w.pending.Write(p)
	for {
		raw := w.pending.Bytes()
		end := bytes.Index(raw, []byte("\n\n"))
		width := 2
		if cr := bytes.Index(raw, []byte("\r\n\r\n")); cr >= 0 && (end < 0 || cr < end) {
			end, width = cr, 4
		}
		if end < 0 {
			break
		}
		frame := append([]byte(nil), raw[:end+width]...)
		w.pending.Next(end + width)
		var payload []byte
		for _, line := range bytes.Split(frame, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				payload = append(payload, bytes.TrimSpace(line[5:])...)
				payload = append(payload, '\n')
			}
		}
		if len(payload) > 0 {
			rewritten := w.rewrite(bytes.TrimSpace(payload))
			if w.failure != nil {
				return 0, w.failure
			}
			if !bytes.Equal(rewritten, bytes.TrimSpace(payload)) {
				var result bytes.Buffer
				for _, line := range bytes.Split(frame, []byte("\n")) {
					if !bytes.HasPrefix(line, []byte("data:")) && len(bytes.TrimSpace(line)) > 0 {
						result.Write(line)
						result.WriteByte('\n')
					}
				}
				fmt.Fprintf(&result, "data: %s\n\n", rewritten)
				frame = result.Bytes()
			}
		}
		if _, err := w.ResponseWriter.Write(frame); err != nil {
			w.failure = err
			return 0, err
		}
	}
	return len(p), nil
}
func (w *toolAliasWriter) Finish() error {
	if w.failure != nil {
		return w.failure
	}
	if w.pending.Len() > 0 {
		return fmt.Errorf("incomplete tool alias response event")
	}
	if len(w.partialNames) > 0 {
		return fmt.Errorf("incomplete client tool name")
	}
	return nil
}

// Payload strings and schemas are immutable during name lowering. Copy only
// the containers whose name fields will be changed, not megabytes of history.
func copyAliasMap(input map[string]any) map[string]any {
	if input == nil {
		return nil
	}
	out := make(map[string]any, len(input))
	for k, v := range input {
		out[k] = v
	}
	return out
}

// Retain references in the client's instructions without editing their text.
// Some compatible backends ignore allowed_tools; explain the aliases explicitly
// rather than forcing a function when the caller requested automatic choice.
func describeClientTools(input map[string]any, proto protocol.Protocol, names map[string]string, anonymous bool) map[string]any {
	if len(names) == 0 && !anonymous {
		return input
	}
	tools := []string{}
	for _, raw := range jsonutil.SliceAt(input, "tools") {
		if name := anonymousToolName(proto, raw); name != "" {
			tools = append(tools, name)
		}
	}
	sort.Strings(tools)
	var note strings.Builder
	note.WriteString("Gateway tool routing: only these caller-supplied tools can be executed: ")
	if len(tools) == 0 {
		note.WriteString("none")
	} else {
		note.WriteString(strings.Join(tools, ", "))
	}
	note.WriteString(". Respect the caller's tool_choice; automatic selection may answer without a tool. ")
	aliases := make([]string, 0, len(names))
	for alias := range names {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		fmt.Fprintf(&note, "Instructions referring to %q mean the caller tool %q; use that tool's exact declared argument schema. ", names[alias], alias)
	}
	if anonymous {
		note.WriteString("Additional built-in tool declarations are compatibility placeholders, not executable tools. Never call a tool outside the caller-supplied list.")
	}
	out := copyAliasMap(input)
	text := note.String()
	switch proto {
	case protocol.Anthropic:
		if system, ok := input["system"].(string); ok {
			out["system"] = text + "\n\n" + system
		} else if blocks, ok := input["system"].([]any); ok {
			out["system"] = append([]any{map[string]any{"type": "text", "text": text}}, blocks...)
		} else if input["system"] == nil {
			out["system"] = text
		} else {
			return input
		}
	case protocol.Responses:
		instructions, ok := input["instructions"].(string)
		if input["instructions"] != nil && !ok {
			return input
		}
		out["instructions"] = text + "\n\n" + instructions
	default:
		if input["messages"] != nil {
			if _, ok := input["messages"].([]any); !ok {
				return input
			}
		}
		messages := jsonutil.SliceAt(input, "messages")
		out["messages"] = append([]any{map[string]any{"role": "system", "content": text}}, messages...)
	}
	return out
}
