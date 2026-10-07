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
	"path"
	"strings"
	"sync"
	"time"
	"work2api/internal/statebackup"
)

// Server is the HTTP surface over an Orchestrator.
type Server struct {
	calls callMonitor
	o     *Orchestrator
	// adminSessions + loginLimiter back the WebUI login (HttpOnly cookie
	// sessions). In-memory only: restart signs everyone out.
	sessions                                                    *adminSessionManager
	loginLimiter                                                *loginRateLimiter
	portalLoginLimiter                                          *loginRateLimiter
	portalSlots                                                 chan struct{}
	modelsAdmission                                             *modelAdmission
	adminSlots, heavySlots, querySlots, bodySlots, refreshSlots chan struct{}
	bodyWaiting                                                 chan struct{}
	bodies                                                      *bodyBudget
	publicBodySlots, publicBodyWaiting                          chan struct{}
	publicBodies                                                *bodyBudget
	refreshMu                                                   sync.Mutex
	refreshes                                                   map[string]*refreshFlight
	// 用户门户状态（HANDOFF §8）：扫码任务表（内存，带 TTL）与每用户轮询限频。
	portalMu          sync.Mutex
	portalTasks       map[string]*portalContributionTask
	portalPollMu      sync.Mutex
	portalPollLimiter map[int64]*pollLimiter
}

// portalUserKey keys the authenticated portal user in the request context.
type portalUserKey struct{}

// pollLimiter is a fixed-window per-user rate limit for contribution polling
// (roughly 1 req/s sustained is plenty for a device-flow status check).
type pollLimiter struct {
	mu     sync.Mutex
	window int64
	count  int
}

const pollLimitPerWindow = 5 // per 10s window

func newPollLimiter() *pollLimiter { return &pollLimiter{} }

func (p *pollLimiter) allow() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	w := time.Now().Unix() / 10
	if p.window != w {
		p.window = w
		p.count = 0
	}
	if p.count >= pollLimitPerWindow {
		return false
	}
	p.count++
	return true
}

type principalContextKey struct{}

// NewServer builds the HTTP server.
func NewServer(o *Orchestrator) *Server {
	return &Server{o: o, sessions: newAdminSessionManager(), loginLimiter: newLoginRateLimiter(), portalLoginLimiter: newLoginRateLimiter(), portalSlots: make(chan struct{}, 8),
		portalTasks: map[string]*portalContributionTask{}, portalPollLimiter: map[int64]*pollLimiter{},
		publicBodySlots: make(chan struct{}, 2), publicBodyWaiting: make(chan struct{}, 2), publicBodies: &bodyBudget{limit: 64 << 10},
		bodyWaiting: make(chan struct{}, positiveOr(o.cfg.ModelQueueSize, 32)), modelsAdmission: newModelAdmission(o.cfg), adminSlots: make(chan struct{}, positiveOr(o.cfg.AdminConcurrency, 8)), heavySlots: make(chan struct{}, positiveOr(o.cfg.HeavyAdminConcurrency, 2)), querySlots: make(chan struct{}, positiveOr(o.cfg.QueryConcurrency, 4)), bodySlots: make(chan struct{}, positiveOr(o.cfg.BodyReadConcurrency, 4)), bodies: &bodyBudget{limit: int64(positiveOr(int(o.cfg.RequestBodyBudget), 32<<20))}, refreshSlots: make(chan struct{}, 8), refreshes: map[string]*refreshFlight{}}
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
	s.mountPortal(mux)
	s.mountWebUI(mux)
	return s.hostGuard(s.stateGuard(s.observeCalls(s.adminGuard(s.portalGuard(s.requestGuard(mux))))))
}

func (s *Server) stateGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/backups" {
			leave := statebackup.Enter()
			defer leave()
		}
		next.ServeHTTP(w, r)
	})
}

// Bound admission and finish reading bodies before opening a long-lived SSE
// response. A body deadline must not become a deadline for the response stream.
func (s *Server) requestGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			w.Header().Set("Cache-Control", "no-store")
			w.Header().Set("Vary", "Authorization, X-Api-Key")
			principal, aerr := s.auth(r)
			if aerr != nil {
				writeAPIErr(w, aerr)
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), principalContextKey{}, principal))
		}
		// Let ServeMux preserve its 404/405/redirect semantics without reading
		// bodies for unmatched routes. GET (including static catch-all) and HEAD
		// never need a body; their normal query admission remains below.
		if mux, ok := next.(*http.ServeMux); ok {
			escaped := r.URL.EscapedPath()
			clean := path.Clean(escaped)
			if strings.HasSuffix(escaped, "/") && clean != "/" {
				clean += "/"
			}
			// ServeMux returns a nonempty pattern even for canonical redirects.
			// Their unnormalized paths have not passed route authentication.
			if _, pattern := mux.Handler(r); pattern == "" || r.Method != http.MethodConnect && clean != escaped {
				next.ServeHTTP(w, r)
				return
			}
		}
		slots := s.adminSlots
		model := isModelRoute(r)
		heavy := isHeavyAdmin(r)
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			slots = s.querySlots
		}
		if heavy {
			slots = s.heavySlots
		}
		if model || r.URL.Path == "/admin/models/test" || r.URL.Path == "/health" || !strings.HasPrefix(r.URL.Path, "/admin/") && !strings.HasPrefix(r.URL.Path, "/v1/") {
			slots = nil
		}
		// Refresh followers share the leader's result, using bounded lightweight admission.
		if heavy && isRefresh(r) {
			slots = s.refreshSlots
		}
		if slots != nil {
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
			default:
				s.writeOverload(w, "request_capacity")
				return
			}
		}
		if r.URL.Path == "/admin/login" {
			sessionOK, headerOK := s.adminAuth(r)
			if !sessionOK && !headerOK && !s.loginLimiter.allow(s.rateLimitIP(r)) {
				writeJSON(w, 429, errBody(429, "尝试次数过多，请 10 分钟后再试", "rate_limit_error").body)
				return
			}
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Body != nil && r.Body != http.NoBody {
			bodySlots, bodyWaiting, bodies := s.bodySlots, s.bodyWaiting, s.bodies
			if r.URL.Path == "/admin/login" || r.URL.Path == "/admin/logout" || strings.HasPrefix(r.URL.Path, "/portal/api/auth/") {
				bodySlots, bodyWaiting, bodies = s.publicBodySlots, s.publicBodyWaiting, s.publicBodies
			}
			limit := s.o.cfg.MaxRequestBytes
			if limit <= 0 {
				limit = 16 * 1024 * 1024
			}
			if r.URL.Path == "/admin/login" || strings.HasPrefix(r.URL.Path, "/portal/api/") {
				limit = 8 * 1024
			}
			if r.ContentLength > limit {
				writeJSON(w, 413, errBody(413, "请求体过大", "invalid_request_error").body)
				return
			}
			select {
			case bodySlots <- struct{}{}:
			default:
				// A short bounded wait absorbs Agent fan-out before body parsing;
				// readers and raw-byte allocations retain their existing limits.
				select {
				case bodyWaiting <- struct{}{}:
				default:
					s.writeOverload(w, "body_read_capacity")
					return
				}
				timer := time.NewTimer(5 * time.Second)
				select {
				case bodySlots <- struct{}{}:
					timer.Stop()
					<-bodyWaiting
				case <-r.Context().Done():
					timer.Stop()
					<-bodyWaiting
					return
				case <-timer.C:
					<-bodyWaiting
					s.writeOverload(w, "body_read_capacity")
					return
				}
			}

			body, reserved, err := func() ([]byte, int64, error) {
				defer func() { <-bodySlots }()
				defer r.Body.Close()
				controller := http.NewResponseController(w)
				_ = controller.SetReadDeadline(time.Now().Add(30 * time.Second))
				defer controller.SetReadDeadline(time.Time{})
				return bodies.read(http.MaxBytesReader(w, r.Body, limit), r.ContentLength)
			}()
			defer bodies.release(reserved)
			if err != nil {
				var overload *apiError
				if errors.As(err, &overload) {
					s.writeOverload(w, "body_budget_exhausted")
					return
				}
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
			if !jsonWithinComplexity(body, positiveOr(s.o.cfg.MaxJSONItems, 100000), positiveOr(s.o.cfg.MaxJSONDepth, 128)) {
				writeJSON(w, 400, errBody(400, "JSON 结构过于复杂或嵌套过深", "invalid_request_error").body)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		if heavy && isRefresh(r) {
			s.serveRefresh(w, r, next)
			return
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
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path == "/admin/login" || r.URL.Path == "/admin/logout" {
			if !s.adminOriginAllowed(r, false) {
				writeJSON(w, 403, errBody(403, "拒绝跨源管理请求", "forbidden").body)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		token := s.o.cfg.AdminToken
		hasAdmin, stateErr := s.adminPasswordState()
		if stateErr != nil {
			writeAPIErr(w, errBody(503, "管理员状态暂不可用", "server_error"))
			return
		}
		if token == "" && !hasAdmin {
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
		if a.Healthy(float64(time.Now().UnixNano()) / 1e9) {
			healthy++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "account_count": len(accounts), "healthy_accounts": healthy,
		"model_source": s.o.models.Source(),
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	principal, aerr := s.auth(r)
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	data := s.modelCatalog(r.Context())
	settings, _ := s.o.db.GetSettings()
	aliases := parseModelAliases(settings["model_aliases"])
	out := make([]map[string]any, 0, len(data))
	for _, item := range data {
		id := str2(item["id"])
		resolved := id
		if real, ok := aliases[id]; ok {
			resolved = real
		}
		// Portal keys see exactly the shared scope (deny by default); private
		// keys keep the unrestricted-catalog behavior.
		if principal.UserID > 0 {
			if !s.portalModelAllowed(principal, id, resolved) {
				continue
			}
		} else if !modelAllowed(principal, id, resolved) {
			continue
		}
		clean := map[string]any{}
		for k, v := range item {
			if k != "account_uids" {
				clean[k] = v
			}
		}
		out = append(out, clean)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}

func (s *Server) modelCatalog(ctx context.Context) []map[string]any {
	data := s.o.models.ListCached()
	// Merge all providers before adding aliases and applying the key's policy.
	data = append(data, s.o.runtimeModels(ctx)...)
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
	return data
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
	// Compatibility estimate, not a provider tokenizer or quota settlement.
	writeJSON(w, http.StatusOK, map[string]any{"input_tokens": estimateInputTokens(body)})
}

func estimateInputTokens(body map[string]any) int {
	textBytes := estimateContentBytes(body["system"])
	nMsg := 0
	if messages, ok := body["messages"].([]any); ok {
		for _, raw := range messages {
			msg, _ := raw.(map[string]any)
			nMsg++
			textBytes += estimateContentBytes(msg["content"])
			if calls := msg["tool_calls"]; calls != nil {
				textBytes += estimateJSONBytes(calls)
			}
		}
	}
	if tools := body["tools"]; tools != nil {
		textBytes += estimateJSONBytes(tools)
	}
	// Restore the old byte/4 baseline rather than reducing CJK estimates using
	// an uncalibrated coefficient. Images/audio require a model-specific counter.
	return textBytes/4 + nMsg*4 + 4
}
func estimateJSONBytes(v any) int { b, _ := json.Marshal(v); return len(b) }
func estimateContentBytes(v any) int {
	switch x := v.(type) {
	case string:
		return len(x)
	case []any:
		n := 0
		for _, item := range x {
			n += estimateContentBytes(item)
		}
		return n
	case map[string]any:
		switch x["type"] {
		case "image", "image_url", "input_image", "audio", "input_audio":
			return 0
		case "tool_use":
			return len(toStrLoose(x["name"])) + estimateJSONBytes(x["input"])
		case "tool_result":
			return estimateContentBytes(x["content"])
		case "thinking":
			return estimateContentBytes(x["thinking"])
		default:
			return estimateContentBytes(x["text"]) + estimateContentBytes(x["content"])
		}
	default:
		return 0
	}
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
	// 拒绝 JSON 后缀数据（{"a":1} {"b":2}），与 requestGuard 的复杂度检查语义一致。
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("JSON 后存在多余数据")
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
