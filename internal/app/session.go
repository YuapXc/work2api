package app

// 会话粘性路由：同一会话的连续请求粘住同一账号，保上游 prompt cache 命中。
// 忠实移植自上游 gateway/session.py（Sliverkiss 粘性键系列的简化版）。
//
// 多账号时账号池逐请求加权轮换，同一会话打到不同账号会打散上游前缀缓存
// （命中归账号维度，换号即全量重算），token 成本与延迟显著上升。粘住后：
// 账号被删/停用/冷却/模型冷却时自动解粘回池轮换，绝不把请求硬塞进坏账号。
// 单账号场景键照常提取，行为不变。

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"work2api/internal/store"
)

// Keep account affinity isolated between users and models; retain only a bounded hash.
func principalSessionKey(p *Principal, model string, body map[string]any) string {
	key := extractSessionKey(body)
	return scopedSessionKey(p, model, key)
}

func scopedSessionKey(p *Principal, model, key string) string {
	if key == "" {
		return ""
	}
	namespace := "private"
	if p != nil && p.UserID > 0 {
		namespace = "portal:" + strconv.FormatInt(p.UserID, 10)
	}
	if p != nil {
		namespace += ":app:" + strconv.FormatInt(p.AppID, 10)
	}
	hash := sha256.Sum256([]byte(namespace + "\x00" + model + "\x00" + key))
	return hex.EncodeToString(hash[:])
}

type requestSessionIdentity struct{ key, agent string }
type requestSessionIdentityKey struct{}

// Resolve once from the original request, before conversion or queueing.
func withRequestSessionIdentity(r *http.Request, body map[string]any) *http.Request {
	r = r.WithContext(context.WithValue(r.Context(), requestDiagnosticsKey{}, &requestDiagnostics{UsageDiagnostics: store.UsageDiagnostics{RequestedLimits: outputLimits(body)}}))
	identity := requestSessionIdentity{}
	for _, name := range []string{"X-Claude-Code-Session-Id", "X-Session-Id", "Session-Id", "X-Conversation-Id", "Conversation-Id"} {
		if value := boundedSessionID(r.Header.Get(name)); value != "" {
			identity.key = "sid:" + value
			break
		}
	}
	if identity.key == "" {
		identity.key = extractSessionKey(body)
	}
	if value := boundedSessionID(r.Header.Get("X-Claude-Code-Agent-Id")); value != "" {
		sum := sha256.Sum256([]byte(value))
		identity.agent = hex.EncodeToString(sum[:])[:16]
	}
	return r.WithContext(context.WithValue(r.Context(), requestSessionIdentityKey{}, identity))
}

func requestSessionKey(r *http.Request, p *Principal, model string, body map[string]any) string {
	if identity, ok := r.Context().Value(requestSessionIdentityKey{}).(requestSessionIdentity); ok {
		return scopedSessionKey(p, model, identity.key)
	}
	return principalSessionKey(p, model, body)
}

func boundedSessionID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 512 {
		return ""
	}
	for _, c := range value {
		if c < 32 || c == 127 {
			return ""
		}
	}
	return value
}

// extractSessionKey 从（转换前的原始）客户端请求体提取会话键；无会话特征返回空串。
// 必须在 BuildUpstreamBody 的白名单剥字段之前提取，否则丢失 prompt_cache_key/metadata。
func extractSessionKey(body map[string]any) string {
	if body == nil {
		return ""
	}
	for _, name := range []string{"session_id", "conversation_id"} {
		if value, _ := body[name].(string); boundedSessionID(value) != "" {
			return "sid:" + boundedSessionID(value)
		}
	}
	// 2) metadata.conversation_id
	if meta, ok := body["metadata"].(map[string]any); ok {
		for _, key := range []string{"conversation_id", "conversationId", "session_id"} {
			if cid, ok := meta[key].(string); ok && boundedSessionID(cid) != "" {
				return "sid:" + boundedSessionID(cid)
			}
		}
		if raw, _ := meta["user_id"].(string); len(raw) <= 512 {
			var identity struct {
				SessionID string `json:"session_id"`
			}
			if json.Unmarshal([]byte(raw), &identity) == nil && boundedSessionID(identity.SessionID) != "" {
				return "sid:" + boundedSessionID(identity.SessionID)
			}
			if index := strings.LastIndex(raw, "_session_"); strings.HasPrefix(raw, "user_") && index >= 0 {
				value := raw[index+9:]
				if len(value) == 36 && value[8] == '-' && value[13] == '-' && value[18] == '-' && value[23] == '-' {
					if decoded, err := hex.DecodeString(strings.ReplaceAll(value, "-", "")); err == nil && len(decoded) == 16 {
						return "sid:" + strings.ToLower(value)
					}
				}
			}
		}
	}
	if k, _ := body["prompt_cache_key"].(string); boundedSessionID(k) != "" {
		return "pck:" + boundedSessionID(k)
	}
	// 3) OpenAI user 字段（只取"看起来像标识"的值，避免普通备注误粘）
	if u, ok := body["user"].(string); ok {
		s := strings.TrimSpace(u)
		if len(s) >= 8 && len(s) <= 128 {
			return "user:" + s
		}
	}
	// 4) 兜底：首条 role==user 消息文本指纹（多轮中首条 user 消息保持不变）
	msgs, _ := body["messages"].([]any)
	for _, mi := range msgs {
		m, ok := mi.(map[string]any)
		if !ok || m["role"] != "user" {
			continue
		}
		text := ""
		switch c := m["content"].(type) {
		case string:
			text = c
		case []any:
			var parts []string
			for _, pi := range c {
				if p, ok := pi.(map[string]any); ok && p["type"] == "text" {
					if t, ok := p["text"].(string); ok {
						parts = append(parts, t)
					}
				}
			}
			text = strings.Join(parts, " ")
		}
		if strings.TrimSpace(text) != "" {
			sum := sha256.Sum256([]byte(text))
			return "fb:" + hex.EncodeToString(sum[:])[:16]
		}
	}
	return ""
}

type sessionEntry struct {
	uid     string
	expires float64
	order   *list.Element
	info    *sessionState
}

// sessionRouter 是 会话键 → 账号 uid 的粘性映射（TTL + 惰性 GC，纯内存）。
type sessionRouter struct {
	mu       sync.Mutex
	ttl      float64
	max      int
	m        map[string]sessionEntry
	order    *list.List
	sequence uint64
}

func newSessionRouter() *sessionRouter {
	return &sessionRouter{ttl: 1800, max: 2000, m: map[string]sessionEntry{}, order: list.New()}
}

func (r *sessionRouter) removeAccount(uid string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, entry := range r.m {
		if entry.uid == uid {
			r.removeLocked(key)
		}
	}
}

func (r *sessionRouter) bind(key, uid string) {
	if key == "" || uid == "" {
		return
	}
	now := nowSec()
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.m[key]; ok {
		if entry.info != nil {
			return
		} // Tracked routes commit only after account admission.
		entry.uid, entry.expires = uid, now+r.ttl
		r.order.MoveToBack(entry.order)
		r.m[key] = entry
		return
	}
	for len(r.m) >= r.max && r.order.Len() > 0 {
		if !r.evictIdleLocked() {
			return
		}
	}
	r.m[key] = sessionEntry{uid: uid, expires: now + r.ttl, order: r.order.PushBack(key)}
}

// Evict oldest bindings under pressure; ordinary lookup does not renew TTL
// or alter account selection. Every auxiliary entry has the same hard cap.
// Never evict a running or queued observation to make room for an idle binding.
func (r *sessionRouter) evictIdleLocked() bool {
	for el := r.order.Front(); el != nil; el = el.Next() {
		key := el.Value.(string)
		e := r.m[key]
		if e.info == nil || e.info.Running+e.info.Waiting == 0 {
			r.removeLocked(key)
			return true
		}
	}
	return false
}

func (r *sessionRouter) removeLocked(key string) {
	if entry, ok := r.m[key]; ok {
		r.order.Remove(entry.order)
		delete(r.m, key)
	}
}

func (r *sessionRouter) unbind(key string) { r.unbindMatching(key, "") }

// An older in-flight failure must not clear a newer account binding.
func (r *sessionRouter) unbindMatching(key, failedUID string) {
	if key == "" {
		return
	}
	r.mu.Lock()
	if failedUID != "" && r.m[key].uid != failedUID {
		r.mu.Unlock()
		return
	}
	if entry, ok := r.m[key]; ok && entry.info != nil {
		entry.uid = ""
		entry.info.ManualUID = ""
		clearCompatibility(entry.info)
		r.sequence++
		entry.info.Version = r.sequence
		r.m[key] = entry
	} else {
		r.removeLocked(key)
	}
	r.mu.Unlock()
}

// lookup 返回粘住的 uid（未过期）；过期则解粘并返回空。账号是否可用由调用方判断。
func (r *sessionRouter) lookup(key string) string {
	if key == "" {
		return ""
	}
	now := nowSec()
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[key]
	if !ok {
		return ""
	}
	if e.expires <= now && (e.info == nil || e.info.Running+e.info.Waiting == 0) {
		r.removeLocked(key)
		return ""
	}
	return e.uid
}
