package app

import (
	"net/http"
	"testing"
	"time"
)

// 验证分类顺序与处置策略——顺序即语义优先级，这些用例锁住易错的边界。
func TestClassifyUpstream(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   ErrKind
	}{
		{"11102 model blocked", 400, `{"code":11102,"msg":"service info not found"}`, errModelBlocked},
		{"402 hard credit", 402, ``, errHardCredit},
		{"number in ordinary error is not session dead", 400, `{"msg":"request 12153 invalid"}`, errClient},
		{"nested session code", 401, `{"error":{"code":"12153"}}`, errSessionDead},
		{"successful model output is not session dead", 200, `{"content":"12153"}`, errNone},
		{"401 session dead 12153", 401, `{"code":12153,"msg":"Offline user session not found"}`, errSessionDead},
		{"403 request illegal account fault", 403, `{"code":11140,"msg":"request illegal"}`, errAccountFault},
		{"429 + 14018 hard credit", 429, `{"code":14018,"msg":"credit"}`, errHardCredit},
		{"429 plain soft rate", 429, `{"msg":"slow down"}`, errSoftRate},
		// 关键顺序：429 的 body 常带 "quota exceeded"，状态码必须先判为限流，
		// 否则误归硬冷却到次日 04:00，白扔一个号约 12h。
		{"429 + quota text stays soft rate", 429, `{"msg":"quota exceeded"}`, errSoftRate},
		{"non-429 quota text hard credit", 200, `{"msg":"quota exceeded"}`, errHardCredit},
		{"11115 prompt too long", 400, `{"code":11115,"msg":"prompt is too long"}`, errPromptTooLong},
		{"11135 image invalid", 400, `{"code":11135,"msg":"invalid image_url content"}`, errImageInvalid},
		{"404 not found", 404, `{}`, errNotFound},
		{"500 server", 500, `oops`, errServer},
		{"403 no envelope waf", 403, `<html>blocked</html>`, errWAFBlock},
		{"11101 bad params", 400, `{"code":11101,"msg":"Unmarshal chat params failed"}`, errBadParams},
		{"channel denied", 400, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel"}`, errChannelDenied},
		{"channel policy display envelope", 400, `{"code":11128,"msg":"Illegal API invocation from an unapproved channel","displayMsg":{"en":"The request was blocked by security policy. Please retry later or contact support."},"actions":["SUBMIT_FEEDBACK","COPY_ERROR","EDIT_INPUT"]}`, errChannelDenied},
		{"nested channel denied", 403, `{"error":{"code":"11128","message":"Illegal API invocation from an unapproved channel"}}`, errChannelDenied},
		{"11128 role validation is not channel denial", 400, `{"code":11128,"msg":"first message is not system prompt"}`, errClient},
		{"11128 alone is not channel denial", 400, `{"code":11128}`, errClient},
		{"channel text without business code stays blocked", 400, `{"msg":"Illegal API invocation from an unapproved channel"}`, errContentBlocked},
		{"security policy still blocks", 400, `{"code":11128,"msg":"blocked by security policy: Illegal API invocation from an unapproved channel"}`, errContentBlocked},
		{"echoed input is not channel denial", 400, `{"code":11128,"msg":"bad request","input":"Illegal API invocation from an unapproved channel"}`, errContentBlocked},
		{"content blocked", 400, `{"msg":"blocked by security policy"}`, errContentBlocked},
		{"generic 4xx client", 400, `{"msg":"weird"}`, errClient},
	}
	for _, c := range cases {
		if got := classifyUpstream(c.status, []byte(c.body), nil); got != c.want {
			t.Errorf("%s: classify(%d, %q) = %q, want %q", c.name, c.status, c.body, got, c.want)
		}
	}
}

func TestActionFor(t *testing.T) {
	now := float64(time.Now().Unix())
	// fail-fast：请求自身问题，不罚号、不换号
	for _, k := range []ErrKind{errPromptTooLong, errImageInvalid, errContentBlocked} {
		a := actionFor(k, nil, nil, now)
		if !a.FailFast || a.Rotate || a.Cooldown != 0 {
			t.Errorf("%s should be fail-fast/no-rotate/no-cooldown, got %+v", k, a)
		}
	}
	if a := actionFor(errChannelDenied, nil, nil, now); !a.Rotate || a.FailFast || a.Disable || a.ModelScoped || a.Cooldown != 0 {
		t.Fatal("channel denial action", a)
	}
	// session dead / account-ban：禁用
	if a := actionFor(errSessionDead, nil, nil, now); !a.Disable {
		t.Errorf("session dead should disable, got %+v", a)
	}
	if a := actionFor(errAccountFault, []byte("request illegal"), nil, now); !a.Disable {
		t.Errorf("11140 request illegal should disable, got %+v", a)
	}
	// 试用未激活：软冷却，不禁用
	if a := actionFor(errAccountFault, []byte("trial not activated"), nil, now); a.Disable || a.Cooldown != softCooldownSec {
		t.Errorf("trial not activated should soft-cooldown, not disable, got %+v", a)
	}
	// model blocked：(账号,模型) 负缓存
	if a := actionFor(errModelBlocked, nil, nil, now); !a.ModelScoped || a.Cooldown <= 0 {
		t.Errorf("model blocked should be model-scoped with cooldown, got %+v", a)
	}
	// hard credit：冷却到次日 04:00（远大于 1 分钟）
	if a := actionFor(errHardCredit, nil, nil, now); a.Cooldown < 60 {
		t.Errorf("hard credit cooldown should reach next 04:00, got %+v", a)
	}
	// client：只换号不罚
	if a := actionFor(errClient, nil, nil, now); !a.Rotate || a.Cooldown != 0 || a.FailFast {
		t.Errorf("client should rotate without penalty, got %+v", a)
	}
}

func TestParseRetryAfter(t *testing.T) {
	h := http.Header{}
	h.Set("Retry-After", "42")
	if v, ok := parseRetryAfter(h, 0); !ok || v != 42 {
		t.Errorf("Retry-After 42 => %v,%v", v, ok)
	}
	h2 := http.Header{}
	h2.Set("Retry-After", "Wed, 21 Oct 2026 07:28:00 GMT") // HTTP-Date 不解析
	if _, ok := parseRetryAfter(h2, 0); ok {
		t.Error("HTTP-Date Retry-After should not parse")
	}
}
