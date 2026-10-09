package app

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"work2api/internal/portal/portalauth"
	"work2api/internal/store"
)

// sessionTTL is how long an admin session cookie stays valid (24h per user
// decision). Sliding: each authenticated request pushes expiry forward.
const sessionTTL = 24 * time.Hour

// cookieName is the HttpOnly session cookie for the WebUI admin surface.
const cookieName = "w2a_admin_session"

// maxSessions bounds memory across operators; each identity also has its own cap.
// At the global bound,
// oldest-expiring sessions are evicted when exceeded.
const maxSessions = 256
const maxSessionsPerAdmin = 8

// adminSessionEntry is one signed-in operator. Token is a 32-byte random value;
// only its SHA-256 is meaningful for comparison, but with an in-memory table
// the raw token is simply stored (process restart invalidates everything,
// which is the intended fail-safe).
type adminSessionEntry struct {
	expires      time.Time
	userID       int64
	passwordHash string
	authVersion  int64
	verifiedAt   time.Time
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
func (m *adminSessionManager) create() string { return m.createForUser(0, "") }

func (m *adminSessionManager) createForUser(userID int64, passwordHash string, version ...int64) string {
	token := newToken()
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	count := 0
	oldestUser := ""
	var oldestUserExpiry time.Time
	for key, entry := range m.sessions {
		if !entry.expires.After(now) {
			delete(m.sessions, key)
			continue
		}
		if entry.userID == userID {
			count++
			if oldestUser == "" || entry.expires.Before(oldestUserExpiry) {
				oldestUser = key
				oldestUserExpiry = entry.expires
			}
		}
	}
	if count >= maxSessionsPerAdmin {
		delete(m.sessions, oldestUser)
	}
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
	v := int64(0)
	if len(version) > 0 {
		v = version[0]
	}
	m.sessions[token] = adminSessionEntry{expires: time.Now().Add(sessionTTL), userID: userID, passwordHash: passwordHash, authVersion: v, verifiedAt: now}
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
func (l *loginRateLimiter) allow(ip string) bool { return l.allowLimit(ip, loginWindowMax) }

// Successful step-up verification clears failed attempts for that identity.
// Repeated legitimate management actions must not trigger a password lockout.
func (l *loginRateLimiter) clear(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempt, key)
}

func (l *loginRateLimiter) allowLimit(ip string, max int) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.attempt[ip]
	if !ok || now.Sub(w.start) >= loginWindowLen {
		if len(l.attempt) >= 4096 {
			for k, old := range l.attempt {
				if now.Sub(old.start) >= loginWindowLen {
					delete(l.attempt, k)
				}
			}
			if len(l.attempt) >= 4096 {
				return false
			}
		}
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
	return w.count <= max
}

// adminAuth resolves the admin auth state of a request: a valid session cookie
// OR a valid X-Admin-Token/Bearer header (kept for scripts) authenticates. The
// three return values mirror the legacy adminGuard's needs.
func (s *Server) adminAuth(r *http.Request) (sessionOK, headerOK bool) {
	_, sessionOK, headerOK = s.resolveAdminIdentity(r)
	return
}

// handleAdminLogin exchanges an ADMIN_TOKEN for an HttpOnly session cookie.
// Rate limited per IP. Also serves the session check: with a valid cookie (or
// header) it returns 200 without checking the token, so the WebUI can probe
// session liveness with a plain GET-style POST of nothing.
func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	hasAdmin, adminErr := s.adminPasswordState()
	probe := map[string]any{"password_enabled": hasAdmin, "bootstrap_needed": !hasAdmin}
	if adminErr != nil {
		writeAPIErr(w, errBody(503, "管理员状态暂不可用", "server_error"))
		return
	}
	body, err := readJSON(r)
	if err != nil && err != io.EOF {
		writeJSON(w, 400, errBody(400, "bad json", "invalid_request_error").body)
		return
	}
	// No ADMIN_TOKEN configured: the whole admin surface is loopback-only and
	// the WebUI must skip login. The probe (empty body) reports that state as
	// ok:true/no_login rather than an error, so the frontend never shows the
	// login page in this mode.
	if s.o.cfg.AdminToken == "" && !hasAdmin {
		if len(body) == 0 {
			writeJSON(w, 200, map[string]any{"ok": true, "probe": true, "no_login": true})
			return
		}
		writeJSON(w, 400, map[string]any{"ok": false, "message": "未设置 ADMIN_TOKEN（本机回环模式无需登录）"})
		return
	}
	// Session probe / liveness check (no token in body): authenticated callers
	// get ok + csrf-free status without consuming a rate-limit slot.
	if len(body) == 0 {
		if sessionOK, headerOK := s.adminAuth(r); sessionOK || headerOK {
			probe["ok"] = true
			probe["probe"] = true
			writeJSON(w, 200, probe)
			return
		}
		probe["ok"] = false
		probe["probe"] = true
		writeJSON(w, 401, probe)
		return
	}
	// Real login: rate-limit, constant-time compare, issue cookie.
	token, _ := body["token"].(string)
	var userID int64
	var passwordHash string
	var authVersion int64
	var temporary bool
	valid := token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.o.cfg.AdminToken)) == 1
	if username, ok := body["username"].(string); ok && token == "" {
		password, _ := body["password"].(string)
		if !s.loginLimiter.allow("user:" + strings.ToLower(strings.TrimSpace(username))) {
			writeAPIErr(w, localOverload("login_attempts"))
			return
		}
		u, rawSession, err := portalauth.Login(s.o.db, username, password)
		if rawSession != "" {
			portalauth.Logout(s.o.db, rawSession)
		}
		if err == nil && u != nil && store.IsAdminRole(u.Role) {
			valid = true
			userID = u.ID
			passwordHash = u.PasswordHash
			authVersion = u.AuthVersion
			temporary = u.MustChangePassword
		}
	}
	if !valid {
		time.Sleep(200 * time.Millisecond) // blunt brute-force cost
		writeJSON(w, 401, map[string]any{"ok": false, "message": "管理员凭据无效"})
		return
	}
	value := s.sessions.createForUser(userID, passwordHash, authVersion)
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   r.TLS != nil || s.o.cfg.AdminCookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, 200, map[string]any{"ok": true, "must_change_password": temporary})
}

// handleAdminLogout drops the session (cookie itself is cleared by the client
// or overwritten on next login).
func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(cookieName); err == nil {
		s.sessions.drop(c.Value)
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: r.TLS != nil || s.o.cfg.AdminCookieSecure, SameSite: http.SameSiteLaxMode,
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

// Forwarded identity is accepted only from explicitly trusted immediate peers.
func (s *Server) rateLimitIP(r *http.Request) string {
	peer := net.ParseIP(clientIP(r))
	for _, raw := range strings.Split(s.o.cfg.TrustedProxyCIDRs, ",") {
		_, network, err := net.ParseCIDR(strings.TrimSpace(raw))
		if err == nil && peer != nil && network.Contains(peer) {
			if ip := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); ip != nil {
				return ip.String()
			}
		}
	}
	return clientIP(r)
}

func (s *Server) adminPasswordState() (bool, error) {
	if s.o.db == nil {
		return false, nil
	}
	return s.o.db.HasAdminUser()
}
