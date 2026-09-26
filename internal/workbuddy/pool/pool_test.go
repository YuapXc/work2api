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

func TestPickExpiryBeatsCost(t *testing.T) {
	soon := nowSec() + 1*86400 // within expiryPriorityDays → urgent
	p := &Pool{accounts: []*Account{
		{UID: "free", Enabled: true},
		{UID: "paid", Enabled: true, CreditsExpireAt: &soon},
	}}
	cost := map[string]float64{"free": 0, "paid": 0.03}
	for i := 0; i < 50; i++ {
		got := p.Pick(map[string]bool{"free": true, "paid": true}, cost)
		if got == nil || got.UID != "paid" {
			t.Fatalf("soon-to-expire account must win over cheaper one, got %v", got)
		}
	}
}

func TestPickCostBlindWhenNil(t *testing.T) {
	p := &Pool{accounts: []*Account{{UID: "a", Enabled: true}, {UID: "b", Enabled: true}}}
	if got := p.Pick(map[string]bool{"a": true, "b": true}, nil); got == nil {
		t.Fatal("cost-blind pick should still return an account")
	}
}
