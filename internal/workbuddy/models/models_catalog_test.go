package models

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"work2api/internal/workbuddy/pool"
)

type catalogTestCredential struct{ profile string }

func (c catalogTestCredential) Profile() string         { return c.profile }
func (c catalogTestCredential) Summary() map[string]any { return nil }
func (c catalogTestCredential) Path() string            { return "" }
func (c catalogTestCredential) CatalogHeaders() (map[string]string, error) {
	return map[string]string{}, nil
}

type countingTransport struct{ calls atomic.Int32 }

func (t *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return &http.Response{
		StatusCode: 200,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(`{"data":{"models":[{"id":"test-model","name":"Test"}],"agents":[{"name":"cli","models":["test-model"]}]}}`)),
		Request:    r,
	}, nil
}

// 目录拉取不得触碰停用账号（对齐上游 0.6.3）：手动停用或共享池「贡献验证中」
// 的账号不应被一次目录请求唤醒/触发风控。
func TestFetchFromUpstreamSkipsDisabledAccounts(t *testing.T) {
	transport := &countingTransport{}
	p := pool.New(map[string]pool.Credential{
		"enabled-uid":  catalogTestCredential{profile: "cn-cli"},
		"disabled-uid": catalogTestCredential{profile: "cn-cli"},
	}, "")
	p.SetEnabled("disabled-uid", false, "已禁用")
	r := NewWithCatalogClient(p, nil, &http.Client{Transport: transport})
	got := r.fetchFromUpstream()
	if len(got) == 0 {
		t.Fatal("no catalog fetched")
	}
	for _, fm := range got {
		uids, _ := fm.entry["account_uids"].([]string)
		for _, uid := range uids {
			if uid == "disabled-uid" {
				t.Fatal("disabled account contributed catalog", fm.id)
			}
		}
	}
	if calls := transport.calls.Load(); calls != 1 {
		t.Fatal("disabled account was fetched", calls)
	}
}
