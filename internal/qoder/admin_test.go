package qoder

import (
	"context"
	"testing"
	"work2api/internal/core/provider"
	"work2api/internal/qoder/account"
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
