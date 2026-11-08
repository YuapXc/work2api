package billing

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestModernCreditSummaryDoesNotDuplicateDetails(t *testing.T) {
	for _, groupsOnly := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var payload any = map[string]any{"Accounts": []any{}}
			switch r.URL.Path {
			case "/billing/meter/get-user-resource-summary":
				if groupsOnly {
					payload = map[string]any{"CreditGroups": []any{map[string]any{"Kind": "plan_base", "Name": "Plan", "CycleRemainCapacity": "0.5", "CycleTotalCapacity": "1"}, map[string]any{"Kind": "benefit", "Name": "Bonus", "CycleRemainCapacity": "0.25", "CycleTotalCapacity": "2"}}}
				} else {
					payload = map[string]any{"Packages": []any{map[string]any{"PackageCode": "base", "CycleRemainCapacity": "0.25", "CycleTotalCapacity": "1"}, map[string]any{"PackageCode": "prepaid", "CycleRemainCapacity": "2.25"}}, "CreditGroups": []any{map[string]any{"Kind": "plan_base", "CycleRemainCapacity": "999", "CycleTotalCapacity": "999"}}}
				}
			case "/billing/meter/get-user-resource-paid-packages":
				payload = map[string]any{"Accounts": []any{map[string]any{"PackageCode": "base", "CapacityRemain": 100, "CapacityRemainPrecise": "0.25", "CapacitySizePrecise": "1"}, map[string]any{"PackageCode": "bonus", "CapacityRemainPrecise": "0.125", "CapacitySizePrecise": "2.5"}}}
			}
			json.NewEncoder(w).Encode(map[string]any{"code": 0, "data": payload})
		}))
		defer srv.Close()
		result, err := FetchCredits(context.Background(), checkinCredential{srv.URL})
		if err != nil {
			t.Fatal(err)
		}
		if groupsOnly {
			if result.Remain != 0.75 || result.Total != 3 || result.UnknownTotal {
				t.Fatal("aggregate groups counted twice", result)
			}
			continue
		}
		if result.Remain != 2.625 || result.Total != 3.5 || !result.UnknownTotal || len(result.Packages) != 3 {
			t.Fatal("precision, summary supplement or dedup failed", result)
		}
		unknown := 0
		for _, p := range result.Packages {
			if p["total"] == nil {
				unknown++
			}
		}
		if unknown != 1 {
			t.Fatal("unknown total fabricated", result.Packages)
		}
	}
	for _, v := range []any{"NaN", "Inf", math.Inf(1), "invalid", nil} {
		if knownCredit(v) != nil {
			t.Fatal("invalid receipt accepted")
		}
	}
	instant := time.Date(2026, 11, 7, 16, 0, 0, 0, time.UTC)
	if BusinessTime(instant).Format("2006-01-02") != "2026-11-08" || BusinessTime(instant.In(time.FixedZone("other", -8*3600))) != BusinessTime(instant) {
		t.Fatal("business date depends on host")
	}
}

func TestCheckinReceiptsAndUnsupportedActivities(t *testing.T) {
	for _, tc := range []struct {
		body, outcome string
		already       bool
		credit        *float64
	}{
		{`{"code":0,"data":{"credit":"0.125","streak_days":3}}`, "claimed", false, ptr(0.125)},
		{`{"code":0,"data":{"credit":"invalid"}}`, "claimed", false, nil},
		{`{"code":1001}`, "done", true, nil}, {`{"code":1002}`, "ineligible", false, nil}, {`{"code":1003}`, "inactive", false, nil},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(tc.body)) }))
		res, err := DailyCheckin(context.Background(), checkinCredential{srv.URL})
		srv.Close()
		if err != nil || res.Outcome != tc.outcome || res.Already != tc.already || (res.Credit == nil) != (tc.credit == nil) || res.Credit != nil && *res.Credit != *tc.credit {
			t.Fatal(res, err)
		}
	}
	for _, valid := range []bool{false, true} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v2/billing/meter/daily-checkin" {
				w.WriteHeader(401)
				w.Write([]byte(`{}`))
				return
			}
			if valid {
				w.Write([]byte(`{"code":0,"data":{"Packages":[]}}`))
			} else {
				w.WriteHeader(401)
				w.Write([]byte(`{}`))
			}
		}))
		res, err := DailyCheckin(context.Background(), checkinCredential{srv.URL})
		srv.Close()
		if valid && (err != nil || res.Outcome != "ineligible") || !valid && err == nil {
			t.Fatal("sign-in and activity rejection confused", res, err)
		}
	}
}

type checkinCredential struct{ endpoint string }

func (c checkinCredential) Endpoint() string                       { return c.endpoint }
func (c checkinCredential) GetHeaders() (map[string]string, error) { return map[string]string{}, nil }

func TestDailyCheckinBoundaries(t *testing.T) {
	for _, tc := range []struct {
		body        string
		status      int
		ok, already bool
	}{
		{`{"code":0}`, 200, true, false}, {`{"code":0}`, 503, false, false},
		{`{"code":null}`, 200, false, false}, {`{"code":"bad"}`, 200, false, false},
		{`{"code":1,"msg":"checkin failed"}`, 200, false, false},
		{`{"code":1,"msg":"Already checked in today"}`, 200, false, true},
		{`{"code":1,"msg":"rate limited"}`, 429, false, false},
	} {
		t.Run(tc.body, func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			res, err := DailyCheckin(context.Background(), checkinCredential{srv.URL})
			wantErr := tc.status >= 500 || tc.status == 429
			if (err != nil) != wantErr || res.OK != tc.ok || res.Already != tc.already || calls != 1 {
				t.Fatal(res, err, calls)
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"code":1,"msg":"请求处理中"}`))
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if _, err := DailyCheckin(ctx, checkinCredential{srv.URL}); err == nil {
		t.Fatal("processing retry ignored cancellation")
	}
}

func TestBillingRejectsHTTPFailureEvenWithSuccessPayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"code":0,"data":{"remain":0}}`))
	}))
	defer srv.Close()
	if _, err := postJSON(context.Background(), checkinCredential{srv.URL}, srv.URL, map[string]any{}); err == nil {
		t.Fatal("HTTP failure became valid billing data")
	}
}

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

func TestPackagesKeepSeparateBalancesAndUseBillingTimezone(t *testing.T) {
	first := time.Date(2026, 11, 7, 16, 0, 0, 0, time.UTC)
	later := first.Add(24 * time.Hour)
	packages := summarizePackages([]any{
		map[string]any{"PackageCode": "bonus", "CapacityRemain": 10.0, "CapacitySize": 10.0, "ExpiredTime": "2026-11-08 00:00:00"},
		map[string]any{"PackageCode": "bonus", "CapacityRemain": 30.0, "CapacitySize": 30.0, "CycleEndTime": "2026-11-08 00:00:00", "DeductionEndTime": float64(later.UnixMilli())},
	})
	if len(packages) != 2 || packages[0]["remain"] != 30.0 || packages[0]["expire_at"] != float64(later.Unix()) ||
		packages[1]["remain"] != 10.0 || packages[1]["expire_at"] != float64(first.Unix()) {
		t.Fatalf("packages: %#v", packages)
	}
}

func ptr(v float64) *float64 { return &v }
