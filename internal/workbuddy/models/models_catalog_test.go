package models

import (
	"context"
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
	if calls := transport.calls.Load(); calls != 2 {
		t.Fatal("disabled account was fetched", calls)
	}
}

// B1：双路目录合并——/v3/config 先拉（同 ID 元数据优先）、插件目录补专属模型；
// 单路失败保留该来源成功快照；全部失败触发失败退避。
type dualTransport struct {
	respond func(path string) (int, string)
	paths   []string
}

func (t *dualTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.paths = append(t.paths, r.URL.Path)
	status, body := t.respond(r.URL.Path)
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    r,
	}, nil
}

func catalogBody(ids ...string) string {
	var models, idsJSON []string
	for _, id := range ids {
		models = append(models, `{"id":"`+id+`","name":"`+id+`"}`)
		idsJSON = append(idsJSON, `"`+id+`"`)
	}
	return `{"data":{"models":[` + strings.Join(models, ",") + `],"agents":[{"name":"cli","models":[` + strings.Join(idsJSON, ",") + `]}]}}`
}

func modelIDs(got []fetchedModel) (map[string]bool, int) {
	ids := map[string]bool{}
	fresh := 0
	for _, fm := range got {
		ids[fm.id] = true
		if fm.fresh {
			fresh++
		}
	}
	return ids, fresh
}

func TestFetchOneMergesDualCatalogSources(t *testing.T) {
	tr := &dualTransport{respond: func(path string) (int, string) {
		if path == "/v3/config" {
			return 200, catalogBody("shared-model", "client-only")
		}
		return 200, catalogBody("shared-model", "legacy-only")
	}}
	p := pool.New(map[string]pool.Credential{"uid1": catalogTestCredential{profile: "cn-cli"}}, "")
	r := NewWithCatalogClient(p, nil, &http.Client{Transport: tr})
	got := r.fetchOneContext(context.Background(), p.Accounts()[0])
	if got == nil {
		t.Fatal("fetch failed")
	}
	ids, fresh := modelIDs(got)
	for _, want := range []string{"shared-model", "client-only", "legacy-only"} {
		if !ids[want] {
			t.Fatalf("缺少 %s: %v", want, ids)
		}
	}
	if fresh != 3 {
		t.Fatalf("两路成功时全部条目应为真实观测, fresh=%d", fresh)
	}
	if len(tr.paths) != 2 || tr.paths[0] != "/v3/config" {
		t.Fatalf("/v3/config 应先拉: %v", tr.paths)
	}
}

func TestFetchOneSingleSourceFailureKeepsSnapshot(t *testing.T) {
	failLegacy := atomic.Bool{}
	tr := &dualTransport{respond: func(path string) (int, string) {
		if path == "/v3/config" {
			return 200, catalogBody("client-only")
		}
		if failLegacy.Load() {
			return 503, `{}`
		}
		return 200, catalogBody("legacy-only")
	}}
	p := pool.New(map[string]pool.Credential{"uid1": catalogTestCredential{profile: "cn-cli"}}, "")
	r := NewWithCatalogClient(p, nil, &http.Client{Transport: tr})
	acc := p.Accounts()[0]
	if got := r.fetchOneContext(context.Background(), acc); got == nil {
		t.Fatal("首次拉取失败")
	}
	failLegacy.Store(true)
	got := r.fetchOneContext(context.Background(), acc)
	if got == nil {
		t.Fatal("单路失败不应整路失败")
	}
	ids, fresh := modelIDs(got)
	if !ids["legacy-only"] {
		t.Fatal("插件目录单路失败应保留其成功快照的专属模型")
	}
	if !ids["client-only"] || fresh != 1 {
		t.Fatalf("只有 /v3/config 条目应为真实观测: ids=%v fresh=%d", ids, fresh)
	}
}

func TestFetchOneAllSourcesFailedPenalizes(t *testing.T) {
	tr := &dualTransport{respond: func(path string) (int, string) { return 503, `{}` }}
	p := pool.New(map[string]pool.Credential{"uid1": catalogTestCredential{profile: "cn-cli"}}, "")
	r := NewWithCatalogClient(p, nil, &http.Client{Transport: tr})
	acc := p.Accounts()[0]
	if got := r.fetchOneContext(context.Background(), acc); got != nil {
		t.Fatal("全部来源失败必须返回 nil（失败退避），不得伪装成功")
	}
	if len(tr.paths) != 2 {
		t.Fatalf("两路都应尝试过: %v", tr.paths)
	}
}

func TestFetchOneRejectsBusinessErrorCode(t *testing.T) {
	tr := &dualTransport{respond: func(path string) (int, string) {
		if path == "/v3/config" {
			return 200, `{"code":999,"data":{"models":[{"id":"bad-model","name":"bad"}],"agents":[{"name":"cli","models":["bad-model"]}]}}`
		}
		return 200, catalogBody("legacy-only")
	}}
	p := pool.New(map[string]pool.Credential{"uid1": catalogTestCredential{profile: "cn-cli"}}, "")
	r := NewWithCatalogClient(p, nil, &http.Client{Transport: tr})
	got := r.fetchOneContext(context.Background(), p.Accounts()[0])
	for _, fm := range got {
		if fm.id == "bad-model" {
			t.Fatal("业务错误码响应中的模型不得入库")
		}
	}
}

type isolatedCatalogTransport func(*http.Request) (*http.Response, error)

func (f isolatedCatalogTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type isolatedCatalogCredential struct {
	catalogTestCredential
	uid string
}

func (c isolatedCatalogCredential) CatalogHeaders() (map[string]string, error) {
	return map[string]string{"X-Test-UID": c.uid}, nil
}
func catalogResponse(r *http.Request, status int, b string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(b)), Request: r}
}
func TestFreshSourceOverridesFailedSourceSnapshot(t *testing.T) {
	phase := 0
	tr := &dualTransport{respond: func(path string) (int, string) {
		if phase == 1 && path == "/v3/config" {
			return 503, `{}`
		}
		name := "Old"
		if phase == 1 {
			name = "New"
		}
		return 200, strings.Replace(catalogBody("shared"), `"name":"shared"`, `"name":"`+name+`"`, 1)
	}}
	p := pool.New(map[string]pool.Credential{"uid": catalogTestCredential{profile: "cn-cli"}}, "")
	r := NewWithCatalogClient(p, nil, &http.Client{Transport: tr})
	r.fetchOneContext(context.Background(), p.Accounts()[0])
	phase = 1
	got := r.fetchOneContext(context.Background(), p.Accounts()[0])
	if len(got) != 1 || got[0].entry["name"] != "New" || !got[0].fresh {
		t.Fatalf("fresh metadata masked: %#v", got)
	}
}
func TestCanceledCatalogDoesNotPenalizeAccount(t *testing.T) {
	p := pool.New(map[string]pool.Credential{"uid": catalogTestCredential{profile: "cn-cli"}}, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := NewWithCatalogClient(p, nil, &http.Client{Transport: isolatedCatalogTransport(func(req *http.Request) (*http.Response, error) { return nil, req.Context().Err() })})
	r.fetchOneContext(ctx, p.Accounts()[0])
	r.RefreshContext(ctx)
	if r.lastFail != 0 {
		t.Fatal("canceled refresh started failure backoff")
	}
	if a := p.Get("uid"); a.FailureCount != 0 || a.CooldownUntil != 0 {
		t.Fatal("cancellation punished account")
	}
}
func TestRefreshPreservesPublishedSnapshotAndSourceRestrictions(t *testing.T) {
	phase := 0
	p := pool.New(map[string]pool.Credential{"a": isolatedCatalogCredential{catalogTestCredential{profile: "cn-cli"}, "a"}, "b": isolatedCatalogCredential{catalogTestCredential{profile: "cn-cli"}, "b"}}, "")
	first := p.Accounts()[0].UID
	tr := isolatedCatalogTransport(func(req *http.Request) (*http.Response, error) {
		uid := req.Header.Get("X-Test-UID")
		if req.URL.Path != "/v3/config" || phase == 0 && uid != first {
			return catalogResponse(req, 200, catalogBody()), nil
		}
		if phase == 1 && uid == first {
			return catalogResponse(req, 503, `{}`), nil
		}
		flag := "false"
		if phase == 1 {
			flag = "true"
		}
		return catalogResponse(req, 200, `{"data":{"models":[{"id":"shared","name":"shared","supportsReasoning":true,"onlyReasoning":`+flag+`}],"agents":[{"name":"cli","models":["shared"]}]}}`), nil
	})
	r := NewWithCatalogClient(p, nil, &http.Client{Transport: tr})
	old := r.Refresh()
	var cfg map[string]any
	for _, m := range old {
		if m["id"] == "shared" {
			cfg = m["reasoning"].(map[string]any)
		}
	}
	phase = 1
	r.Refresh()
	if cfg["onlyReasoning"] != false {
		t.Fatal("refresh mutated old snapshot")
	}
	for _, m := range r.ListCached() {
		if m["id"] == "shared" && !boolOf(m["reasoning"].(map[string]any)["onlyReasoning"]) {
			t.Fatal("merged restriction lost")
		}
	}
	cfg["onlyReasoning"] = true
	sources, _ := catalogSources("cn-cli")
	if boolOf(r.catalogCache[[3]string{"cn-cli", first, sources[0]}][0].reasoning["onlyReasoning"]) {
		t.Fatal("source snapshot polluted")
	}
}
