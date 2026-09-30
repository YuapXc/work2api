package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Server is the HTTP surface over an Orchestrator.
type Server struct {
	o *Orchestrator
	// adminSessions + loginLimiter back the WebUI login (HttpOnly cookie
	// sessions). In-memory only: restart signs everyone out.
	sessions     *adminSessionManager
	loginLimiter *loginRateLimiter
}

type principalContextKey struct{}

// NewServer builds the HTTP server.
func NewServer(o *Orchestrator) *Server {
	return &Server{o: o, sessions: newAdminSessionManager(), loginLimiter: newLoginRateLimiter()}
}

// Handler returns the root handler with all routes mounted and Host guarded.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("POST /v1/messages", s.handleMessages)
	mux.HandleFunc("POST /v1/messages/count_tokens", s.handleCountTokens)
	mux.HandleFunc("POST /v1/responses", s.handleResponses)
	s.mountAdmin(mux)
	s.mountWebUI(mux)
	return s.hostGuard(s.adminGuard(s.requestGuard(mux)))
}

// Bound admission and finish reading bodies before opening a long-lived SSE
// response. A body deadline must not become a deadline for the response stream.
func (s *Server) requestGuard(next http.Handler) http.Handler {
	capacity := s.o.cfg.MaxConcurrentRequests
	if capacity <= 0 {
		capacity = 32
	}
	slots := make(chan struct{}, capacity)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			principal, aerr := s.auth(r)
			if aerr != nil {
				writeAPIErr(w, aerr)
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal))
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			writeJSON(w, 429, errBody(429, "并发请求过多，请稍后重试", "rate_limit_error").body)
			return
		}
		if r.URL.Path == "/admin/login" && s.o.cfg.AdminToken != "" {
			sessionOK, headerOK := s.adminAuth(r)
			if !sessionOK && !headerOK && !s.loginLimiter.allow(clientIP(r)) {
				writeJSON(w, 429, errBody(429, "尝试次数过多，请 10 分钟后再试", "rate_limit_error").body)
				return
			}
		}
		if r.Body != nil && r.Body != http.NoBody {
			limit := s.o.cfg.MaxRequestBytes
			if limit <= 0 {
				limit = 16 * 1024 * 1024
			}
			if r.URL.Path == "/admin/login" {
				limit = 8 * 1024
			}
			if r.ContentLength > limit {
				writeJSON(w, 413, errBody(413, "请求体过大", "invalid_request_error").body)
				return
			}
			controller := http.NewResponseController(w)
			_ = controller.SetReadDeadline(time.Now().Add(30 * time.Second))
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
			_ = r.Body.Close()
			_ = controller.SetReadDeadline(time.Time{})
			if err != nil {
				status := http.StatusBadRequest
				var tooLarge *http.MaxBytesError
				var timeout net.Error
				if errors.As(err, &tooLarge) {
					status = http.StatusRequestEntityTooLarge
				} else if errors.As(err, &timeout) && timeout.Timeout() {
					status = http.StatusRequestTimeout
				}
				writeJSON(w, status, errBody(status, "请求体读取失败或超过限制", "invalid_request_error").body)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		next.ServeHTTP(w, r)
	})
}

// adminGuard authenticates the /admin/* surface. Historically ADMIN_TOKEN was
// read into config but never enforced, so the whole management API was open to
// anyone who could reach the host — a real exposure once ALLOW_EXTERNAL_HOST=1
// puts the server on a LAN/public interface. Now:
//   - POST /admin/login|logout are always reachable (login must be reachable to
//     authenticate; logout is harmless).
//   - With ADMIN_TOKEN set, requests authenticate via session cookie (WebUI
//     login) OR X-Admin-Token/Bearer header (scripts, kept for compatibility).
//   - Without ADMIN_TOKEN the management API is loopback-only regardless of
//     ALLOW_EXTERNAL_HOST; the WebUI skips login in that mode.
//
// The static WebUI at "/" is served without a token so the operator can load
// the login page.
func (s *Server) adminGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/admin/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/admin/login" || r.URL.Path == "/admin/logout" {
			if !s.adminOriginAllowed(r, false) {
				writeJSON(w, 403, errBody(403, "拒绝跨源管理请求", "forbidden").body)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		token := s.o.cfg.AdminToken
		if token == "" {
			// Loopback check MUST use the peer address (r.RemoteAddr), not the
			// Host header: the header is client-controlled, so a remote
			// attacker could send "Host: 127.0.0.1" and walk straight in when
			// the listener is bound beyond loopback (HOST=0.0.0.0). The
			// hostGuard's Host check exists separately for DNS-rebinding.
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			if !isLoopbackHost(host) {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{
					"message": "管理端未设置 ADMIN_TOKEN，仅本机（回环）可访问。需要远程管理请设置 ADMIN_TOKEN 环境变量。",
					"type":    "forbidden"}})
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		sessionOK, headerOK := s.adminAuth(r)
		if !sessionOK && !headerOK {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
				"message": "管理端会话无效或已过期，请重新登录（或携带 X-Admin-Token 请求头）。",
				"type":    "unauthorized"}})
			return
		}
		// Header-authenticated scripts are exempt; ambient browser cookies must
		// not authorize a sibling origin, even when it is considered same-site.
		if sessionOK && !headerOK && !s.adminOriginAllowed(r, true) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{
				"message": "拒绝跨站请求（CSRF 防护）。", "type": "forbidden"}})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Explicit token headers authenticate scripts independently of browser cookies.
// Cookie writes require a same-origin browser signal; reads also reject sibling
// origins so credentials/export cannot be navigated from an untrusted subdomain.
func (s *Server) adminOriginAllowed(r *http.Request, requireBrowserSignal bool) bool {
	site := r.Header.Get("Sec-Fetch-Site")
	if site != "" && site != "same-origin" && site != "none" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		u, err := url.Parse(origin)
		scheme := "http"
		if r.TLS != nil || s.o.cfg.AdminCookieSecure {
			scheme = "https"
		}
		return err == nil && u.Scheme == scheme && strings.EqualFold(u.Host, r.Host) && u.User == nil && u.Path == "" && u.RawQuery == "" && u.Fragment == ""
	}
	safe := r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions
	return !requireBrowserSignal || safe || site == "same-origin"
}

// hostGuard blocks DNS-rebinding: only loopback/allowed hosts unless
// ALLOW_EXTERNAL_HOST is set.
func (s *Server) hostGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.o.cfg.AllowExternalHost {
			host := r.Host
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			if !isLoopbackHost(host) {
				writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]any{
					"message": "Host 不被允许（防 DNS rebinding）。局域网/公网访问请设置 ALLOW_EXTERNAL_HOST=1",
					"type":    "forbidden"}})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func isLoopbackHost(host string) bool {
	if host == "localhost" || host == "" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// handleHealth is a liveness probe, unauthenticated by design (load balancers).
// Public exposure note: it reports counts and health summary only — account
// identifiers were previously enumerated here, which is information leakage on
// a public listener (they key cooldowns and display in logs, not secrets, but
// there is no reason to publish them).
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	accounts := s.o.pool.Accounts()
	healthy := 0
	for _, a := range accounts {
		if a.Enabled && a.CooldownUntil <= float64(time.Now().UnixNano())/1e9 {
			healthy++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "account_count": len(accounts), "healthy_accounts": healthy,
		"model_source": s.o.models.Source(),
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, aerr := s.auth(r); aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	data := s.o.models.ListCached()
	settings, _ := s.o.db.GetSettings()
	aliases := parseModelAliases(settings["model_aliases"])
	if len(aliases) > 0 {
		byID := map[string]map[string]any{}
		for _, m := range data {
			byID[str2(m["id"])] = m
		}
		for alias, real := range aliases {
			if _, dup := byID[alias]; dup {
				continue
			}
			if src, ok := byID[real]; ok {
				item := map[string]any{}
				for k, v := range src {
					item[k] = v
				}
				item["id"] = alias
				item["name"] = alias + "（" + real + " 别名）"
				data = append(data, item)
			}
		}
	}
	out := make([]map[string]any, 0, len(data))
	for _, item := range data {
		clean := map[string]any{}
		for k, v := range item {
			if k == "account_uids" {
				continue
			}
			clean[k] = v
		}
		out = append(out, clean)
	}
	// Append models served by non-default provider runtimes (qoder/*, opencode/*).
	out = append(out, s.o.runtimeModels(r.Context())...)
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if _, aerr := s.auth(r); aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	body, err := readJSON(r)
	if err != nil {
		writeJSON(w, 400, errBody(400, "bad json", "invalid_request_error").body)
		return
	}
	textLen := 0
	nMsg := 0
	if msgs, ok := body["messages"].([]any); ok {
		for _, m := range msgs {
			nMsg++
			mm, _ := m.(map[string]any)
			switch c := mm["content"].(type) {
			case string:
				textLen += len(c)
			case []any:
				for _, p := range c {
					if pm, ok := p.(map[string]any); ok {
						if t, ok := pm["text"].(string); ok {
							textLen += len(t)
						}
					}
				}
			}
		}
	}
	est := textLen/4 + nMsg*4 + 4
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": est})
}

func (s *Server) auth(r *http.Request) (*Principal, *apiError) {
	if principal, ok := r.Context().Value(principalContextKey{}).(*Principal); ok {
		return principal, nil
	}
	return s.o.checkAPIKey(r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAPIErr(w http.ResponseWriter, e *apiError) {
	writeJSON(w, e.status, e.body)
}

func readJSON(r *http.Request) (map[string]any, error) {
	var body map[string]any
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil {
		return nil, err
	}
	return body, nil
}

func str2(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

var _ = strings.TrimSpace
