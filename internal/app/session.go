package app

// 会话粘性路由：同一会话的连续请求粘住同一账号，保上游 prompt cache 命中。
// 忠实移植自上游 gateway/session.py（Sliverkiss 粘性键系列的简化版）。
//
// 多账号时账号池逐请求加权轮换，同一会话打到不同账号会打散上游前缀缓存
// （命中归账号维度，换号即全量重算），token 成本与延迟显著上升。粘住后：
// 账号被删/停用/冷却/模型冷却时自动解粘回池轮换，绝不把请求硬塞进坏账号。
// 单账号场景键照常提取，行为不变。

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
)

// extractSessionKey 从（转换前的原始）客户端请求体提取会话键；无会话特征返回空串。
// 必须在 BuildUpstreamBody 的白名单剥字段之前提取，否则丢失 prompt_cache_key/metadata。
func extractSessionKey(body map[string]any) string {
	if body == nil {
		return ""
	}
	// 1) OpenAI prompt_cache_key
	if k, ok := body["prompt_cache_key"].(string); ok && strings.TrimSpace(k) != "" {
		return "pck:" + strings.TrimSpace(k)
	}
	// 2) metadata.conversation_id
	if meta, ok := body["metadata"].(map[string]any); ok {
		for _, key := range []string{"conversation_id", "conversationId"} {
			if cid, ok := meta[key].(string); ok && strings.TrimSpace(cid) != "" {
				return "cid:" + strings.TrimSpace(cid)
			}
		}
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
}

// sessionRouter 是 会话键 → 账号 uid 的粘性映射（TTL + 惰性 GC，纯内存）。
type sessionRouter struct {
	mu  sync.Mutex
	ttl float64
	max int
	m   map[string]sessionEntry
}

func newSessionRouter() *sessionRouter {
	return &sessionRouter{ttl: 1800, max: 2000, m: map[string]sessionEntry{}}
}

func (r *sessionRouter) bind(key, uid string) {
	if key == "" || uid == "" {
		return
	}
	now := nowSec()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[key] = sessionEntry{uid: uid, expires: now + r.ttl}
	if len(r.m) > r.max {
		for k, e := range r.m {
			if e.expires <= now {
				delete(r.m, k)
			}
		}
	}
}

func (r *sessionRouter) unbind(key string) {
	if key == "" {
		return
	}
	r.mu.Lock()
	delete(r.m, key)
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
	if e.expires <= now {
		delete(r.m, key)
		return ""
	}
	return e.uid
}
