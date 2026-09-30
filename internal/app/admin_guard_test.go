package app

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"work2api/internal/config"
)

// TestAdminGuard locks the /admin/* auth matrix: with a token set, every admin
// request must carry it; with no token, admin is loopback-only regardless of
// ALLOW_EXTERNAL_HOST; non-admin paths always pass through. The loopback check
// uses the peer address (RemoteAddr), never the client-controlled Host header.
func TestAdminGuard(t *testing.T) {
	srv := &Server{o: &Orchestrator{}, sessions: newAdminSessionManager(), loginLimiter: newLoginRateLimiter()}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	call := func(token, path, remoteAddr, host, header string) int {
		srv.o.cfg = &config.Config{AdminToken: token}
		h := srv.adminGuard(next)
		r := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
		r.Host = host
		r.RemoteAddr = remoteAddr
		if header != "" {
			r.Header.Set("X-Admin-Token", header)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	cases := []struct {
		name                string
		token, path, remote string
		host, header        string
		want                int
	}{
		{"no token, loopback peer ok", "", "/admin/accounts", "127.0.0.1:51000", "127.0.0.1:8787", "", 200},
		{"no token, remote peer blocked", "", "/admin/accounts", "10.0.0.5:51000", "10.0.0.5:8787", "", http.StatusForbidden},
		// spoofed Host header must NOT bypass the peer check
		{"no token, spoofed loopback Host blocked", "", "/admin/accounts", "10.0.0.5:51000", "127.0.0.1:8787", "", http.StatusForbidden},
		{"token set, correct header ok", "secret", "/admin/accounts", "10.0.0.5:51000", "10.0.0.5:8787", "secret", 200},
		{"token set, wrong header 401", "secret", "/admin/accounts", "10.0.0.5:51000", "10.0.0.5:8787", "nope", http.StatusUnauthorized},
		{"token set, missing header 401", "secret", "/admin/accounts", "127.0.0.1:51000", "127.0.0.1:8787", "", http.StatusUnauthorized},
		{"non-admin path always passes", "secret", "/v1/models", "10.0.0.5:51000", "10.0.0.5:8787", "", 200},
		{"login endpoint reachable without auth", "secret", "/admin/login", "10.0.0.5:51000", "10.0.0.5:8787", "", 200},
	}
	for _, c := range cases {
		if got := call(c.token, c.path, c.remote, c.host, c.header); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}
