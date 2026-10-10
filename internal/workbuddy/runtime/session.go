package runtime

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
	"work2api/internal/core/provider"
	"work2api/internal/store"
)

// Keep account affinity isolated between users and models; retain only a bounded hash.
func PrincipalSessionKey(p SessionPrincipal, model string, body map[string]any) string {
	key := ExtractSessionKey(body)
	return ScopedSessionKey(p, model, key)
}

func ScopedSessionKey(p SessionPrincipal, model, key string) string {
	if key == "" {
		return ""
	}
	namespace := "private"
	if p != nil && p.SessionCaller().UserID > 0 {
		namespace = "portal:" + strconv.FormatInt(p.SessionCaller().UserID, 10)
	}
	if p != nil {
		namespace += ":app:" + strconv.FormatInt(p.SessionCaller().AppID, 10)
	}
	hash := sha256.Sum256([]byte(namespace + "\x00" + model + "\x00" + key))
	return hex.EncodeToString(hash[:])
}

type RequestSessionIdentity struct{ Key, Agent string }
type RequestSessionIdentityKey struct{}

// Resolve once from the original request, before conversion or queueing.
func WithRequestSessionIdentity(r *http.Request, body map[string]any) *http.Request {
	r = r.WithContext(context.WithValue(r.Context(), RequestDiagnosticsKey{}, &RequestDiagnostics{UsageDiagnostics: store.UsageDiagnostics{RequestedLimits: OutputLimits(body)}}))
	identity := RequestSessionIdentity{}
	if value := provider.HeaderSessionID(r.Header); value != "" {
		identity.Key = "sid:" + value
	}
	if identity.Key == "" {
		identity.Key = ExtractSessionKey(body)
	}
	if value := BoundedSessionID(r.Header.Get("X-Claude-Code-Agent-Id")); value != "" {
		sum := sha256.Sum256([]byte(value))
		identity.Agent = hex.EncodeToString(sum[:])[:16]
	}
	return r.WithContext(context.WithValue(r.Context(), RequestSessionIdentityKey{}, identity))
}

func RequestSessionKey(r *http.Request, p SessionPrincipal, model string, body map[string]any) string {
	if identity, ok := r.Context().Value(RequestSessionIdentityKey{}).(RequestSessionIdentity); ok {
		return ScopedSessionKey(p, model, identity.Key)
	}
	return PrincipalSessionKey(p, model, body)
}

var BoundedSessionID = provider.BoundedSessionID

// ExtractSessionKey 从（转换前的原始）客户端请求体提取会话键；无会话特征返回空串。
// 必须在 BuildUpstreamBody 的白名单剥字段之前提取，否则丢失 prompt_cache_key/metadata。
func ExtractSessionKey(body map[string]any) string {
	if body == nil {
		return ""
	}
	for _, name := range []string{"session_id", "conversation_id"} {
		if value, _ := body[name].(string); BoundedSessionID(value) != "" {
			return "sid:" + BoundedSessionID(value)
		}
	}
	// 2) metadata.conversation_id
	if meta, ok := body["metadata"].(map[string]any); ok {
		for _, key := range []string{"conversation_id", "conversationId", "session_id"} {
			if cid, ok := meta[key].(string); ok && BoundedSessionID(cid) != "" {
				return "sid:" + BoundedSessionID(cid)
			}
		}
		if raw, _ := meta["user_id"].(string); len(raw) <= 512 {
			var identity struct {
				SessionID string `json:"session_id"`
			}
			if json.Unmarshal([]byte(raw), &identity) == nil && BoundedSessionID(identity.SessionID) != "" {
				return "sid:" + BoundedSessionID(identity.SessionID)
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
	if k, _ := body["prompt_cache_key"].(string); BoundedSessionID(k) != "" {
		return "pck:" + BoundedSessionID(k)
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

type SessionEntry struct {
	Uid     string
	Expires float64
	Order   *list.Element
	Info    *SessionState
}

// SessionRouter 是 会话键 → 账号 uid 的粘性映射（TTL + 惰性 GC，纯内存）。
type SessionRouter struct {
	Mu       sync.Mutex
	Ttl      float64
	Max      int
	M        map[string]SessionEntry
	Order    *list.List
	Sequence uint64
}

func NewSessionRouter() *SessionRouter {
	return &SessionRouter{Ttl: 1800, Max: 2000, M: map[string]SessionEntry{}, Order: list.New()}
}

func (r *SessionRouter) RemoveAccount(uid string) {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	for key, entry := range r.M {
		if entry.Uid == uid {
			r.RemoveLocked(key)
		}
	}
}

func (r *SessionRouter) Bind(key, uid string) {
	if key == "" || uid == "" {
		return
	}
	now := nowSec()
	r.Mu.Lock()
	defer r.Mu.Unlock()
	if entry, ok := r.M[key]; ok {
		if entry.Info != nil {
			return
		} // Tracked routes Commit only after account admission.
		entry.Uid, entry.Expires = uid, now+r.Ttl
		r.Order.MoveToBack(entry.Order)
		r.M[key] = entry
		return
	}
	for len(r.M) >= r.Max && r.Order.Len() > 0 {
		if !r.EvictIdleLocked() {
			return
		}
	}
	r.M[key] = SessionEntry{Uid: uid, Expires: now + r.Ttl, Order: r.Order.PushBack(key)}
}

// Evict oldest bindings under pressure; ordinary Lookup does not renew TTL
// or alter account selection. Every auxiliary entry has the same hard cap.
// Never evict a running or queued observation to make room for an idle binding.
func (r *SessionRouter) EvictIdleLocked() bool {
	for el := r.Order.Front(); el != nil; el = el.Next() {
		key := el.Value.(string)
		e := r.M[key]
		if e.Info == nil || e.Info.Running+e.Info.Waiting == 0 {
			r.RemoveLocked(key)
			return true
		}
	}
	return false
}

func (r *SessionRouter) RemoveLocked(key string) {
	if entry, ok := r.M[key]; ok {
		r.Order.Remove(entry.Order)
		delete(r.M, key)
	}
}

func (r *SessionRouter) Unbind(key string) { r.UnbindMatching(key, "") }

// An older in-flight failure must not clear a newer account binding.
func (r *SessionRouter) UnbindMatching(key, failedUID string) {
	if key == "" {
		return
	}
	r.Mu.Lock()
	if failedUID != "" && r.M[key].Uid != failedUID {
		r.Mu.Unlock()
		return
	}
	if entry, ok := r.M[key]; ok && entry.Info != nil {
		entry.Uid = ""
		entry.Info.ManualUID = ""
		ClearCompatibility(entry.Info)
		r.Sequence++
		entry.Info.Version = r.Sequence
		r.M[key] = entry
	} else {
		r.RemoveLocked(key)
	}
	r.Mu.Unlock()
}

// Lookup 返回粘住的 uid（未过期）；过期则解粘并返回空。账号是否可用由调用方判断。
func (r *SessionRouter) Lookup(key string) string {
	if key == "" {
		return ""
	}
	now := nowSec()
	r.Mu.Lock()
	defer r.Mu.Unlock()
	e, ok := r.M[key]
	if !ok {
		return ""
	}
	if e.Expires <= now && (e.Info == nil || e.Info.Running+e.Info.Waiting == 0) {
		r.RemoveLocked(key)
		return ""
	}
	return e.Uid
}

// SessionPrincipal exposes authenticated identity without gateway implementation details.
type SessionPrincipal interface{ SessionCaller() provider.Caller }
