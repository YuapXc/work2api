package qoder

import (
	"context"
	"crypto/sha256"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"work2api/internal/core/provider"
	"work2api/internal/qoder/account"
	"work2api/internal/qoder/bridge"
	"work2api/internal/qoder/checkin"
)

func TestGlobalAccountCheckinDoesNotSendTokenToCN(t *testing.T) {
	previous := account.DataRoot()
	account.SetDataRoot(t.TempDir())
	defer account.SetDataRoot(previous)
	a := &account.Account{ID: "global-checkin-fixture", Region: account.RegionGlobal, Name: "global"}
	if err := account.Save(a); err != nil {
		t.Fatal(err)
	}
	if err := account.SaveSecret(a.ID, "dt-global-fixture"); err != nil {
		t.Fatal(err)
	}
	results := checkin.CheckinAllContext(context.Background())
	if len(results) != 1 || results[0].Status != "unsupported" {
		t.Fatal(results)
	}
}

func TestAccountRowReflectsCredentialScopedCooldown(t *testing.T) {
	previous := account.DataRoot()
	account.SetDataRoot(t.TempDir())
	t.Cleanup(func() { account.SetDataRoot(previous) })
	id := "health-fixture"
	acct := &account.Account{ID: id, Region: account.RegionCN}
	secret := `{"device_token":"token","refresh_token":"first"}`
	r := &Runtime{quotaInflight: map[string]bool{id: true}, accountCooldowns: map[[32]byte]time.Time{}}
	until := time.Now().Add(time.Minute)
	r.accountCooldowns[modelCatalogKey(acct, secret)] = until
	row := r.accountRow(id, "fixture", string(acct.Region), "native", "device", true, secret)
	if row["healthy"] != false || row["has_secret"] != true || row["cooldown_until"] != float64(until.UnixMilli())/1000 {
		t.Fatal("cooldown not reflected", row)
	}
	for _, current := range []string{`{"device_token":"token","refresh_token":"rotated"}`, ""} {
		row = r.accountRow(id, "fixture", string(acct.Region), "native", "device", true, current)
		if row["healthy"] != (current != "") || row["cooldown_until"] != nil {
			t.Fatal("stale credential cooldown leaked", row)
		}
	}
	r.accountCooldowns[modelCatalogKey(acct, secret)] = time.Now().Add(-time.Second)
	row = r.accountRow(id, "fixture", string(acct.Region), "native", "device", true, secret)
	if row["healthy"] != true || row["cooldown_until"] != nil {
		t.Fatal("expired cooldown remained unhealthy", row)
	}
	localID := "qoder-local-cn"
	r.quotaInflight[localID] = true
	r.accountCooldowns[modelCatalogKey(&account.Account{ID: localID, Region: account.NormalizeRegion("cn")}, "local-token")] = until
	row = r.accountRow(localID, "local", "cn", "local", "device", false, "local-token")
	if row["healthy"] != false {
		t.Fatal("local cooldown identity mismatch", row)
	}
}

func TestCatalogEmptySnapshotAndAccountIsolation(t *testing.T) {
	previous := account.DataRoot()
	account.SetDataRoot(t.TempDir())
	t.Cleanup(func() { account.SetDataRoot(previous) })
	a := &account.Account{ID: "catalog-one", Region: account.Region("cn")}
	if err := account.Save(a); err != nil {
		t.Fatal(err)
	}
	if err := account.SaveSecret(a.ID, "test-secret"); err != nil {
		t.Fatal(err)
	}
	if err := account.SetActive(a.ID); err != nil {
		t.Fatal(err)
	}
	r := &Runtime{modelCacheLoaded: true, modelCacheKey: modelCatalogKey(a, "test-secret")}
	if got := r.Models(context.Background()); len(got) != 0 {
		t.Fatal("empty live snapshot revived fallback", got)
	}
	r.modelCache = []provider.CatalogModel{{ID: "qoder/authorized", Extra: map[string]any{"enable": true}}}
	r.markCatalogStale(r.modelCacheKey)
	got := r.Models(context.Background())
	if len(got) != 1 || got[0].Extra["catalog_stale"] != true {
		t.Fatal("last trusted snapshot not retained as stale", got)
	}
	got[0].Extra["enable"] = false
	if r.modelCache[0].Extra["enable"] != true {
		t.Fatal("catalog caller mutated runtime cache")
	}
	if err := account.SaveSecret(a.ID, "rotated-secret"); err != nil {
		t.Fatal(err)
	}
	got = r.Models(context.Background())
	if len(got) != 2 || got[0].ID == "qoder/authorized" || got[0].Extra["catalog_source"] != "fallback" {
		t.Fatal("credential rotation reused old catalog", got)
	}
}

func TestQuotaEntryIncludesEveryCreditBucket(t *testing.T) {
	q := &account.QuotaInfo{
		UserQuota:         &account.QuotaBucket{Used: 100, Total: 2000, Remaining: 1900, ResetTime: "2026-11-01T00:00:00Z"},
		AddonQuota:        &account.QuotaBucket{Used: 50, Total: 300, Remaining: 250},
		DedicatedPackages: []account.QuotaPackage{{Label: "Qwen 专属积分", Used: 258, Total: 2000, Remaining: 1742, ExpireAt: 1800000000000}},
	}
	e := buildQuotaEntry(q)
	if e.total != 4300 || e.remaining != 3892 || len(e.packages) != 3 {
		t.Fatalf("incomplete aggregate: %+v", e)
	}
	var total, remaining float64
	for _, p := range e.packages {
		total += p["total"].(float64)
		remaining += p["remain"].(float64)
	}
	if total != e.total || remaining != e.remaining {
		t.Fatal("details do not reconcile with account total")
	}
	if e.packages[0]["reset_time"] != q.UserQuota.ResetTime {
		t.Fatal("subscription reset was lost")
	}
	if _, ok := e.packages[0]["expire_at"]; ok {
		t.Fatal("reset mislabeled as expiry")
	}
	if e.packages[2]["expire_at"] != float64(1800000000) {
		t.Fatal("package expiry not converted from milliseconds")
	}
	if empty := buildQuotaEntry(&account.QuotaInfo{}); len(empty.packages) != 0 || empty.remaining != 0 {
		t.Fatal("absent buckets created phantom credits")
	}
}

type patExchangeTransport struct {
	body   string
	status int
}

func (f patExchangeTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: f.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(f.body)), Request: r}, nil
}
func TestPATRejectsInvalidExchangeWithoutChangingActiveAccount(t *testing.T) {
	root := account.DataRoot()
	account.SetDataRoot(t.TempDir())
	defer account.SetDataRoot(root)
	a := &account.Account{ID: "old", Name: "old", Active: true}
	if err := account.Save(a); err != nil {
		t.Fatal(err)
	}
	account.SaveSecret(a.ID, "original")
	transport := http.DefaultTransport
	defer func() { http.DefaultTransport = transport }()
	for _, body := range []string{`{}`, `{"id":"new"}`, `{"securityOauthToken":"session"}`, `{"code":401,"id":"new","securityOauthToken":"session"}`} {
		http.DefaultTransport = patExchangeTransport{body, 200}
		if _, err := New("error").AddAccountByToken(context.Background(), "fake-pat", map[string]any{"region": "cn"}); err == nil {
			t.Fatal("invalid exchange accepted", body)
		}
		list, _ := account.List()
		if len(list) != 1 || !list[0].Active || list[0].ID != "old" {
			t.Fatal("invalid import changed active state")
		}
	}
}
func TestPATReimportPreservesMetadataAndFailurePreservesSecret(t *testing.T) {
	root := account.DataRoot()
	account.SetDataRoot(t.TempDir())
	defer account.SetDataRoot(root)
	created := time.Now().Add(-time.Hour).UTC()
	a := &account.Account{ID: "123User", Name: "Alias", Region: account.RegionCN, AuthMode: "pat", APIMode: "anthropic", Tags: []string{"tag"}, SortOrder: 4, CreatedAt: created}
	if err := account.Save(a); err != nil {
		t.Fatal(err)
	}
	account.SaveSecret(a.ID, "old-pat")
	transport := http.DefaultTransport
	http.DefaultTransport = patExchangeTransport{`{"id":"123","name":"User","securityOauthToken":"session"}`, 200}
	defer func() { http.DefaultTransport = transport }()
	if _, err := New("error").AddAccountByToken(context.Background(), "new-pat", map[string]any{"region": "cn"}); err != nil {
		t.Fatal(err)
	}
	got, _ := account.Get(a.ID)
	if got.Name != "Alias" || got.APIMode != "anthropic" || got.SortOrder != 4 || !got.CreatedAt.Equal(created) || len(got.Tags) != 1 || !got.Active {
		t.Fatal("reimport erased metadata", got)
	}
	// Prevent saving the account before any credential is overwritten.
	path := filepath.Join(account.DataRoot(), "accounts", a.ID+".json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := New("error").AddAccountByToken(context.Background(), "bad-replacement", map[string]any{"region": "cn"}); err == nil {
		t.Fatal("filesystem failure ignored")
	}
	secret, _ := account.GetSecret(a.ID)
	if secret != "new-pat" {
		t.Fatal("failed reimport destroyed previous credential")
	}
}

func TestPrivateFailoverEligibilityAndAffinityInvalidation(t *testing.T) {
	oldRoot := account.DataRoot()
	account.SetDataRoot(t.TempDir())
	defer account.SetDataRoot(oldRoot)
	r := &Runtime{saltSet: true, bridges: map[string]*bridge.Bridge{}, bridgeKeys: map[string][32]byte{}, accountModels: map[[32]byte]accountModelSnapshot{}}
	r.localOnce.Do(func() {})
	save := func(id string, region account.Region, model string) *account.Account {
		a := &account.Account{ID: id, Region: region}
		if e := account.Save(a); e != nil {
			t.Fatal(e)
		}
		secret := "secret-" + id
		if e := account.SaveSecret(id, secret); e != nil {
			t.Fatal(e)
		}
		r.bridges[id] = &bridge.Bridge{}
		r.bridgeKeys[id] = sha256.Sum256([]byte(string(region) + "\x00" + secret))
		r.accountModels[modelCatalogKey(a, secret)] = accountModelSnapshot{map[string]bool{model: true}, time.Now().Add(time.Minute)}
		return a
	}
	primary := save("primary", account.RegionCN, "qfmodel")
	save("a-global", account.RegionGlobal, "qfmodel")
	save("b-other-model", account.RegionCN, "other")
	backup := save("c-backup", account.RegionCN, "qfmodel")
	candidate, e := r.nextAccountBridge(context.Background(), account.RegionCN, "qfmodel", map[string]bool{primary.ID: true})
	if e != nil || candidate == nil || candidate.account.ID != backup.ID {
		t.Fatal(candidate, e)
	}
	req := provider.ServeRequest{Headers: http.Header{"Authorization": []string{"Bearer key1"}, "X-Session-Id": []string{"session"}}, Payload: map[string]any{"model": "qoder/qfmodel"}}
	key, ok := requestAffinityKey(req)
	preferred := modelCatalogKey(primary, "secret-primary")
	r.rememberAffinity(key, ok, preferred, backup, "secret-c-backup")
	if a, _ := r.affinityAccount(key, ok, preferred); a == nil || a.ID != backup.ID {
		t.Fatal("affinity missing")
	}
	req.Headers.Set("Authorization", "Bearer key2")
	other, _ := requestAffinityKey(req)
	if key == other {
		t.Fatal("affinity crosses authenticated keys")
	}
	if a, _ := r.affinityAccount(key, ok, [32]byte{}); a != nil {
		t.Fatal("active change did not invalidate affinity")
	}
	if e := account.SetGatewayHidden(backup.ID, true); e != nil {
		t.Fatal(e)
	}
	if a, _ := r.affinityAccount(key, ok, preferred); a != nil {
		t.Fatal("hidden account revived")
	}
	if c, e := r.nextAccountBridge(context.Background(), account.RegionCN, "qfmodel", map[string]bool{primary.ID: true}); e != nil || c != nil {
		t.Fatal("hidden account selected", c, e)
	}
	if e := account.SetGatewayHidden(backup.ID, false); e != nil {
		t.Fatal(e)
	}
	if e := account.SaveSecret(backup.ID, "reimported"); e != nil {
		t.Fatal(e)
	}
	if a, _ := r.affinityAccount(key, ok, preferred); a != nil {
		t.Fatal("old identity revived")
	}
	r.coolAccount(primary, "secret-primary", &bridge.UpstreamError{Status: 429, RetryAfter: 2 * time.Minute})
	if time.Until(r.accountCooldowns[preferred]) < 119*time.Second {
		t.Fatal("Retry-After ignored")
	}
	for _, cause := range []error{context.Canceled, &net.DNSError{Err: "DNS failed"}, bridge.WrapTransportError(&net.DNSError{Err: "DNS timeout", IsTimeout: true}), &bridge.UpstreamError{Status: 401}, &bridge.UpstreamError{Status: 400, Detail: "invalid_parameter_error"}, &bridge.UpstreamError{Status: 503, Detail: "DataInspectionFailed"}} {
		if canFailover(cause) {
			t.Fatal("nonretryable switched", cause)
		}
	}
	for _, cause := range []error{&bridge.UpstreamError{Status: 429}, &bridge.UpstreamError{Status: 503}, io.ErrUnexpectedEOF} {
		if !canFailover(cause) {
			t.Fatal("retryable did not switch", cause)
		}
	}
}
