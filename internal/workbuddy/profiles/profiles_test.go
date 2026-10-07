package profiles

import (
	"strings"
	"testing"

	"work2api/internal/workbuddy/siterouting"
)

func testAuth() siterouting.Auth {
	return siterouting.Auth{"accessToken": "tok", "domain": "copilot.tencent.com"}
}

// A8/A10 回归：chat 链路必须带三个共享同一值的请求 id 头，且绝不能发
// X-Conversation-ID / X-Session-ID（疑似上游缓存归属键，上游刻意不发）。
func TestChatHeadersRequestIDs(t *testing.T) {
	h := ChatHeaders(testAuth(), Account{"uid": "u1"})
	rid := h["X-Request-ID"]
	if rid == "" || len(rid) != 32 {
		t.Fatalf("X-Request-ID 应为 32 位 hex, got %q", rid)
	}
	if h["X-Conversation-Message-ID"] != rid || h["X-Conversation-Request-ID"] != rid {
		t.Fatal("三个 id 头应共享同一值（对账用）")
	}
	for _, k := range []string{"X-Conversation-ID", "X-Session-ID"} {
		if _, ok := h[k]; ok {
			t.Fatalf("%s 不应发送（疑似缓存归属键）", k)
		}
	}
	// 每次调用生成新 id
	h2 := ChatHeaders(testAuth(), Account{"uid": "u1"})
	if h2["X-Request-ID"] == rid {
		t.Fatal("request id 应每次重新生成")
	}
	// 基础身份头仍在
	if !strings.Contains(h["User-Agent"], "CLI/") {
		t.Fatalf("UA 缺失或异常: %q", h["User-Agent"])
	}
	if h["Authorization"] != "Bearer tok" {
		t.Fatal("Authorization 缺失")
	}
}
