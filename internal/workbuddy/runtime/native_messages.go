package runtime

import (
	"context"
	"encoding/json"
	"maps"
	"strconv"
	"strings"

	"work2api/internal/core/protocol"
	"work2api/internal/workbuddy/siterouting"
)

type messagesRequestKey struct{}
type messagesBetaKey struct{}

func nativeMessagesEndpoint(profile, model string) string {
	if profile != "cn-cli" || !strings.HasPrefix(strings.ToLower(model), "claude-") {
		return ""
	}
	return siterouting.DomesticEndpoint + "/v1/messages"
}

func nativeMessagesBody(ctx context.Context, chat map[string]any) (map[string]any, error) {
	var body map[string]any
	if original, ok := ctx.Value(messagesRequestKey{}).(map[string]any); ok {
		body = maps.Clone(original)
	} else {
		var err error
		body, err = protocol.PrepareRequest(protocol.Chat, protocol.Anthropic, chat, "")
		if err != nil {
			return nil, err
		}
	}
	body["model"] = chat["model"]
	body["stream"] = true
	return withoutMessagesThinking(body, false), nil
}

func recoverNativeImages(body map[string]any, raw []byte, full bool) (map[string]any, int, int) {
	if full {
		out, count := withoutRequestImages(body)
		return out, 0, count
	}
	mi, pi, targeted := rejectedImagePosition(raw)
	return transformRequestImages(body, func(m, p int, part map[string]any) (map[string]any, bool, bool) {
		if targeted {
			return nil, false, m == mi && p == pi
		}
		source, _ := part["source"].(map[string]any)
		if source["type"] != "base64" {
			return nil, false, false
		}
		value, fixed, invalid := compatibleImageURL("data:" + ToStr(source["media_type"]) + ";base64," + ToStr(source["data"]))
		if !fixed {
			return nil, false, invalid
		}
		header, data, _ := strings.Cut(value, ",")
		mime := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
		next := maps.Clone(part)
		copy := maps.Clone(source)
		copy["media_type"], copy["data"] = mime, data
		next["source"] = copy
		return next, true, false
	})
}

func withoutMessagesThinking(body map[string]any, all bool) map[string]any {
	messages, _ := body["messages"].([]any)
	var next []any
	for i, v := range messages {
		m, ok := v.(map[string]any)
		if !ok || m["role"] != "assistant" {
			continue
		}
		blocks, ok := m["content"].([]any)
		if !ok {
			continue
		}
		var kept []any
		changed := false
		for _, v := range blocks {
			b, _ := v.(map[string]any)
			if b["type"] == "thinking" && (all || b["signature"] == nil || b["signature"] == "") {
				changed = true
				continue
			}
			kept = append(kept, v)
		}
		if !changed {
			continue
		}
		if next == nil {
			next = append([]any(nil), messages...)
		}
		if len(kept) == 0 {
			kept = []any{map[string]any{"type": "text", "text": ""}}
		}
		copy := maps.Clone(m)
		copy["content"] = kept
		next[i] = copy
	}
	if next == nil {
		return body
	}
	out := maps.Clone(body)
	out["messages"] = next
	return out
}

type nativeMessageResult struct {
	message map[string]any
	blocks  map[int]map[string]any
	args    map[int]*strings.Builder
	text    map[int]map[string]*strings.Builder
}

func messageBlockIndex(value any) int { i, _ := strconv.Atoi(ToStr(value)); return i }

func (n *nativeMessageResult) observe(event map[string]any) {
	switch event["type"] {
	case "message_start":
		message, _ := event["message"].(map[string]any)
		n.message = maps.Clone(message)
		if n.message == nil {
			n.message = map[string]any{}
		}
		n.blocks = map[int]map[string]any{}
		n.args = map[int]*strings.Builder{}
		n.text = map[int]map[string]*strings.Builder{}
	case "content_block_start":
		if n.blocks == nil {
			return
		}
		i := messageBlockIndex(event["index"])
		b, _ := event["content_block"].(map[string]any)
		n.blocks[i] = maps.Clone(b)
	case "content_block_delta":
		i := messageBlockIndex(event["index"])
		b := n.blocks[i]
		d, _ := event["delta"].(map[string]any)
		if b == nil {
			return
		}
		for _, key := range []string{"text", "thinking", "signature"} {
			if s, ok := d[key].(string); ok {
				if n.text[i] == nil {
					n.text[i] = map[string]*strings.Builder{}
				}
				if n.text[i][key] == nil {
					n.text[i][key] = &strings.Builder{}
					n.text[i][key].WriteString(ToStr(b[key]))
				}
				n.text[i][key].WriteString(s)
			}
		}
		if s, ok := d["partial_json"].(string); ok {
			if n.args[i] == nil {
				n.args[i] = &strings.Builder{}
			}
			n.args[i].WriteString(s)
		}
	case "content_block_stop":
		i := messageBlockIndex(event["index"])
		for key, b := range n.text[i] {
			n.blocks[i][key] = b.String()
		}
		if a := n.args[i]; a != nil {
			var input any
			if json.Unmarshal([]byte(a.String()), &input) == nil {
				n.blocks[i]["input"] = input
			}
		}
		delete(n.args, i)
		delete(n.text, i)
	case "message_delta":
		if n.message == nil {
			return
		}
		delta, _ := event["delta"].(map[string]any)
		for k, v := range delta {
			n.message[k] = v
		}
		if usage, ok := event["usage"].(map[string]any); ok {
			current, _ := n.message["usage"].(map[string]any)
			current = maps.Clone(current)
			if current == nil {
				current = map[string]any{}
			}
			maps.Copy(current, usage)
			n.message["usage"] = current
		}
	case "message_stop":
		if n.message == nil {
			return
		}
		blocks := []any{}
		for i := 0; i < len(n.blocks); i++ {
			blocks = append(blocks, n.blocks[i])
		}
		n.message["content"] = blocks
	}
}
