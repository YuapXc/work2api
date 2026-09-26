package pool

import "testing"

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
		got := p.Pick(map[string]bool{"free": true, "paid": true}, cost)
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
		got := p.Pick(map[string]bool{"free": true, "paid": true}, cost)
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
		got := p.Pick(map[string]bool{"fresh": true, "expiring": true}, cost)
		if got == nil || got.UID != "expiring" {
			t.Fatalf("within same-cost group, expiring should burn first, got %v", got)
		}
	}
}

func TestPickCostBlindWhenNil(t *testing.T) {
	p := &Pool{accounts: []*Account{{UID: "a", Enabled: true}, {UID: "b", Enabled: true}}}
	if got := p.Pick(map[string]bool{"a": true, "b": true}, nil); got == nil {
		t.Fatal("cost-blind pick should still return an account")
	}
}
