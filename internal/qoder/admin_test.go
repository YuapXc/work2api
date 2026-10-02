package qoder

import (
	"testing"
	"work2api/internal/qoder/account"
)

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
