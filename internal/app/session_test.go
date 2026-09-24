package app

import "testing"

func TestExtractSessionKey(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want string
	}{
		{"prompt_cache_key wins", map[string]any{"prompt_cache_key": "abc", "user": "longuser123"}, "pck:abc"},
		{"metadata conversation_id", map[string]any{"metadata": map[string]any{"conversation_id": "conv-9"}}, "cid:conv-9"},
		{"user field", map[string]any{"user": "session-12345"}, "user:session-12345"},
		{"user too short ignored, falls to msg", map[string]any{"user": "abc", "messages": []any{map[string]any{"role": "user", "content": "hi there"}}}, "fb:"},
		{"message text fingerprint", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hello world"}}}, "fb:"},
		{"content blocks", map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "hi"}}}}}, "fb:"},
		{"no session features", map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": "x"}}}, ""},
		{"nil", nil, ""},
	}
	for _, c := range cases {
		got := extractSessionKey(c.body)
		if c.want == "fb:" {
			if len(got) < 3 || got[:3] != "fb:" {
				t.Errorf("%s: got %q, want fb:* prefix", c.name, got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	// 同一首条 user 消息在多轮追加后指纹保持稳定（粘性键的核心保证）
	turn1 := extractSessionKey(map[string]any{"messages": []any{map[string]any{"role": "user", "content": "start"}}})
	turn2 := extractSessionKey(map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": "start"},
		map[string]any{"role": "assistant", "content": "ok"},
		map[string]any{"role": "user", "content": "next"},
	}})
	if turn1 != turn2 || turn1 == "" {
		t.Errorf("fingerprint should be stable across turns: %q vs %q", turn1, turn2)
	}
}

func TestSessionRouter(t *testing.T) {
	r := newSessionRouter()
	if r.lookup("k") != "" {
		t.Error("empty router should miss")
	}
	r.bind("k", "uid1")
	if got := r.lookup("k"); got != "uid1" {
		t.Errorf("lookup after bind = %q, want uid1", got)
	}
	r.unbind("k")
	if r.lookup("k") != "" {
		t.Error("lookup after unbind should miss")
	}
	// 过期项应被解粘
	r.bind("k2", "uid2")
	r.mu.Lock()
	e := r.m["k2"]
	e.expires = nowSec() - 1
	r.m["k2"] = e
	r.mu.Unlock()
	if r.lookup("k2") != "" {
		t.Error("expired entry should be pruned on lookup")
	}
	// 空键不绑定
	r.bind("", "uid")
	if r.lookup("") != "" {
		t.Error("empty key must not bind")
	}
}
