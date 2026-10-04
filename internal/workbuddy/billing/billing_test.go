package billing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

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
			if err != nil || res.OK != tc.ok || res.Already != tc.already || calls != 1 {
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

func ptr(v float64) *float64 { return &v }
