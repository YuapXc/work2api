// 参照 magpie-community/plugins 的 WorkBuddy Chat 签名协议（MIT）；来源与许可见 docs/providers.md。
package runtime

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"sync"

	"work2api/internal/core/protocol"
	"work2api/internal/core/provider"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/upstream"
)

const chatSignatureBytes = 64 << 20

var chatSignatureRefusal = regexp.MustCompile("(?i)thinking_signature_invalid|invalid\\s+`?signature`?\\s+in\\s+`?thinking|block is bound to a different conversation")

func rejectsChatSignature(raw []byte) bool {
	matches := func(s string) bool {
		s = strings.TrimSpace(s)
		return s != "" && s[0] != '{' && s[0] != '[' && chatSignatureRefusal.MatchString(s)
	}
	fault := upstream.ParseBusinessFault(raw)
	if fault.Code == "11159" || matches(fault.Message) {
		return true
	}
	text := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(text, "{") && !strings.HasPrefix(text, "[") {
		return chatSignatureRefusal.MatchString(text)
	}
	root := JsonRoot(text)
	inner, _ := root["error"].(map[string]any)
	for _, obj := range []map[string]any{root, inner} {
		for _, key := range []string{"msg", "message", "displayMsg", "error", "type", "code"} {
			s, _ := obj[key].(string)
			if matches(s) {
				return true
			}
		}
	}
	return false
}

type chatSignatureTurn struct {
	scope     string
	keys      []string
	extra     json.RawMessage
	reasoning string
	size      int
}
type chatSignatureStore struct {
	mu     sync.Mutex
	order  list.List
	index  map[string]*list.Element
	scopes map[string]int
	used   int
}

func (s *chatSignatureStore) remove(e *list.Element) {
	t := e.Value.(*chatSignatureTurn)
	for _, k := range t.keys {
		if s.index[k] == e {
			delete(s.index, k)
		}
	}
	s.scopes[t.scope]--
	if s.scopes[t.scope] == 0 {
		delete(s.scopes, t.scope)
	}
	s.used -= t.size
	s.order.Remove(e)
}
func signatureAnchors(ids []string, text, reasoning string) []string {
	var out []string
	for _, id := range ids {
		if id != "" {
			out = append(out, "c:"+id)
			if len(id) > 64 {
				out = append(out, "c:"+protocol.BoundCallID(id))
			}
		}
	}
	if len(out) == 0 && strings.TrimSpace(text) != "" {
		out = append(out, fmt.Sprintf("t:%x", sha256.Sum256([]byte(strings.TrimSpace(text)))))
	}
	if len(out) == 0 && strings.TrimSpace(reasoning) != "" {
		out = append(out, fmt.Sprintf("r:%x", sha256.Sum256([]byte(strings.TrimSpace(reasoning)))))
	}
	return out
}
func signatureContent(v any) string {
	if text, ok := v.(string); ok {
		return text
	}
	var out strings.Builder
	blocks, _ := v.([]any)
	for _, raw := range blocks {
		if text, ok := raw.(string); ok {
			out.WriteString(text)
			continue
		}
		m, _ := raw.(map[string]any)
		if text, ok := m["text"].(string); ok {
			out.WriteString(text)
		}
	}
	return out.String()
}
func signatureMessageAnchors(m map[string]any) []string {
	var ids []string
	calls, _ := m["tool_calls"].([]any)
	for _, raw := range calls {
		call, _ := raw.(map[string]any)
		id, _ := call["id"].(string)
		if id != "" {
			ids = append(ids, id)
		}
	}
	reasoning, _ := m["reasoning"].(string)
	if reasoning == "" {
		reasoning, _ = m["reasoning_content"].(string)
	}
	return signatureAnchors(ids, signatureContent(m["content"]), reasoning)
}
func (r *Runtime) chatSignatureScope(ctx context.Context, acc *pool.Account, model string) string {
	if model == "" || provider.CurrentInvocation(ctx).Diagnostic {
		return ""
	}
	identity, _ := ctx.Value(RequestSessionIdentityKey{}).(RequestSessionIdentity)
	if identity.Key == "" || identity.Scope == "" {
		return ""
	}
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%p", identity.Scope, identity.Key, identity.Agent, acc.Profile, acc.UID, model, r.Manager(acc.UID))
}
func (s *chatSignatureStore) restore(scope string, body map[string]any) (map[string]any, map[int]*list.Element) {
	if scope == "" {
		return body, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scopes[scope] == 0 {
		return body, nil
	}
	messages, _ := body["messages"].([]any)
	var copied []any
	restored := map[int]*list.Element{}
	for i, raw := range messages {
		m, _ := raw.(map[string]any)
		if m["role"] != "assistant" {
			continue
		}
		switch m["extra_fields"].(type) {
		case map[string]any, []any:
			continue
		}
		var found *list.Element
		for _, a := range signatureMessageAnchors(m) {
			if e := s.index[scope+"\n"+a]; e != nil {
				found = e
				break
			}
		}
		if found == nil {
			continue
		}
		if copied == nil {
			copied = append([]any(nil), messages...)
		}
		next := make(map[string]any, len(m)+2)
		for k, v := range m {
			next[k] = v
		}
		t := found.Value.(*chatSignatureTurn)
		var extra map[string]any
		_ = json.Unmarshal(t.extra, &extra)
		next["extra_fields"] = extra
		reasoning, _ := next["reasoning"].(string)
		fallback, _ := next["reasoning_content"].(string)
		if strings.TrimSpace(reasoning+fallback) == "" && t.reasoning != "" {
			next["reasoning"] = t.reasoning
			delete(next, "reasoning_content")
		}
		copied[i] = next
		restored[i] = found
		s.order.MoveToBack(found)
	}
	if copied == nil {
		return body, nil
	}
	out := make(map[string]any, len(body))
	for k, v := range body {
		out[k] = v
	}
	out["messages"] = copied
	return out, restored
}
func (s *chatSignatureStore) unsigned(body map[string]any, restored map[int]*list.Element) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	messages, _ := body["messages"].([]any)
	copied := append([]any(nil), messages...)
	for i, e := range restored {
		t := e.Value.(*chatSignatureTurn)
		if len(t.keys) > 0 && s.index[t.keys[0]] == e {
			s.remove(e)
		}
		m, _ := messages[i].(map[string]any)
		next := make(map[string]any, len(m))
		for k, v := range m {
			if k != "extra_fields" {
				next[k] = v
			}
		}
		copied[i] = next
	}
	out := make(map[string]any, len(body))
	for k, v := range body {
		out[k] = v
	}
	out["messages"] = copied
	return out
}
func (s *chatSignatureStore) keep(scope string, t *chatSignatureTap) {
	if scope == "" || t.failed || t.extra == nil {
		return
	}
	anchors := signatureAnchors(t.ids, t.text.String(), t.reasoning.String())
	if len(anchors) == 0 {
		return
	}
	raw, _ := json.Marshal(t.extra)
	size := cap(raw) + t.reasoning.Len() + 512
	for _, anchor := range anchors {
		size += len(scope) + len(anchor) + 64
	}
	if size > chatSignatureBytes {
		return
	}
	turn := &chatSignatureTurn{scope: scope, extra: raw, reasoning: strings.Clone(t.reasoning.String()), size: size}
	for _, anchor := range anchors {
		turn.keys = append(turn.keys, scope+"\n"+anchor)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.index == nil {
		s.index = map[string]*list.Element{}
		s.scopes = map[string]int{}
	}
	for _, key := range turn.keys {
		if e := s.index[key]; e != nil {
			s.remove(e)
		}
	}
	for s.order.Len() >= 2048 || s.used+size > chatSignatureBytes {
		s.remove(s.order.Front())
	}
	e := s.order.PushBack(turn)
	s.used += size
	s.scopes[scope]++
	for _, key := range turn.keys {
		s.index[key] = e
	}
}

type chatSignatureTap struct {
	extra           map[string]any
	reasoning, text strings.Builder
	ids             []string
	idsSeen         map[string]bool
	failed          bool
}

func (t *chatSignatureTap) observe(ctx context.Context, line string) {
	if t.failed {
		return
	}
	chunk := upstream.DecodeFrame(ctx, line)
	choices, _ := chunk["choices"].([]any)
	for _, raw := range choices {
		c, _ := raw.(map[string]any)
		if index, ok := c["index"].(float64); ok && index != 0 {
			continue
		}
		delta, _ := c["delta"].(map[string]any)
		message, _ := c["message"].(map[string]any)
		if delta != nil {
			t.readDelta(delta)
		}
		// A complete JSON response is also projected as an identical delta by the upstream client.
		if message != nil && !t.failed && (delta == nil || !reflect.DeepEqual(delta, message)) {
			t.readDelta(message)
		}
	}
}

func (t *chatSignatureTap) readDelta(d map[string]any) {
	if next, ok := d["extra_fields"].(map[string]any); ok {
		if t.extra == nil {
			t.extra = map[string]any{}
		}
		for k, v := range next {
			old, oldOK := t.extra[k].(map[string]any)
			incoming, newOK := v.(map[string]any)
			if oldOK && newOK {
				merged := make(map[string]any, len(old)+len(incoming))
				for a, b := range old {
					merged[a] = b
				}
				for a, b := range incoming {
					merged[a] = b
				}
				t.extra[k] = merged
			} else {
				t.extra[k] = v
			}
		}
	}
	text, _ := d["content"].(string)
	reason, _ := d["reasoning_content"].(string)
	if reason == "" {
		reason, _ = d["reasoning"].(string)
	}
	if t.reasoning.Len()+t.text.Len()+len(text)+len(reason) > chatSignatureBytes {
		t.failed = true
		t.extra = nil
		t.reasoning.Reset()
		t.text.Reset()
		return
	}
	t.text.WriteString(text)
	t.reasoning.WriteString(reason)
	calls, _ := d["tool_calls"].([]any)
	for _, raw := range calls {
		c, _ := raw.(map[string]any)
		id, _ := c["id"].(string)
		if id != "" && !t.idsSeen[id] {
			if t.idsSeen == nil {
				t.idsSeen = map[string]bool{}
			}
			t.idsSeen[id] = true
			t.ids = append(t.ids, id)
		}
	}
}
