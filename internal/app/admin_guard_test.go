package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"work2api/internal/config"
)

func TestDeploymentTokenDefaults(t *testing.T) {
	previousRoot := config.PackageRoot
	config.PackageRoot = t.TempDir()
	t.Cleanup(func() { config.PackageRoot = previousRoot })
	t.Setenv("ADMIN_TOKEN", "")
	t.Setenv("ALLOW_EXTERNAL_HOST", "")
	t.Setenv("ADMIN_COOKIE_SECURE", "")
	for _, tc := range []struct {
		name, token, external, secure  string
		args                           []string
		wantExternal, wantSecure, auto bool
	}{
		{name: "local defaults", auto: true},
		{name: "token only", token: "test-token", wantExternal: true, wantSecure: true, auto: true},
		{name: "flag token", args: []string{"-admin-token", "test-token"}, wantExternal: true, wantSecure: true, auto: true},
		{name: "private host override", token: "test-token", external: "false", wantSecure: true, auto: true},
		{name: "legacy external override", external: "1", wantExternal: true, wantSecure: true, auto: true},
		{name: "explicit insecure LAN", token: "test-token", secure: "false", wantExternal: true},
		{name: "explicit secure", token: "test-token", secure: "true", wantExternal: true, wantSecure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ADMIN_TOKEN", tc.token)
			t.Setenv("ALLOW_EXTERNAL_HOST", tc.external)
			t.Setenv("ADMIN_COOKIE_SECURE", tc.secure)
			if tc.name == "local defaults" {
				_ = os.Unsetenv("ALLOW_EXTERNAL_HOST")
				_ = os.Unsetenv("ADMIN_COOKIE_SECURE")
			}
			cfg := config.Load(tc.args)
			if cfg.AllowExternalHost != tc.wantExternal || cfg.AdminCookieSecure != tc.wantSecure || cfg.AdminCookieSecureAuto != tc.auto {
				t.Fatalf("unexpected deployment policy: external=%t secure=%t auto=%t", cfg.AllowExternalHost, cfg.AdminCookieSecure, cfg.AdminCookieSecureAuto)
			}
		})
	}
}

func TestAutomaticDeploymentOriginPolicy(t *testing.T) {
	srv := &Server{o: &Orchestrator{cfg: &config.Config{AdminToken: "test-token", AdminCookieSecure: true, AdminCookieSecureAuto: true}}}
	for _, tc := range []struct {
		name, url, peer string
		secure          bool
	}{
		{"local HTTP", "http://localhost:8787", "127.0.0.1:10000", false},
		{"SSH tunnel HTTP", "http://127.0.0.1:18788", "127.0.0.1:10000", false},
		{"IPv6 local HTTP", "http://[::1]:8787", "[::1]:10000", false},
		{"IPv6 without port", "http://[::1]", "[::1]:10000", false},
		{"public HTTP via proxy", "http://gateway.example.com", "127.0.0.1:10000", true},
		{"spoofed localhost from remote", "http://localhost:8787", "192.0.2.1:10000", true},
		{"local HTTPS", "https://localhost:8787", "127.0.0.1:10000", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, tc.url+"/portal/api/auth/login", nil)
			r.RemoteAddr = tc.peer
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			r.Header.Set("Origin", tc.url)
			r.Header.Set("X-Forwarded-Proto", "http")
			r.Header.Set("X-Real-IP", "127.0.0.1")
			if got := srv.browserCookieSecure(r); got != tc.secure {
				t.Fatalf("cookie secure=%t, want %t", got, tc.secure)
			}
			w := httptest.NewRecorder()
			srv.setPortalCookie(w, r, "test-session")
			cookies := w.Result().Cookies()
			if len(cookies) != 1 || cookies[0].Secure != tc.secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
				t.Fatal("portal cookie violates deployment policy")
			}
			wantOrigin := r.TLS != nil || !tc.secure
			if srv.adminOriginAllowed(r, true) != wantOrigin {
				t.Fatal("origin check disagrees with transport policy")
			}
			r.Header.Set("Origin", "https://attacker.example.com")
			if srv.adminOriginAllowed(r, true) {
				t.Fatal("foreign origin allowed")
			}
		})
	}
	srv.o.cfg.AdminCookieSecureAuto = false
	r := httptest.NewRequest(http.MethodPost, "http://localhost:8787/", nil)
	r.RemoteAddr = "127.0.0.1:10000"
	if !srv.browserCookieSecure(r) {
		t.Fatal("explicit Secure policy was bypassed")
	}
}

func TestAgentCapacityDefaultsFollowRequestSize(t *testing.T) {
	previousRoot := config.PackageRoot
	config.PackageRoot = t.TempDir()
	t.Cleanup(func() { config.PackageRoot = previousRoot })
	for _, key := range []string{"MAX_REQUEST_BYTES", "REQUEST_BODY_BUDGET", "MODEL_BUFFER_BUDGET_BYTES", "MODEL_MEMORY_GUARD", "MAX_RESPONSE_BYTES", "MAX_CONCURRENT_REQUESTS", "MAX_JSON_ITEMS", "MAX_JSON_DEPTH"} {
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	cfg := config.Load(nil)
	if cfg.MaxRequestBytes != 256<<20 || cfg.MaxResponseBytes != 128<<20 || cfg.RequestBodyBudget != 0 || cfg.MaxJSONItems != config.DefaultMaxJSONItems || cfg.MaxJSONDepth != config.DefaultMaxJSONDepth || cfg.ModelMemoryGuard {
		t.Fatal("ordinary Agent requests retain restrictive defaults")
	}
	if cfg.ModelBufferBudgetBytes != 4*cfg.MaxResponseBytes*int64(cfg.MaxConcurrentRequests) {
		t.Fatal("response budget cannot cover configured execution capacity")
	}
	t.Setenv("MAX_REQUEST_BYTES", "134217728")
	cfg = config.Load(nil)
	if cfg.RequestBodyBudget != 0 {
		t.Fatal("implicit body quota was restored")
	}
	t.Setenv("REQUEST_BODY_BUDGET", "33554432")
	t.Setenv("MODEL_BUFFER_BUDGET_BYTES", "67108864")
	t.Setenv("MODEL_MEMORY_GUARD", "true")
	cfg = config.Load(nil)
	if cfg.RequestBodyBudget != 32<<20 || cfg.ModelBufferBudgetBytes != 64<<20 || !cfg.ModelMemoryGuard {
		t.Fatal("operator capacity overrides were ignored")
	}
}

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

func TestLoopbackManagementRejectsCrossOriginWrites(t *testing.T) {
	s := &Server{o: &Orchestrator{cfg: &config.Config{AdminCookieSecureAuto: true}}}
	for _, path := range []string{"/admin/settings", "/admin/portal/bootstrap"} {
		for _, tc := range []struct {
			origin, site string
			want         int
		}{
			{"http://127.0.0.1:8787", "same-origin", 200},
			{"", "", 200},
			{"http://127.0.0.1:9999", "same-site", 403},
			{"https://attacker.example", "cross-site", 403},
			{"null", "", 403},
			{"http://127.0.0.1:8787/path", "", 403},
			{"", "cross-site", 403},
		} {
			t.Run(path+"/"+tc.origin+tc.site, func(t *testing.T) {
				called := false
				h := s.adminGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(200) }))
				r := httptest.NewRequest("POST", "http://127.0.0.1:8787"+path, strings.NewReader(`{"model_aliases":"changed=model"}`))
				r.RemoteAddr = "127.0.0.1:40000"
				r.Header.Set("Content-Type", "text/plain")
				r.Header.Set("Origin", tc.origin)
				r.Header.Set("Sec-Fetch-Site", tc.site)
				r.Header.Set("X-Admin-Token", "unverified")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != tc.want || called != (tc.want == 200) {
					t.Fatal(w.Code, called, w.Body.String())
				}
			})
		}
	}
}
