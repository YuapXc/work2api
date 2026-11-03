package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"work2api/internal/workbuddy/credentials"
	"work2api/internal/workbuddy/pool"
)

func TestLiveAccountQuotes(t *testing.T) {
	if os.Getenv("WORK2API_LIVE_QUOTES") != "1" {
		t.Skip("explicit live verification only")
	}
	files := credentials.FindAuthFiles("", filepath.Join("..", "..", "..", "auths"))
	if len(files) == 0 {
		t.Fatal("no local credentials")
	}
	r := New(nil, nil)
	defer r.client.CloseIdleConnections()
	success := 0
	for i, file := range files {
		mgr := credentials.NewManager(file)
		headers, err := mgr.CatalogHeaders()
		if err != nil {
			t.Logf("account %d: authorization unavailable", i+1)
			continue
		}
		sources, err := catalogSources(mgr.Profile())
		if err != nil {
			t.Fatal(err)
		}
		for _, source := range sources {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			data := r.fetchCatalogDocument(ctx, source, headers)
			cancel()
			if data == nil {
				t.Logf("account %d profile=%s catalog unavailable", i+1, mgr.Profile())
				continue
			}
			success++
			rawModels, _ := data["models"].([]any)
			for _, raw := range rawModels {
				m, _ := raw.(map[string]any)
				if m["id"] == "deepseek-v4.1-flash" || m["id"] == "glm-5.3-flash" {
					t.Logf("account %d profile=%s model=%s raw_credits=%v", i+1, mgr.Profile(), m["id"], m["credits"])
				}
			}
		}
	}
	if success == 0 {
		t.Fatal("no live catalog response")
	}
}

func TestPromotionRulesRespectDatesScopeAndTrialEnrollment(t *testing.T) {
	var data map[string]any
	err := json.Unmarshal([]byte(`{
 "productFeaturesConfig":{"ModelTrialBanner":{"banners":[{"modelId":"trial","targetModelId":"paid","trialDays":14,"firstUseTimeKey":"trial.first_use"}]}},
 "modelPromotions":[
  {"enabled":true,"modelIds":["m"],"badge":{"label":"Night offer","icon":"do not retain"},"hover":{"textZh":"Official schedule","action":{"type":"selectModel"}},"priority":50,"schedule":{"timezone":"Asia/Shanghai","daily":[{"start":"08:00","end":"23:00"}]}},
  {"enabled":true,"modelIds":["m"],"badge":{"label":"Night offer"},"hover":{"textZh":"Official schedule"},"priority":100,"discount":{"factor":0},"schedule":{"timezone":"Asia/Shanghai","daily":[{"start":"23:00","end":"08:00"}]}},
  {"enabled":true,"modelIds":["m"],"badge":{"label":"Expired"},"schedule":{"validUntil":"2000-01-01T00:00:00Z"}},
  {"enabled":true,"modelIds":["m"],"badge":{"label":"Future"},"schedule":{"validFrom":"2099-01-01T00:00:00Z"}},
  {"enabled":true,"modelIds":["m"],"badge":{"label":"Invalid date"},"schedule":{"validUntil":"bad"}},
  {"enabled":false,"modelIds":["m"],"badge":{"label":"Disabled"}},
  {"enabled":true,"modelIds":["trial"],"badge":{"label":"Trial"},"schedule":{"validUntil":"2000-01-01T00:00:00Z"}}
 ]}`), &data)
	if err != nil {
		t.Fatal(err)
	}
	r := New(nil, nil)
	r.savePromotionMetadata(&pool.Account{UID: "a", Profile: "cn-cli"}, data)
	a := map[string]any{"uid": "a", "profile": "cn-cli", "enabled": true}
	b := map[string]any{"uid": "b", "profile": "cn-cli", "enabled": true}
	got := r.Promotions("m", []map[string]any{a, b})
	if len(got) != 1 || got[0].Label != "Night offer" || !got[0].Partial || got[0].Region != "domestic" {
		t.Fatalf("scope/date filtering or reminder dedup failed: %+v", got)
	}
	if len(r.Promotions("paid", []map[string]any{a})) != 0 {
		t.Fatal("trial target became a paid fallback or inherited an offer")
	}
	trial := r.Promotions("trial", []map[string]any{a})
	if len(trial) != 1 || trial[0].TrialDays != 14 {
		t.Fatal("enrollment deadline incorrectly ended an existing personal trial", trial)
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), "icon") || strings.Contains(string(encoded), "action") || strings.Contains(string(encoded), "factor") {
		t.Fatal("client actions or price mutations leaked into display metadata")
	}
	if r.Promotions("m", []map[string]any{a})[0].Partial {
		t.Fatal("unrelated accounts affected a scoped user's display")
	}
	b["enabled"] = false
	if r.Promotions("m", []map[string]any{a, b})[0].Partial {
		t.Fatal("disabled account counted as an applicable account")
	}
	got[0].Daily[0].Start = "mutated"
	if r.Promotions("m", []map[string]any{a})[0].Daily[0].Start == "mutated" {
		t.Fatal("caller mutated cached promotion schedule")
	}
	r.savePromotionMetadata(&pool.Account{UID: "a", Profile: "cn-cli"}, map[string]any{})
	if len(r.Promotions("m", []map[string]any{a})) != 0 {
		t.Fatal("successful empty metadata did not remove an old offer")
	}
	r.savePromotionMetadata(&pool.Account{UID: "a", Profile: "cn-cli"}, data)
	snapshot := r.promotions[[2]string{"cn-cli", "a"}]
	snapshot.expires = time.Now().Add(-time.Second)
	r.promotions[[2]string{"cn-cli", "a"}] = snapshot
	if len(r.Promotions("m", []map[string]any{a})) != 0 {
		t.Fatal("expired metadata advertised an offer")
	}
}

func TestDesktopPromotionQueryCannotChangeInferenceCatalog(t *testing.T) {
	failed := false
	var desktopCalls int
	headers := map[string]string{"X-IDE-Type": "CLI", "x-client-platform": "cli", "Authorization": "Bearer synthetic"}
	r := NewWithCatalogClient(nil, nil, &http.Client{Transport: isolatedCatalogTransport(func(req *http.Request) (*http.Response, error) {
		desktopCalls++
		if req.URL.Host != "copilot.tencent.com" || req.URL.Path != "/v3/config" || req.Header.Get("X-IDE-Type") != "WorkBuddy" || req.Header.Get("x-client-platform") != "" || req.Header.Get("Authorization") != "Bearer synthetic" {
			t.Fatal("metadata request changed endpoint, dropped auth or retained CLI platform")
		}
		if failed {
			return catalogResponse(req, 503, ""), nil
		}
		return catalogResponse(req, 200, `{"code":0,"data":{"models":[{"id":"desktop-only","credits":0}],"agents":[{"name":"cli","models":["desktop-only"]}],"modelPromotions":[{"enabled":true,"modelIds":["m"],"badge":{"label":"Offer"}}]}}`), nil
	})})
	a := &pool.Account{UID: "a", Profile: "cn-cli"}
	r.refreshPromotionMetadata(context.Background(), a, "https://copilot.tencent.com/v3/config", headers)
	if desktopCalls != 1 || headers["X-IDE-Type"] != "CLI" || headers["x-client-platform"] != "cli" {
		t.Fatal("inference headers were mutated or metadata was not fetched")
	}
	if len(r.models) != 0 || len(r.AccountCosts("cn-cli", "a")) != 0 || len(r.promotions[[2]string{"cn-cli", "a"}].models["m"]) != 1 {
		t.Fatal("desktop whitelist/price contaminated inference catalog, or offer missing")
	}
	failed = true
	r.refreshPromotionMetadata(context.Background(), a, "https://copilot.tencent.com/v3/config", headers)
	if desktopCalls != 2 || len(r.models) != 0 {
		t.Fatal("metadata failure changed the model catalog")
	}
}

func TestCachedModelIndexRefreshAndSnapshot(t *testing.T) {
	r := &Registry{models: []map[string]any{{"id": "old", "max_output_tokens": 100, "credits_by_region": map[string]float64{"domestic": 1}}}}
	entry, ready := r.CachedModel("old")
	if !ready || entry == nil || r.MaxOutputTokens("old") != 100 || r.CreditsByRegion("old")["domestic"] != 1 {
		t.Fatal("indexed facts missing")
	}
	entry["id"] = "mutated"
	if _, ready := r.CachedModel("missing"); !ready {
		t.Fatal("unknown model confused with empty catalog")
	}
	r.mu.Lock()
	r.models = []map[string]any{{"id": "new"}}
	r.modelIndex = nil
	r.mu.Unlock()
	if entry, _ := r.CachedModel("old"); entry != nil {
		t.Fatal("old model survived publication")
	}
	if entry, _ := r.CachedModel("new"); entry == nil {
		t.Fatal("new model absent")
	}
}

func BenchmarkCatalogLookup(b *testing.B) {
	r := &Registry{}
	for i := 0; i < 300; i++ {
		r.models = append(r.models, map[string]any{"id": fmt.Sprintf("model-%d", i), "name": "test", "account_uids": []string{"a", "b"}})
	}
	r.CachedModel("model-299")
	b.Run("whole_snapshot", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for _, entry := range r.ListCached() {
				if entry["id"] == "model-299" {
					break
				}
			}
		}
	})
	b.Run("indexed_entry", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			r.CachedModel("model-299")
		}
	})
}

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
func TestAccountBillingCostsStaySeparateAndFollowCatalogRefresh(t *testing.T) {
	revoked := false
	p := pool.New(map[string]pool.Credential{
		"regular": isolatedCatalogCredential{catalogTestCredential{profile: "cn-cli"}, "regular"},
		"offer":   isolatedCatalogCredential{catalogTestCredential{profile: "cn-work"}, "offer"},
	}, "")
	r := NewWithCatalogClient(p, nil, &http.Client{Transport: isolatedCatalogTransport(func(req *http.Request) (*http.Response, error) {
		if revoked {
			return catalogResponse(req, 200, catalogBody()), nil
		}
		price := "1"
		if req.Header.Get("X-Test-UID") == "offer" {
			price = "0"
		}
		body := strings.Replace(catalogBody("m"), `"name":"m"`, `"name":"m","credits":`+price, 1)
		return catalogResponse(req, 200, body), nil
	})})
	for _, account := range p.Accounts() {
		if len(r.fetchOneContext(context.Background(), account)) != 1 {
			t.Fatal("catalog missing")
		}
	}
	regular, offer := r.AccountCost("cn-cli", "regular", "m"), r.AccountCost("cn-work", "offer", "m")
	if regular == nil || *regular != 1 || offer == nil || *offer != 0 {
		t.Fatal(regular, offer)
	}
	snapshot := r.AccountCosts("cn-work", "offer")
	snapshot["m"] = 999
	*offer = 999
	if got := r.AccountCost("cn-work", "offer", "m"); got == nil || *got != 0 {
		t.Fatal("snapshot mutated internal prices")
	}
	if r.AccountCost("cn-cli", "offer", "m") != nil {
		t.Fatal("profile mixed")
	}
	revoked = true
	for _, account := range p.Accounts() {
		r.fetchOneContext(context.Background(), account)
	}
	if r.AccountCost("cn-cli", "regular", "m") != nil {
		t.Fatal("revoked quote retained")
	}
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

func TestRefreshFeedbackPartialFailureRevocationAndCancellableLock(t *testing.T) {
	phase := 0
	p := pool.New(map[string]pool.Credential{"uid": catalogTestCredential{profile: "cn-cli"}}, "")
	tr := isolatedCatalogTransport(func(req *http.Request) (*http.Response, error) {
		if phase == 3 || phase == 1 && req.URL.Path != "/v3/config" {
			return catalogResponse(req, 503, `{}`), nil
		}
		body := catalogBody()
		if phase < 2 {
			body = `{"data":{"models":[{"id":"test-model"}],"agents":[{"name":"cli","models":["test-model"]}]}}`
		}
		return catalogResponse(req, 200, body), nil
	})
	r := NewWithCatalogClient(p, nil, &http.Client{Transport: tr})
	initial, err := r.RefreshWithStatus(context.Background())
	if err != nil || r.Stale() || len(initial) < 2 {
		t.Fatal(initial, err)
	}
	phase = 1
	if _, err = r.RefreshWithStatus(context.Background()); !errors.Is(err, ErrPartialRefresh) || !r.Stale() {
		t.Fatal("partial failure hidden", err)
	}
	phase = 2
	current, err := r.RefreshWithStatus(context.Background())
	if err != nil || r.Stale() {
		t.Fatal(err)
	}
	for _, m := range current {
		if m["id"] == "test-model" {
			t.Fatal("revoked model retained")
		}
	}
	phase = 3
	if _, err = r.RefreshWithStatus(context.Background()); !errors.Is(err, ErrRefreshFailed) || !r.Stale() {
		t.Fatal("failure reported as success", err)
	}
	r.refreshMu.Lock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	done := make(chan error, 1)
	go func() { _, err := r.RefreshWithStatus(ctx); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Error(err)
		}
	case <-time.After(time.Second):
		t.Error("lock ignored cancellation")
	}
	cancel()
	r.refreshMu.Unlock()
}

func TestMediaModelsOutsideChatWhitelistRemainTypedAndUnpriced(t *testing.T) {
	p := pool.New(map[string]pool.Credential{"a": catalogTestCredential{profile: "cn-cli"}}, "")
	r := NewWithCatalogClient(p, nil, &http.Client{Transport: isolatedCatalogTransport(func(req *http.Request) (*http.Response, error) {
		return catalogResponse(req, 200, `{"code":0,"data":{"agents":[{"name":"cli","models":["chat"]}],"models":[{"id":"chat","credits":0},{"id":"draw","tags":["text-to-image"]},{"id":"movie","tags":["text-to-video"],"videoGeneration":{"minSeconds":3,"maxSeconds":15,"resolutions":["720P"]}}]}}`), nil
	})})
	r.Refresh()
	for id, kind := range map[string]string{"draw": "image", "movie": "video"} {
		m, _ := r.CachedModel(id)
		if m == nil || m["model_type"] != kind || m["vision"] != false || m["credits"] != nil {
			t.Fatalf("invalid media metadata: %#v", m)
		}
		if len(r.CreditsByRegion(id)) != 0 {
			t.Fatal("unknown media price became free")
		}
		own := r.AccountMedia("a", id)
		if own == nil {
			t.Fatal("missing account capability")
		}
		own["model_type"] = "mutated"
		if r.AccountMedia("a", id)["model_type"] != kind {
			t.Fatal("shared account metadata mutated")
		}
	}
}
