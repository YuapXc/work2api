package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"work2api/internal/config"
)

// TestAdminGuard locks the /admin/* auth matrix: with a token set, every admin
// request must carry it; with no token, admin is loopback-only regardless of
// ALLOW_EXTERNAL_HOST; non-admin paths always pass through.
func TestAdminGuard(t *testing.T) {
	srv := &Server{o: &Orchestrator{}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	call := func(token, path, host, header string) int {
		srv.o.cfg = &config.Config{AdminToken: token}
		h := srv.adminGuard(next)
		r := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
		r.Host = host
		if header != "" {
			r.Header.Set("X-Admin-Token", header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	cases := []struct {
		name              string
		token, path, host string
		header            string
		want              int
	}{
		{"no token, loopback admin ok", "", "/admin/accounts", "127.0.0.1:8787", "", 200},
		{"no token, remote admin blocked", "", "/admin/accounts", "10.0.0.5:8787", "", http.StatusForbidden},
		{"token set, correct header ok", "secret", "/admin/accounts", "10.0.0.5:8787", "secret", 200},
		{"token set, wrong header 401", "secret", "/admin/accounts", "10.0.0.5:8787", "nope", http.StatusUnauthorized},
		{"token set, missing header 401", "secret", "/admin/accounts", "127.0.0.1:8787", "", http.StatusUnauthorized},
		{"non-admin path always passes", "secret", "/v1/models", "10.0.0.5:8787", "", 200},
	}
	for _, c := range cases {
		if got := call(c.token, c.path, c.host, c.header); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}
