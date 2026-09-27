package billing

import "testing"

// TestEarliestPackageExpiry: the account-level expiry is the soonest expiry
// among packages that still hold a balance; zero-balance packages and packages
// without a known expiry are ignored, and an all-empty/unknown set yields nil.
func TestEarliestPackageExpiry(t *testing.T) {
	f := func(v float64) float64 { return v }
	cases := []struct {
		name string
		pkgs []map[string]any
		want *float64
	}{
		{"nil when empty", nil, nil},
		{
			"soonest among funded",
			[]map[string]any{
				{"remain": 1000.0, "expire_at": f(2000)},
				{"remain": 500.0, "expire_at": f(1500)},
			},
			ptr(1500),
		},
		{
			"skips zero-balance even if it expires sooner",
			[]map[string]any{
				{"remain": 0.0, "expire_at": f(1000)},
				{"remain": 300.0, "expire_at": f(1800)},
			},
			ptr(1800),
		},
		{
			"skips packages with no known expiry (nil)",
			[]map[string]any{
				{"remain": 900.0, "expire_at": nil},
				{"remain": 200.0, "expire_at": f(2500)},
			},
			ptr(2500),
		},
		{
			"nil when nothing funded carries an expiry",
			[]map[string]any{
				{"remain": 0.0, "expire_at": f(1000)},
				{"remain": 700.0, "expire_at": nil},
			},
			nil,
		},
	}
	for _, c := range cases {
		got := earliestPackageExpiry(c.pkgs)
		switch {
		case got == nil && c.want == nil:
		case got == nil || c.want == nil:
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		case *got != *c.want:
			t.Errorf("%s: got %v, want %v", c.name, *got, *c.want)
		}
	}
}

func ptr(v float64) *float64 { return &v }
