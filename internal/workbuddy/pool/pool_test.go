package pool

import "testing"

func TestExhaustedAccountsNeverUseCooldownFallback(t *testing.T) {
	zero := 0.0
	p := &Pool{accounts: []*Account{{UID: "empty", Enabled: true, CreditsRemain: &zero}, {UID: "unknown", Enabled: true, CooldownUntil: nowSec() + 5}}}
	if got := p.Pick(map[string]bool{"empty": true}, nil, 7); got != nil {
		t.Fatal("exhausted account was revived", got)
	}
	if got := p.Pick(nil, nil, 7); got == nil || got.UID != "unknown" {
		t.Fatal("unknown credits lost short cooldown fallback", got)
	}
	positive := 1.0
	p.accounts[0].CreditsRemain = &positive
	if got := p.Pick(map[string]bool{"empty": true}, nil, 7); got == nil {
		t.Fatal("refreshed credits did not restore eligibility")
	}
}

func uids(as []*Account) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.UID
	}
	return out
}

func TestCheapestGroupExcludesUnknownAndPaid(t *testing.T) {
	a := &Account{UID: "free"}
	b := &Account{UID: "paid"}
	c := &Account{UID: "unknown"} // not in cost map
	g := cheapestGroup([]*Account{a, b, c}, map[string]float64{"free": 0, "paid": 0.03})
	if len(g) != 1 || g[0].UID != "free" {
		t.Fatalf("want [free] (min known, unknown last), got %v", uids(g))
	}
}

func TestCheapestGroupAllUnknownNoFilter(t *testing.T) {
	a := &Account{UID: "x"}
	b := &Account{UID: "y"}
	g := cheapestGroup([]*Account{a, b}, map[string]float64{})
	if len(g) != 2 {
		t.Fatalf("all-unknown must not filter (soft fallback), got %v", uids(g))
	}
}

func TestCheapestGroupTieKeepsBoth(t *testing.T) {
	a := &Account{UID: "a"}
	b := &Account{UID: "b"}
	g := cheapestGroup([]*Account{a, b}, map[string]float64{"a": 0, "b": 0})
	if len(g) != 2 {
		t.Fatalf("equal cost keeps both, got %v", uids(g))
	}
}

func TestPickPrefersCheapest(t *testing.T) {
	p := &Pool{accounts: []*Account{{UID: "free", Enabled: true}, {UID: "paid", Enabled: true}}}
	cost := map[string]float64{"free": 0, "paid": 0.03}
	for i := 0; i < 50; i++ {
		got := p.Pick(map[string]bool{"free": true, "paid": true}, cost, 7)
		if got == nil || got.UID != "free" {
			t.Fatalf("cost-aware pick must always choose free, got %v", got)
		}
	}
}

func TestPickCostBeatsExpiry(t *testing.T) {
	// 成本绝对优先：更便宜的账号即使对方额度快到期也胜出（对方到期额度可能作废）。
	soon := nowSec() + 1*86400 // paid 账号 1 天后到期
	p := &Pool{accounts: []*Account{
		{UID: "free", Enabled: true},
		{UID: "paid", Enabled: true, CreditsExpireAt: &soon},
	}}
	cost := map[string]float64{"free": 0, "paid": 0.03}
	for i := 0; i < 50; i++ {
		got := p.Pick(map[string]bool{"free": true, "paid": true}, cost, 7)
		if got == nil || got.UID != "free" {
			t.Fatalf("cost must outrank expiry: expected free, got %v", got)
		}
	}
}

func TestPickExpiryBreaksTieWithinCostGroup(t *testing.T) {
	// 同成本组内，快到期的先烧。
	soon := nowSec() + 1*86400
	p := &Pool{accounts: []*Account{
		{UID: "fresh", Enabled: true},
		{UID: "expiring", Enabled: true, CreditsExpireAt: &soon},
	}}
	cost := map[string]float64{"fresh": 0, "expiring": 0} // 同价
	for i := 0; i < 50; i++ {
		got := p.Pick(map[string]bool{"fresh": true, "expiring": true}, cost, 7)
		if got == nil || got.UID != "expiring" {
			t.Fatalf("within same-cost group, expiring should burn first, got %v", got)
		}
	}
}

func TestPickCostBlindWhenNil(t *testing.T) {
	p := &Pool{accounts: []*Account{{UID: "a", Enabled: true}, {UID: "b", Enabled: true}}}
	if got := p.Pick(map[string]bool{"a": true, "b": true}, nil, 7); got == nil {
		t.Fatal("cost-blind pick should still return an account")
	}
}

// pkgWith builds a one-package credit list for tests.
func pkgWith(remain, expireAt float64) []map[string]any {
	return []map[string]any{{"remain": remain, "expire_at": expireAt}}
}

// TestPickPrefersLargerExpiringPile: same cost, both expiring within the window
// at similar times, but one has far more balance about to vanish — it should be
// picked overwhelmingly so the bigger expiring pile is burned first (Option A).
func TestPickPrefersLargerExpiringPile(t *testing.T) {
	soonBig := nowSec() + 2*86400
	soonSmall := nowSec() + 2*86400
	p := &Pool{accounts: []*Account{
		{UID: "big", Enabled: true, CreditPackages: pkgWith(8000, soonBig)},
		{UID: "small", Enabled: true, CreditPackages: pkgWith(200, soonSmall)},
	}}
	cost := map[string]float64{"big": 0, "small": 0}
	bigHits := 0
	for i := 0; i < 400; i++ {
		if got := p.Pick(map[string]bool{"big": true, "small": true}, cost, 7); got != nil && got.UID == "big" {
			bigHits++
		}
	}
	// 期望命中率约 74%（big 权重 3.0 vs small 约 1.05，400 次期望 ~296 次、
	// σ≈8.8）。阈值 250 留出 ~5σ 裕度，避免统计性偶发失败；仍断言明显多数
	// （软偏置允许 small 偶尔胜出，不要求 100%）。
	if bigHits < 250 {
		t.Fatalf("larger expiring pile should dominate, big won %d/400", bigHits)
	}
}

// TestPickExpiringPileIgnoredOutsideWindow: a huge balance expiring far beyond
// the window must NOT pull traffic — only in-window expiring amounts bias burn.
func TestPickExpiringPileIgnoredOutsideWindow(t *testing.T) {
	farBig := nowSec() + 60*86400 // 60 天后，远超 7 天窗口
	soonSmall := nowSec() + 2*86400
	p := &Pool{accounts: []*Account{
		{UID: "farbig", Enabled: true, CreditPackages: pkgWith(9000, farBig)},
		{UID: "soonsmall", Enabled: true, CreditPackages: pkgWith(300, soonSmall)},
	}}
	cost := map[string]float64{"farbig": 0, "soonsmall": 0}
	soonHits := 0
	for i := 0; i < 400; i++ {
		if got := p.Pick(map[string]bool{"farbig": true, "soonsmall": true}, cost, 7); got != nil && got.UID == "soonsmall" {
			soonHits++
		}
	}
	// farbig is not urgent (outside window) so soonsmall alone forms the urgent
	// group and is always chosen.
	if soonHits != 400 {
		t.Fatalf("only the in-window expiring account should be urgent, soonsmall won %d/400", soonHits)
	}
}

func TestDomesticPreferenceCannotOverrideCostOrExpiry(t *testing.T) {
	cn := &Account{UID: "cn", Profile: "cn-cli", Enabled: true}
	intl := &Account{UID: "intl", Profile: "intl-work", Enabled: true}
	p := &Pool{accounts: []*Account{cn, intl}}
	cost := map[string]float64{"cn": 2, "intl": 1}
	for i := 0; i < 30; i++ {
		if got := p.Pick(nil, cost, 7); got != intl {
			t.Fatal("region overrode cost", got)
		}
	}
	soon := nowSec() + 86400
	intl.CreditsExpireAt = &soon
	cost["cn"] = 1
	for i := 0; i < 30; i++ {
		if got := p.Pick(nil, cost, 7); got != intl {
			t.Fatal("region overrode urgent expiry", got)
		}
	}
	if !domesticCostTie([]*Account{cn, intl}, cost) {
		t.Fatal("same-price regions not eligible")
	}
	if domesticCostTie([]*Account{cn, intl}, nil) || domesticCostTie([]*Account{cn, intl}, map[string]float64{"cn": 1}) || domesticCostTie([]*Account{cn, intl}, map[string]float64{"cn": 2, "intl": 1}) {
		t.Fatal("region preference requires equal known costs")
	}
	cn.Profile = "unknown"
	if domesticCostTie([]*Account{cn, intl}, cost) {
		t.Fatal("unknown profile classified as domestic")
	}
}
