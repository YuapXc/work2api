package app

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// sessionTTL is how long an admin session cookie stays valid (24h per user
// decision). Sliding: each authenticated request pushes expiry forward.
const sessionTTL = 24 * time.Hour

// cookieName is the HttpOnly session cookie for the WebUI admin surface.
const cookieName = "w2a_admin_session"

// maxSessions caps the in-memory session table (single-operator deployments);
// oldest-expiring sessions are evicted when exceeded.
const maxSessions = 16

// adminSessionEntry is one signed-in operator. Token is a 32-byte random value;
// only its SHA-256 is meaningful for comparison, but with an in-memory table
// the raw token is simply stored (process restart invalidates everything,
// which is the intended fail-safe).
type adminSessionEntry struct {
	expires time.Time
}

// adminSessionManager issues and validates admin login sessions in memory. Nothing
// is persisted: restarting the gateway signs every browser out (fail-safe) and
// no session material ever touches disk.
type adminSessionManager struct {
	mu       sync.Mutex
	sessions map[string]adminSessionEntry
}

func newAdminSessionManager() *adminSessionManager {
	return &adminSessionManager{sessions: map[string]adminSessionEntry{}}
}

// newToken returns 32 random bytes hex-encoded (64 chars).
func newToken() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

// create signs in and returns the cookie value. Concurrent logins beyond
// maxSessions evict the soonest-expiring session.
func (m *adminSessionManager) create() string {
	token := newToken()
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sessions) >= maxSessions {
		oldest := ""
		var oldestExp time.Time
		for t, e := range m.sessions {
			if oldest == "" || e.expires.Before(oldestExp) {
				oldest, oldestExp = t, e.expires
				if oldestExp.Before(time.Now()) {
					break // already expired: perfect victim
				}
			}
		}
		delete(m.sessions, oldest)
	}
	m.sessions[token] = adminSessionEntry{expires: time.Now().Add(sessionTTL)}
	return token
}

// validate returns true if the token is a live session, sliding its expiry.
func (m *adminSessionManager) validate(token string) bool {
	if token == "" {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(e.expires) {
		delete(m.sessions, token)
		return false
	}
	e.expires = time.Now().Add(sessionTTL)
	m.sessions[token] = e
	return true
}

// drop removes one session (logout).
func (m *adminSessionManager) drop(token string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, token)
}

// loginRateLimiter is a tiny fixed-window limiter for POST /admin/login:
// per-IP attempt counter that resets after the window. Prevents unlimited
// brute-force of the ADMIN_TOKEN through the login endpoint.
type loginRateLimiter struct {
	mu      sync.Mutex
	attempt map[string]*loginWindow
}

type loginWindow struct {
	start time.Time
	count int
}

const (
	loginWindowLen = 10 * time.Minute
	loginWindowMax = 10 // attempts per window per IP
)

func newLoginRateLimiter() *loginRateLimiter {
	return &loginRateLimiter{attempt: map[string]*loginWindow{}}
}

// allow reports whether an attempt from ip may proceed.
func (l *loginRateLimiter) allow(ip string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.attempt[ip]
	if !ok || now.Sub(w.start) >= loginWindowLen {
		l.attempt[ip] = &loginWindow{start: now, count: 1}
		return true
	}
	w.count++
	// Opportunistic cleanup: drop long-stale entries so the map can't grow.
	if len(l.attempt) > 256 {
		for k, w2 := range l.attempt {
			if now.Sub(w2.start) >= loginWindowLen {
				delete(l.attempt, k)
			}
		}
	}
	return w.count <= loginWindowMax
}

// adminAuth resolves the admin auth state of a request: a valid session cookie
// OR a valid X-Admin-Token/Bearer header (kept for scripts) authenticates. The
// three return values mirror the legacy adminGuard's needs.
func (s *Server) adminAuth(r *http.Request) (sessionOK, headerOK bool) {
	if c, err := r.Cookie(cookieName); err == nil && c.Value != "" {
		sessionOK = s.sessions.validate(c.Value)
	}
	if header := r.Header.Get("X-Admin-Token"); header != "" {
		headerOK = subtle.ConstantTimeCompare([]byte(header), []byte(s.o.cfg.AdminToken)) == 1
	} else if a := r.Header.Get("Authorization"); strings.HasPrefix(a, "Bearer ") {
		headerOK = subtle.ConstantTimeCompare([]byte(strings.TrimSpace(a[7:])), []byte(s.o.cfg.AdminToken)) == 1
	}
	return sessionOK, headerOK
}

// handleAdminLogin exchanges an ADMIN_TOKEN for an HttpOnly session cookie.
// Rate limited per IP. Also serves the session check: with a valid cookie (or
// header) it returns 200 without checking the token, so the WebUI can probe
// session liveness with a plain GET-style POST of nothing.
func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	if s.o.cfg.AdminToken == "" {
		writeJSON(w, 400, map[string]any{"ok": false, "message": "未设置 ADMIN_TOKEN（本机回环模式无需登录）"})
		return
	}
	// Session probe / liveness check (no token in body): authenticated callers
	// get ok + csrf-free status without consuming a rate-limit slot.
	body, _ := readJSON(r)
	if len(body) == 0 {
		if sessionOK, headerOK := s.adminAuth(r); sessionOK || headerOK {
			writeJSON(w, 200, map[string]any{"ok": true, "probe": true})
			return
		}
		writeJSON(w, 401, map[string]any{"ok": false, "probe": true})
		return
	}
	// Real login: rate-limit, constant-time compare, issue cookie.
	ip := clientIP(r)
	if !s.loginLimiter.allow(ip) {
		writeJSON(w, 429, map[string]any{"ok": false, "message": "尝试次数过多，请 10 分钟后再试"})
		return
	}
	token, _ := body["token"].(string)
	if subtle.ConstantTimeCompare([]byte(token), []byte(s.o.cfg.AdminToken)) != 1 {
		time.Sleep(200 * time.Millisecond) // blunt brute-force cost
		writeJSON(w, 401, map[string]any{"ok": false, "message": "Token 无效"})
		return
	}
	value := s.sessions.create()
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleAdminLogout drops the session (cookie itself is cleared by the client
// or overwritten on next login).
func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.sessions.drop(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

// clientIP extracts the remote address host for rate limiting.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
