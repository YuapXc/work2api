package profiles

import (
	"strings"
	"testing"

	"work2api/internal/workbuddy/siterouting"
)

func testAuth() siterouting.Auth {
	return siterouting.Auth{"accessToken": "tok", "domain": "copilot.tencent.com"}
}

// Chat 保留三个共享请求标识，并额外发送独立的 UUID 会话头；不发送 X-Session-ID。
func TestChatHeadersRequestIDs(t *testing.T) {
	h := ChatHeaders(testAuth(), Account{"uid": "u1"})
	rid := h["X-Request-ID"]
	if rid == "" || len(rid) != 32 {
		t.Fatalf("X-Request-ID 应为 32 位 hex, got %q", rid)
	}
	if h["X-Conversation-Message-ID"] != rid || h["X-Conversation-Request-ID"] != rid {
		t.Fatal("三个 id 头应共享同一值（对账用）")
	}
	conversation := h["X-Conversation-ID"]
	if len(conversation) != 36 || conversation[14] != '4' || !strings.ContainsRune("89ab", rune(conversation[19])) {
		t.Fatal("conversation id must be UUID v4")
	}
	for _, k := range []string{"X-Session-ID"} {
		if _, ok := h[k]; ok {
			t.Fatalf("%s 不应发送（疑似缓存归属键）", k)
		}
	}
	// 每次调用生成新 id
	h2 := ChatHeaders(testAuth(), Account{"uid": "u1"})
	if h2["X-Request-ID"] == rid {
		t.Fatal("request id 应每次重新生成")
	}
	if h2["X-Conversation-ID"] == conversation {
		t.Fatal("conversation transport id should be fresh")
	}
	// 基础身份头仍在
	if !strings.Contains(h["User-Agent"], "CLI/") {
		t.Fatalf("UA 缺失或异常: %q", h["User-Agent"])
	}
	if h["Authorization"] != "Bearer tok" {
		t.Fatal("Authorization 缺失")
	}
}

func TestChatDeviceTokenIsAccountAndProductScoped(t *testing.T) {
	for _, tc := range []struct {
		domain, token string
		want          bool
	}{
		{"www.workbuddy.cn", "real-account-device", true},
		{"www.codebuddy.cn", "real-account-device", false},
		{"www.codebuddy.ai", "real-account-device", false},
		{"www.workbuddy.ai", "real-account-device", false},
		{"www.workbuddy.cn", "bad\r\nheader", false},
		{"www.workbuddy.cn", strings.Repeat("x", 1025), false},
	} {
		auth := siterouting.Auth{"accessToken": "fixture", "domain": tc.domain, "deviceToken": tc.token}
		got := ChatHeaders(auth, Account{"uid": "fixture"})
		if (got["X-Device-Token"] == tc.token) != tc.want {
			t.Fatal(tc.domain, tc.want)
		}
		if CredentialHeaders(auth, nil)["X-Device-Token"] != "" || CatalogHeaders(auth, nil)["X-Device-Token"] != "" {
			t.Fatal("device token leaked into another purpose")
		}
	}
	first := siterouting.Auth{"domain": "www.workbuddy.cn", "accessToken": "one", "device_token": "first-device"}
	second := siterouting.Auth{"domain": "www.workbuddy.cn", "accessToken": "two"}
	if ChatHeaders(first, nil)["X-Device-Token"] != "first-device" || ChatHeaders(second, nil)["X-Device-Token"] != "" {
		t.Fatal("account token crossed identities")
	}
}

func TestCloudEnterpriseRoutingAndIOA(t *testing.T) {
	for _, tc := range []struct{ domain, endpoint string }{
		{"team.sso.copilot.tencent.com", "https://team.sso.copilot.tencent.com"},
		{"team.copilot.qq.com", "https://team.copilot.qq.com"},
		{"team.sso.codebuddy.cn", "https://team.sso.codebuddy.cn"},
		{"tencent.sso.copilot.tencent.com", siterouting.DomesticEndpoint},
		{"tencent.sso.codebuddy.cn", siterouting.DomesticEndpoint},
		{"www.workbuddy.cn", "https://www.workbuddy.cn"},
	} {
		a := siterouting.Auth{"domain": tc.domain}
		endpoint, err := siterouting.EndpointForAuth(a)
		if err != nil || endpoint != tc.endpoint {
			t.Fatal(tc.domain, endpoint, err)
		}
		refresh, err := siterouting.RefreshURLForAuth(a)
		if err != nil || refresh != tc.endpoint+"/v2/plugin/auth/token/refresh" {
			t.Fatal(refresh, err)
		}
		if Domain := siterouting.DomainForAuth(a); Domain != tc.domain {
			t.Fatal("wrong domain header", Domain)
		}
	}
	for _, host := range []string{"team.sso.codebuddy.cn.evil.test", "a.b.sso.codebuddy.cn", "-a.sso.codebuddy.cn", "https://u@team.sso.codebuddy.cn", "https://team.sso.codebuddy.cn:443", "http://team.sso.codebuddy.cn", "127.0.0.1", "team.sso.codebuddy.cn/path"} {
		if _, err := siterouting.NormalizeDomain(host); err == nil {
			t.Fatal("unsafe enterprise host accepted", host)
		}
	}
}
