package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"maps"
	"regexp"
	"strconv"
	"strings"

	"work2api/internal/core/protocol"
	"work2api/internal/workbuddy/siterouting"
	"work2api/internal/workbuddy/upstream"
)

type messagesRequestKey struct{}
type responsesRequestKey struct{}
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
	} else if original, ok := ctx.Value(responsesRequestKey{}).(map[string]any); ok {
		var err error
		body, err = protocol.PrepareRequest(protocol.Responses, protocol.Anthropic, original, "")
		if err != nil {
			return nil, err
		}
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
	return filterMessagesThinking(body, func(_, _ int, block map[string]any) bool {
		if block["type"] != "thinking" {
			return false
		}
		sig, _ := block["signature"].(string)
		return all || strings.TrimSpace(sig) == "" || foreignThinkingSignature.MatchString(sig)
	})
}

var foreignThinkingSignature = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var boundThinkingRefusal = regexp.MustCompile("(?i)invalid `?signature`? in `?thinking`? block|bound to a different conversation|not created in this conversation|invalid data in `?redacted_thinking`? block")
var boundThinkingPosition = regexp.MustCompile(`messages\.(\d+)\.content\.(\d+)`)

type thinkingRefusalKey struct {
	owner string
	block [32]byte
}

func thinkingBlockKey(block map[string]any) [32]byte {
	value, _ := block["signature"].(string)
	if value == "" {
		value, _ = block["data"].(string)
	}
	if value == "" {
		value, _ = block["thinking"].(string)
	}
	kind, _ := block["type"].(string)
	return sha256.Sum256([]byte(kind + "\x00" + value))
}

func isThinkingBlock(block map[string]any) bool {
	return block["type"] == "thinking" || block["type"] == "redacted_thinking"
}

func (r *Runtime) withoutRefusedThinking(owner string, body map[string]any) map[string]any {
	r.thinkingMu.RLock()
	defer r.thinkingMu.RUnlock()
	if len(r.refusedThinking) == 0 {
		return body
	}
	return filterMessagesThinking(body, func(_, _ int, block map[string]any) bool {
		return r.refusedThinking[thinkingRefusalKey{owner, thinkingBlockKey(block)}]
	})
}

func (r *Runtime) recoverBoundThinking(owner string, body map[string]any, err *upstream.UpstreamError) (map[string]any, bool) {
	if err.StatusCode != 400 {
		return body, false
	}
	fault := upstream.ParseBusinessFault(err.Raw)
	if fault.Code != "11159" && !boundThinkingRefusal.Match(err.Raw) {
		return body, false
	}
	fromMessage, fromBlock := 0, 0
	if at := boundThinkingPosition.FindSubmatch(err.Raw); at != nil {
		mi, e1 := strconv.Atoi(string(at[1]))
		bi, e2 := strconv.Atoi(string(at[2]))
		messages, _ := body["messages"].([]any)
		if e1 == nil && e2 == nil && mi < len(messages) {
			message, _ := messages[mi].(map[string]any)
			blocks, _ := message["content"].([]any)
			if bi < len(blocks) {
				block, _ := blocks[bi].(map[string]any)
				if isThinkingBlock(block) {
					fromMessage, fromBlock = mi, bi
				}
			}
		}
	}
	var keys []thinkingRefusalKey
	next := filterMessagesThinking(body, func(mi, bi int, block map[string]any) bool {
		if mi < fromMessage || mi == fromMessage && bi < fromBlock {
			return false
		}
		if len(keys) < 1<<14 {
			keys = append(keys, thinkingRefusalKey{owner, thinkingBlockKey(block)})
		}
		return true
	})
	if len(keys) == 0 {
		return body, false
	}
	r.thinkingMu.Lock()
	if r.refusedThinking == nil || len(r.refusedThinking)+len(keys) > 1<<14 {
		r.refusedThinking = make(map[thinkingRefusalKey]bool)
	}
	for _, key := range keys {
		if len(r.refusedThinking) >= 1<<14 {
			break
		}
		r.refusedThinking[key] = true
	}
	r.thinkingMu.Unlock()
	return next, true
}

func filterMessagesThinking(body map[string]any, drop func(int, int, map[string]any) bool) map[string]any {
	messages, _ := body["messages"].([]any)
	var next []any
	for i, v := range messages {
		m, ok := v.(map[string]any)
		if !ok || m["role"] != "assistant" {
			if next != nil {
				next = append(next, v)
			}
			continue
		}
		blocks, ok := m["content"].([]any)
		if !ok {
			if next != nil {
				next = append(next, v)
			}
			continue
		}
		var kept []any
		changed := false
		for bi, v := range blocks {
			b, _ := v.(map[string]any)
			if isThinkingBlock(b) && drop(i, bi, b) {
				if !changed {
					kept = append(make([]any, 0, len(blocks)), blocks[:bi]...)
				}
				changed = true
				continue
			}
			if changed {
				kept = append(kept, v)
			}
		}
		if !changed {
			if next != nil {
				next = append(next, v)
			}
			continue
		}
		if next == nil {
			next = append(make([]any, 0, len(messages)), messages[:i]...)
		}
		if len(kept) == 0 {
			continue
		}
		copy := maps.Clone(m)
		copy["content"] = kept
		next = append(next, copy)
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
