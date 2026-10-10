package app

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"work2api/internal/portal/portalauth"
	"work2api/internal/store"
	"work2api/internal/workbuddy/credentials"
	"work2api/internal/workbuddy/oauth"
	wbruntime "work2api/internal/workbuddy/runtime"
	"work2api/internal/workbuddy/siterouting"
)

// 用户门户 HTTP 面（HANDOFF §8 方案阶段 A）。挂载点 /portal/*：
//   - /portal/api/auth/*（注册/登录/登出/改密）无需会话但按 IP 限流 + 同源校验；
//   - 其余 /portal/api/* 需要用户会话（SQLite 持久化，可服务端吊销）；
//   - /admin/portal/* 复用现有 adminGuard（含 CSRF/Origin 校验）。
//
// 权限与调度隔离语义在 orchestrator（attachPortalScope / pickInScope），数据
// 关系在 store_portal；本文件只做 HTTP 编排、所有权校验和脱敏输出。

var inviteCodeRe = regexp.MustCompile(`^[A-Z0-9]{6,32}$`)

// portalTaskTTL bounds a scan-task's lifetime; expired tasks are swept lazily.
const portalTaskTTL = 10 * time.Minute
const portalMaxTasks = 64

var portalOAuthBegin = oauth.Begin
var portalOAuthPoll = oauth.Poll

func (s *Server) mountPortal(mux *http.ServeMux) {
	s.mountIdentity(mux)
	// 认证（限流在 portalGuard 按路径做）
	mux.HandleFunc("GET /portal/api/auth/state", s.portalAuthState)
	mux.HandleFunc("POST /portal/api/auth/register", s.portalRegister)
	mux.HandleFunc("POST /portal/api/auth/login", s.portalLogin)
	mux.HandleFunc("POST /portal/api/auth/logout", s.portalLogout)
	mux.HandleFunc("POST /portal/api/auth/password", s.portalChangePassword)
	// 会话内接口
	mux.HandleFunc("GET /portal/api/me", s.portalMe)
	mux.HandleFunc("GET /portal/api/models", s.portalModels)
	mux.HandleFunc("GET /portal/api/keys", s.portalListKeys)
	mux.HandleFunc("POST /portal/api/keys", s.portalCreateKey)
	mux.HandleFunc("POST /portal/api/keys/{id}/toggle", s.portalToggleKey)
	mux.HandleFunc("POST /portal/api/keys/{id}/models", s.portalSetKeyModels)
	mux.HandleFunc("DELETE /portal/api/keys/{id}", s.portalDeleteKey)
	mux.HandleFunc("GET /portal/api/contributions", s.portalListContributions)
	mux.HandleFunc("POST /portal/api/contributions/begin", s.portalContributionBegin)
	mux.HandleFunc("POST /portal/api/contributions/poll", s.portalContributionPoll)
	mux.HandleFunc("POST /portal/api/contributions/cancel", s.portalContributionCancel)
	mux.HandleFunc("POST /portal/api/contributions/{id}/revoke", s.portalRevokeContribution)
	mux.HandleFunc("POST /portal/api/contributions/{id}/share", s.portalShareContribution)
	mux.HandleFunc("POST /portal/api/contributions/{id}/models/refresh", s.portalRefreshContributionModels)
	mux.HandleFunc("GET /portal/api/usage", s.portalUsage)
	// 管理端（/admin 前缀 → adminGuard 自然生效：会话/令牌 + CSRF）
	mux.HandleFunc("GET /admin/portal/users", s.adminPortalUsers)
	mux.HandleFunc("POST /admin/portal/users/{id}/status", s.adminPortalUserStatus)
	mux.HandleFunc("POST /admin/portal/users/{id}/concurrency", s.adminPortalUserConcurrency)
	mux.HandleFunc("POST /admin/portal/users/{id}/password", s.adminPortalResetPassword)
	mux.HandleFunc("GET /admin/portal/invites", s.adminPortalInvites)
	mux.HandleFunc("POST /admin/portal/invites", s.adminPortalCreateInvite)
	mux.HandleFunc("DELETE /admin/portal/invites/{code}", s.adminPortalRevokeInvite)
	mux.HandleFunc("GET /admin/portal/groups", s.adminPortalGroups)
	mux.HandleFunc("POST /admin/portal/groups", s.adminPortalCreateGroup)
	mux.HandleFunc("POST /admin/portal/groups/{id}/models", s.adminPortalGroupModels)
	mux.HandleFunc("POST /admin/portal/groups/{id}/enable", s.adminPortalGroupEnable)
	mux.HandleFunc("POST /admin/portal/groups/{id}/accounts", s.adminPortalGroupAccounts)
	mux.HandleFunc("POST /admin/portal/groups/{id}/grants", s.adminPortalGroupGrants)
	mux.HandleFunc("DELETE /admin/portal/groups/{id}", s.adminPortalGroupDelete)
	mux.HandleFunc("GET /admin/portal/overview", s.adminPortalOverview)
	mux.HandleFunc("POST /admin/portal/bootstrap", s.adminPortalBootstrap)
	mux.HandleFunc("POST /admin/portal/default-group", s.adminPortalDefaultGroup)
	mux.HandleFunc("POST /admin/portal/accounts/{uid}/sharing", s.adminPortalAccountSharing)
}

// adminPortalBootstrap creates the first portal admin user. One-time and
// idempotent: once an admin user exists it refuses (never auto-promotes, never
// overwrites). Intended to be run once through the SSH tunnel.
func (s *Server) adminPortalBootstrap(w http.ResponseWriter, r *http.Request) {
	body, err := readJSON(r)
	if err != nil {
		writeJSON(w, 400, errBody(400, "bad json", "invalid_request_error").Body)
		return
	}
	username, _ := body["username"].(string)
	password, _ := body["password"].(string)
	username, hash, err := portalauth.UserPasswordHash(username, password)
	if err != nil {
		writeJSON(w, 400, errBody(400, err.Error(), "invalid_request_error").Body)
		return
	}
	id, err := s.o.db.CreateInitialAdmin(username, hash)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeJSON(w, 409, errBody(409, "管理员已初始化或用户名已存在", "conflict").Body)
		} else {
			writeJSON(w, 500, errBody(500, "初始化失败", "server_error").Body)
		}
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "id": id, "username": strings.ToLower(strings.TrimSpace(username))})
}

// isPortalAuthPath reports whether the portal API path is one of the
// unauthenticated auth endpoints (rate-limited per-IP, no session required).
func isPortalAuthPath(path string) bool {
	return path == "/portal/api/auth/state" || path == "/portal/api/auth/register" ||
		path == "/portal/api/auth/login" || path == "/portal/api/auth/logout"
}

// portalGuard authenticates /portal/api/* and enforces same-origin writes.
func (s *Server) portalGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if !strings.HasPrefix(path, "/portal/api/") {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Vary", "Cookie, Authorization")
		select {
		case s.portalSlots <- struct{}{}:
			defer func() { <-s.portalSlots }()
		default:
			s.writeOverload(w, "portal_capacity")
			return
		}
		// 写操作（含认证接口）一律同源校验；带会话的写请求要求浏览器信号
		// （与 adminGuard 一致），认证接口保持宽松以兼容旧客户端。
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !s.adminOriginAllowed(r, !isPortalAuthPath(path)) {
			writeJSON(w, 403, errBody(403, "拒绝跨源请求", "forbidden").Body)
			return
		}
		if isPortalAuthPath(path) {
			if r.Method == http.MethodPost && path != "/portal/api/auth/logout" && (!s.portalLoginLimiter.allowLimit("ip:"+s.rateLimitIP(r), 60) || !s.portalLoginLimiter.allowLimit("global", 300)) {
				w.Header().Set("Retry-After", "600")
				writeJSON(w, 429, errBody(429, "尝试次数过多，请 10 分钟后再试", "rate_limit_error").Body)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		user, err := s.portalUser(r)
		if err != nil {
			writeJSON(w, 401, errBody(401, "请先登录", "unauthorized").Body)
			return
		}
		if user.MustChangePassword && path != "/portal/api/me" && path != "/portal/api/auth/password" {
			writeAPIErr(w, errBody(403, "请先修改临时密码", "password_change_required"))
			return
		}
		// 轮询端点单独限流（每用户），防高频打上游。
		if path == "/portal/api/contributions/poll" || path == "/portal/api/contributions/begin" || strings.HasSuffix(path, "/models/refresh") {
			s.portalPollMu.Lock()
			if len(s.portalPollLimiter) >= 1024 {
				window := time.Now().Unix() / 10
				for id, limiter := range s.portalPollLimiter {
					limiter.mu.Lock()
					stale := limiter.window < window-1
					limiter.mu.Unlock()
					if stale {
						delete(s.portalPollLimiter, id)
					}
				}
			}
			lim := s.portalPollLimiter[user.ID]
			if lim == nil {
				if len(s.portalPollLimiter) >= 4096 {
					s.portalPollMu.Unlock()
					s.writeOverload(w, "portal_poll_capacity")
					return
				}
				lim = newPollLimiter()
				s.portalPollLimiter[user.ID] = lim
			}
			s.portalPollMu.Unlock()
			if !lim.allow() {
				writeJSON(w, 429, errBody(429, "轮询过于频繁，请稍后再试", "rate_limit_error").Body)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), portalUserKey{}, user)))
	})
}

// portalUser resolves the session cookie to the live user (nil when absent).
func (s *Server) portalUser(r *http.Request) (*store.User, error) {
	c, err := r.Cookie(portalauth.UserCookieName)
	if err != nil || c.Value == "" {
		return nil, errors.New("no session")
	}
	return portalauth.SessionUser(s.o.db, c.Value)
}

// portalCtx returns the authenticated portal user from the request context.
func portalCtx(r *http.Request) *store.User {
	u, _ := r.Context().Value(portalUserKey{}).(*store.User)
	return u
}

// --- 认证 ---

// portalAuthState tells the frontend whether registration is open and how.
func (s *Server) portalAuthState(w http.ResponseWriter, r *http.Request) {
	mode := s.o.cfg.PortalRegistrationMode
	if mode != "open" && mode != "closed" {
		mode = "invite" // invite 或未知值一律按邀请码
	}
	writeJSON(w, 200, map[string]any{"mode": mode, "portal_enabled": s.o.cfg.PortalEnabled})
}

func (s *Server) portalRegister(w http.ResponseWriter, r *http.Request) {
	if !s.o.cfg.PortalEnabled {
		writeJSON(w, 403, errBody(403, "门户未启用", "forbidden").Body)
		return
	}
	body, err := readJSON(r)
	if err != nil {
		writeJSON(w, 400, errBody(400, "bad json", "invalid_request_error").Body)
		return
	}
	mode := s.o.cfg.PortalRegistrationMode
	if mode != "open" && mode != "invite" {
		writeJSON(w, 403, errBody(403, "当前未开放注册", "forbidden").Body)
		return
	}
	username, _ := body["username"].(string)
	password, _ := body["password"].(string)
	invite, _ := body["invite_code"].(string)
	code := strings.ToUpper(strings.TrimSpace(invite))
	if mode == "invite" {
		if !inviteCodeRe.MatchString(code) {
			writeJSON(w, 400, errBody(400, "邀请码格式无效", "invalid_request_error").Body)
			return
		}
		if s.inviteRejected(w, code) {
			return // 已写响应
		}
	}
	username, hash, err := portalauth.UserPasswordHash(username, password)
	if err != nil {
		writeAPIErr(w, errBody(400, err.Error(), "invalid_request_error"))
		return
	}
	var id int64
	if mode == "invite" {
		id, err = s.o.db.CreateInvitedUser(username, hash, code)
	} else {
		id, err = s.o.db.CreateUser(username, hash, "user")
	}
	if err != nil {
		if errors.Is(err, store.ErrInvalidInvite) {
			writeAPIErr(w, errBody(400, "邀请码无效、过期或已使用", "invalid_request_error"))
		} else if errors.Is(err, store.ErrConflict) {
			writeAPIErr(w, errBody(409, "用户名已存在", "conflict"))
		} else {
			writeAPIErr(w, errBody(503, "注册暂不可用，请重试", "server_error"))
		}
		return
	}
	// 注册即登录：直接发会话，省一次登录交互。
	user, err := s.o.db.GetUser(id)
	if err != nil || user == nil {
		writeJSON(w, 500, errBody(500, "用户查询失败", "server_error").Body)
		return
	}
	if serr := s.startPortalSession(w, r, user.ID, hash); serr != nil {
		writeJSON(w, 500, errBody(500, "会话创建失败", "server_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "user": portalUserView(user)})
}

// inviteRejected is a UX pre-check for a friendlier error before the bcrypt
// cost of user creation; the authoritative consume is atomic afterwards. It
// writes the error response itself and reports whether it did.
func (s *Server) inviteRejected(w http.ResponseWriter, code string) bool {
	c, err := s.o.db.FindInviteCode(code)
	if err != nil {
		writeJSON(w, 500, errBody(500, "邀请码查询失败", "server_error").Body)
		return true
	}
	if c != nil {
		if c["used_by"] != nil {
			writeJSON(w, 400, errBody(400, "邀请码已被使用", "invalid_request_error").Body)
			return true
		}
		if exp, _ := c["expires_at"].(float64); exp != 0 && exp <= float64(time.Now().Unix()) {
			writeJSON(w, 400, errBody(400, "邀请码已过期", "invalid_request_error").Body)
			return true
		}
		return false // 存在且未消费：交给原子消费
	}
	writeJSON(w, 400, errBody(400, "邀请码无效", "invalid_request_error").Body)
	return true
}

func (s *Server) portalLogin(w http.ResponseWriter, r *http.Request) {
	body, err := readJSON(r)
	if err != nil {
		writeJSON(w, 400, errBody(400, "bad json", "invalid_request_error").Body)
		return
	}
	username, _ := body["username"].(string)
	password, _ := body["password"].(string)
	// 「用户名+IP」联合计数：纯用户名键可被第三方用来定向锁死任意已知用户，
	// 联合后只有同一来源的尝试才消耗该键的额度；per-IP 与 global 闸不变。
	if !s.portalLoginLimiter.allow("user:" + s.rateLimitIP(r) + ":" + strings.ToLower(strings.TrimSpace(username))) {
		writeAPIErr(w, localOverload("login_attempts"))
		return
	}
	user, token, err := portalauth.Login(s.o.db, username, password)
	if err != nil {
		time.Sleep(200 * time.Millisecond) // 等时耗：未知用户也走 dummy bcrypt，此处再加固定延迟
		writeJSON(w, 401, errBody(401, "用户名或密码错误", "auth_error").Body)
		return
	}
	s.setPortalCookie(w, r, token)
	writeJSON(w, 200, map[string]any{"ok": true, "user": portalUserView(user)})
}

func (s *Server) portalLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(portalauth.UserCookieName); err == nil {
		portalauth.Logout(s.o.db, c.Value)
	}
	s.clearPortalCookie(w, r)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) portalChangePassword(w http.ResponseWriter, r *http.Request) {
	user := portalCtx(r)
	if user == nil {
		writeJSON(w, 401, errBody(401, "请先登录", "unauthorized").Body)
		return
	}
	if !s.portalLoginLimiter.allow("password:" + strconv.FormatInt(user.ID, 10)) {
		writeAPIErr(w, localOverload("login_attempts"))
		return
	}
	body, _ := readJSON(r)
	oldPass, _ := body["old_password"].(string)
	newPass, _ := body["new_password"].(string)
	if err := portalauth.ChangePassword(s.o.db, user.ID, oldPass, newPass); err != nil {
		writeJSON(w, 400, errBody(400, err.Error(), "invalid_request_error").Body)
		return
	}
	// ChangePassword 撤销了全部会话（含当前）；重发一个让本浏览器保持登录，
	// 其它设备全部下线（HANDOFF §4）。
	_, token, serr := portalauth.Login(s.o.db, user.Username, newPass)
	if serr != nil {
		writeJSON(w, 500, errBody(500, "会话创建失败", "server_error").Body)
		return
	}
	s.setPortalCookie(w, r, token)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// startPortalSession creates a session row and sets the cookie.
func (s *Server) startPortalSession(w http.ResponseWriter, r *http.Request, userID int64, expectedHash string) error {
	token := newToken()
	if err := s.o.db.CreateUserSessionForPassword(portalauth.HashToken(token), userID, float64(time.Now().Unix())+portalauth.SessionTTL, expectedHash); err != nil {
		return err
	}
	s.setPortalCookie(w, r, token)
	return nil
}

func (s *Server) setPortalCookie(w http.ResponseWriter, r *http.Request, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     portalauth.UserCookieName,
		Value:    token,
		Path:     "/",
		MaxAge:   int(portalauth.SessionTTL),
		HttpOnly: true,
		Secure:   r.TLS != nil || s.o.cfg.AdminCookieSecure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearPortalCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name: portalauth.UserCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: r.TLS != nil || s.o.cfg.AdminCookieSecure, SameSite: http.SameSiteLaxMode,
	})
}

func portalUserView(u *store.User) map[string]any {
	if u == nil {
		return nil
	}
	return map[string]any{"id": u.ID, "username": u.Username, "role": u.Role, "must_change_password": u.MustChangePassword}
}

// --- 会话内接口 ---

// portalMe returns profile + eligibility + the recomputed permission set, so
// the portal can show 可用模型 without probing /v1/models.
func (s *Server) portalMe(w http.ResponseWriter, r *http.Request) {
	user := portalCtx(r)
	if user.MustChangePassword {
		writeJSON(w, 200, map[string]any{"user": portalUserView(user), "eligible": false,
			"eligible_accounts": 0, "groups_enabled": 0, "available_models": []string{}, "own_models": []string{}, "shared_models": []string{}})
		return
	}
	accounts, err := s.o.db.OwnedAccountUIDs(user.ID)
	if err != nil {
		writeAPIErr(w, errBody(503, "账号查询失败", "server_error"))
		return
	}
	groups, err := s.o.db.GrantedGroups(user.ID)
	if err != nil {
		writeAPIErr(w, errBody(503, "共享权限查询失败", "server_error"))
		return
	}
	enabledGroups := 0
	p := &Principal{UserID: user.ID}
	if aerr := s.o.attachPortalScope(p); aerr != nil && aerr.Status >= 500 {
		writeAPIErr(w, aerr)
		return
	}
	available := s.o.portalCatalogPermissions(p)
	for model := range p.SharedModels {
		if !available[model] {
			delete(p.SharedModels, model)
		}
	}
	for _, g := range groups {
		if !g.Enabled || strings.TrimSpace(g.AllowedModels) == "" {
			continue
		}
		enabledGroups++
	}
	models := make([]string, 0, len(available))
	for m := range available {
		models = append(models, m)
	}
	sort.Strings(models)
	writeJSON(w, 200, map[string]any{
		"user":              portalUserView(user),
		"eligible":          len(available) > 0,
		"eligible_accounts": len(accounts),
		"own_models":        sortedPortalModels(p.OwnedModels),
		"shared_models":     sortedPortalModels(p.SharedModels),
		"groups_enabled":    enabledGroups,
		"available_models":  models,
	})
}

func sortedPortalModels(models map[string]bool) []string {
	list := make([]string, 0, len(models))
	for model := range models {
		list = append(list, model)
	}
	sort.Strings(list)
	return list
}

// --- Key 管理 ---

func (s *Server) portalListKeys(w http.ResponseWriter, r *http.Request) {
	user := portalCtx(r)
	apps, err := s.o.db.UserApps(user.ID)
	if err != nil {
		writeJSON(w, 500, errBody(500, "查询失败", "server_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"keys": apps})
}

// portalCreateKey issues a user-owned key. Portal keys never default to
// unrestricted: with no explicit allowlist they are narrowed to the current
// permission set (HANDOFF §7), and explicit models must be a subset of it.
func (s *Server) portalCreateKey(w http.ResponseWriter, r *http.Request) {
	user := portalCtx(r)
	if !s.o.cfg.PortalEnabled {
		writeJSON(w, 403, errBody(403, "门户未启用", "forbidden").Body)
		return
	}
	body, _ := readJSON(r)
	name, _ := body["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		writeJSON(w, 400, errBody(400, "请填写 Key 名称", "invalid_request_error").Body)
		return
	}
	if len(name) > 64 {
		writeJSON(w, 400, errBody(400, "Key 名称过长（≤64 字符）", "invalid_request_error").Body)
		return
	}
	available := s.portalAvailableModels(user.ID)
	allowedJSON := "[]"
	if raw, ok := body["allowed_models"]; ok && raw != nil {
		arr, valid := raw.([]any)
		if !valid {
			writeAPIErr(w, errBody(400, "allowed_models 必须为字符串数组", "invalid_request_error"))
			return
		}
		req := make([]string, 0, len(arr))
		for _, item := range arr {
			m, valid := item.(string)
			if !valid {
				writeAPIErr(w, errBody(400, "模型名称必须为字符串", "invalid_request_error"))
				return
			}
			if m = strings.TrimSpace(m); m == "" {
				continue
			}
			if !available[m] {
				writeJSON(w, 403, errBody(403, "模型 "+m+" 不在共享范围内", "model_not_allowed").Body)
				return
			}
			req = append(req, m)
		}
		if len(req) > 0 {
			b, _ := json.Marshal(req)
			allowedJSON = string(b)
		}
	} else {
		list := make([]string, 0, len(available))
		for m := range available {
			list = append(list, m)
		}
		sort.Strings(list)
		b, _ := json.Marshal(list)
		allowedJSON = string(b)
	}
	count, err := s.o.db.CountUserApps(user.ID)
	if err != nil {
		writeJSON(w, 500, errBody(500, "查询失败", "server_error").Body)
		return
	}
	if count >= s.o.cfg.PortalKeyMaxPerUser {
		writeJSON(w, 403, errBody(403, fmt.Sprintf("每个用户最多创建 %d 个 Key", s.o.cfg.PortalKeyMaxPerUser), "forbidden").Body)
		return
	}
	key := s.o.genAPIKey()
	enc, err := s.o.crypto.Encrypt(key)
	if err != nil {
		writeJSON(w, 500, errBody(500, "Key 创建失败", "server_error").Body)
		return
	}
	id, err := s.o.db.CreateAppForUserLimited(user.ID, name, s.o.hashKey(key), enc, s.o.cfg.PortalKeyMaxPerUser, allowedJSON, key[:min(10, len(key))]+"…")
	if err != nil {
		if errors.Is(err, store.ErrKeyLimit) {
			writeJSON(w, 403, errBody(403, "Key 数量已达上限", "forbidden").Body)
			return
		}
		if errors.Is(err, store.ErrConflict) {
			writeJSON(w, 409, errBody(409, "同名 Key 已存在", "conflict").Body)
			return
		}
		writeJSON(w, 400, errBody(400, err.Error(), "invalid_request_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "key": key, "ok": true})
}

// portalAvailableModels reuses dispatch authorization to expose only models
// backed by an active contribution and an enabled group, excluding disabled models.
func (s *Server) portalAvailableModels(userID int64) map[string]bool {
	p := &Principal{UserID: userID}
	if s.o.attachPortalScope(p) != nil {
		return map[string]bool{}
	}
	return s.o.portalCatalogPermissions(p)
}

// Catalog visibility also requires a supporting account within the user's
// model scope. A model existing only on someone else's site is not usable.
func (o *Orchestrator) portalCatalogPermissions(p *Principal) map[string]bool {
	allowed := map[string]bool{}
	for _, entry := range o.wb.Catalog.ListCached() {
		id := str2(entry["id"])
		if !p.PortalModels[id] {
			continue
		}
		for uid := range o.modelEntryAccountUIDs(entry) {
			if p.ModelScopes[id][uid] {
				allowed[id] = true
				break
			}
		}
	}
	return allowed
}

// portalModels exposes catalog metadata only, never account identifiers or
// credentials. Listing uses the same permission scope as model execution.
func (s *Server) portalModels(w http.ResponseWriter, r *http.Request) {
	p := &Principal{UserID: portalCtx(r).ID}
	items := []map[string]any{}
	if aerr := s.o.attachPortalScope(p); aerr != nil {
		if aerr.Status >= 500 {
			writeAPIErr(w, aerr)
			return
		}
		writeJSON(w, 200, map[string]any{"models": items, "source": s.o.wb.Catalog.Source()})
		return
	}
	visible := s.o.portalCatalogPermissions(p)
	for _, model := range s.o.wb.Catalog.ListCached() {
		id := str2(model["id"])
		if !visible[id] {
			continue
		}
		item := map[string]any{"id": id, "own_account": p.OwnedModels[id], "shared_pool": p.SharedModels[id]}
		for _, field := range []string{"name", "context_length", "max_output_tokens", "vision", "reasoning", "input_modalities", "output_modalities"} {
			if value, ok := model[field]; ok {
				item[field] = value
			}
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool { return str2(items[i]["id"]) < str2(items[j]["id"]) })
	writeJSON(w, 200, map[string]any{"models": items, "source": s.o.wb.Catalog.Source()})
}

func (s *Server) portalToggleKey(w http.ResponseWriter, r *http.Request) {
	user := portalCtx(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	enabled, owned, err := s.o.db.ToggleAppOwned(id, user.ID)
	if err != nil {
		writeJSON(w, 500, errBody(500, "操作失败", "server_error").Body)
		return
	}
	if !owned {
		writeJSON(w, 404, errBody(404, "Key 不存在", "invalid_request_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "enabled": enabled})
}

func (s *Server) portalSetKeyModels(w http.ResponseWriter, r *http.Request) {
	user := portalCtx(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	body, _ := readJSON(r)
	arr, valid := body["allowed_models"].([]any)
	if !valid {
		writeAPIErr(w, errBody(400, "allowed_models 必须为字符串数组", "invalid_request_error"))
		return
	}
	available := s.portalAvailableModels(user.ID)
	req := []string{}
	for _, item := range arr {
		m, _ := item.(string)
		if m = strings.TrimSpace(m); m == "" {
			continue
		}
		if !available[m] {
			writeJSON(w, 403, errBody(403, "模型 "+m+" 不在共享范围内", "model_not_allowed").Body)
			return
		}
		req = append(req, m)
	}
	allowedJSON := "[]"
	if len(req) > 0 {
		b, _ := json.Marshal(req)
		allowedJSON = string(b)
	}
	ok, err := s.o.db.SetAppModelsOwned(id, user.ID, allowedJSON)
	if err != nil {
		writeJSON(w, 500, errBody(500, "操作失败", "server_error").Body)
		return
	}
	if !ok {
		writeJSON(w, 404, errBody(404, "Key 不存在", "invalid_request_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "allowed_models": req})
}

func (s *Server) portalDeleteKey(w http.ResponseWriter, r *http.Request) {
	user := portalCtx(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	ok, err := s.o.db.DeleteAppOwned(id, user.ID)
	if err != nil {
		writeJSON(w, 500, errBody(500, "操作失败", "server_error").Body)
		return
	}
	if !ok {
		// 与"不存在"同响应：不向他人 Key 的存在性提供预言。
		writeJSON(w, 404, errBody(404, "Key 不存在", "invalid_request_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// --- 贡献（WorkBuddy 扫码） ---

// portalContributionTask is a bounded, owner-bound OAuth login task held in
// memory; only its owner can poll it, and it expires with the upstream state.
type portalContributionTask struct {
	userID  int64
	site    string
	created time.Time
	expires time.Time
	polling bool
}

// portalContributionBegin starts a device-authorization login for the user to
// scan. Caps concurrent tasks per user (PORTAL_MAX_TASKS_PER_USER).
func (s *Server) portalContributionBegin(w http.ResponseWriter, r *http.Request) {
	user := portalCtx(r)
	if !s.o.cfg.PortalEnabled {
		writeJSON(w, 403, errBody(403, "门户未启用", "forbidden").Body)
		return
	}
	body, _ := readJSON(r)
	site, _ := body["site"].(string)
	accepted, _ := body["accepted"].(bool)
	if !accepted {
		writeJSON(w, 400, errBody(400, "请先阅读并同意共享范围说明", "invalid_request_error").Body)
		return
	}
	now := time.Now()
	reservation := newToken()
	t := &portalContributionTask{userID: user.ID, created: now, expires: now.Add(portalTaskTTL)}
	s.portalMu.Lock()
	active := 0
	for k, task := range s.portalTasks {
		if now.After(task.expires) && !task.polling {
			delete(s.portalTasks, k)
			continue
		}
		if task.userID == user.ID {
			active++
		}
	}
	if active >= s.o.cfg.PortalMaxTasksPerUser || len(s.portalTasks) >= portalMaxTasks {
		s.portalMu.Unlock()
		writeJSON(w, 429, errBody(429, "扫码任务已达上限，请稍后再试", "rate_limit_error").Body)
		return
	}
	s.portalTasks[reservation] = t
	s.portalMu.Unlock()
	defer s.removePortalTask(reservation)
	res, err := portalOAuthBegin(site)
	if err != nil {
		writeJSON(w, 502, errBody(502, "发起登录失败："+err.Error(), "upstream_error").Body)
		return
	}
	state, _ := res["state"].(string)
	authURL, _ := res["authUrl"].(string)
	siteKey, _ := res["site"].(string)
	if state == "" || authURL == "" {
		writeJSON(w, 502, errBody(502, "上游未返回登录会话", "upstream_error").Body)
		return
	}
	s.portalMu.Lock()
	t.site = siteKey
	delete(s.portalTasks, reservation)
	s.portalTasks[state] = t
	s.portalMu.Unlock()
	writeJSON(w, 200, map[string]any{"task_id": state, "auth_url": authURL, "site": siteKey, "expires_at": t.expires.Unix()})
}

// portalContributionPoll advances the OAuth flow once. The full completion
// (persist credential → dedupe → create contribution → grant) happens inside
// the poll that observes "ready", so completion is one-time by construction:
// the task is removed from the table before the response is written.
func (s *Server) portalContributionPoll(w http.ResponseWriter, r *http.Request) {
	user := portalCtx(r)
	body, _ := readJSON(r)
	taskID, _ := body["task_id"].(string)
	s.portalMu.Lock()
	t := s.portalTasks[taskID]
	if t == nil || t.userID != user.ID {
		s.portalMu.Unlock()
		writeJSON(w, 404, errBody(404, "任务不存在或已过期", "invalid_request_error").Body)
		return
	}
	if time.Now().After(t.expires) {
		delete(s.portalTasks, taskID)
		s.portalMu.Unlock()
		writeJSON(w, 200, map[string]any{"status": "expired"})
		return
	}
	if t.polling {
		s.portalMu.Unlock()
		writeJSON(w, 200, map[string]any{"status": "pending"})
		return
	}
	t.polling = true
	s.portalMu.Unlock()
	defer func() {
		s.portalMu.Lock()
		if s.portalTasks[taskID] == t {
			t.polling = false
		}
		s.portalMu.Unlock()
	}()
	res, err := portalOAuthPoll(taskID, t.site)
	if err != nil {
		s.removePortalTask(taskID)
		writeJSON(w, 200, map[string]any{"status": "failed", "message": "登录失败，请重新发起"})
		return
	}
	if res["status"] != "ready" {
		writeJSON(w, 200, map[string]any{"status": "pending"})
		return
	}
	s.portalMu.Lock()
	if s.portalTasks[taskID] != t || time.Now().After(t.expires) {
		s.portalMu.Unlock()
		writeJSON(w, 200, map[string]any{"status": "cancelled"})
		return
	}
	delete(s.portalTasks, taskID)
	s.portalMu.Unlock()
	account, _ := res["account"].(map[string]any)
	uid, _ := account["uid"].(string)
	auth, _ := res["auth"].(map[string]any)
	site := portalSiteFromAuth(auth)
	if err := s.completePortalContribution(user.ID, uid, site, res); err != nil {
		writeJSON(w, 200, map[string]any{"status": "failed", "message": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	verified := s.o.wb.Catalog.RefreshAccount(ctx, uid)
	// 脱敏输出（HANDOFF §6）：只回打码 uid。
	writeJSON(w, 200, map[string]any{
		"status": "ready", "account_uid_masked": maskUID(uid), "site": site, "models_verified": verified,
	})
}

func (s *Server) removePortalTask(taskID string) {
	s.portalMu.Lock()
	delete(s.portalTasks, taskID)
	s.portalMu.Unlock()
}

// Cancellation removes ownership before an in-flight poll may commit its result.
func (s *Server) portalContributionCancel(w http.ResponseWriter, r *http.Request) {
	body, err := readJSON(r)
	if err != nil {
		writeJSON(w, 400, errBody(400, "bad json", "invalid_request_error").Body)
		return
	}
	taskID, _ := body["task_id"].(string)
	s.portalMu.Lock()
	t := s.portalTasks[taskID]
	if t == nil || t.userID != portalCtx(r).ID {
		s.portalMu.Unlock()
		writeJSON(w, 404, errBody(404, "任务不存在或已过期", "invalid_request_error").Body)
		return
	}
	delete(s.portalTasks, taskID)
	s.portalMu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true, "status": "cancelled"})
}

// Account registration and ownership checks share the admin account mutation lock.
// A pending contribution reserves identity before any credential file is written.
func (s *Server) completePortalContribution(userID int64, uid, site string, res map[string]any) error {
	if uid == "" {
		return errors.New("上游账号信息不完整")
	}
	s.o.wb.AccountMu.Lock()
	defer s.o.wb.AccountMu.Unlock()
	user, err := s.o.db.GetUser(userID)
	if err != nil || user == nil || user.Status != "active" {
		return errors.New("用户状态已变更，请重新登录")
	}
	existing, err := s.o.db.ContributionByAccount(uid)
	if err != nil {
		return errors.New("账号归属查询失败，请重试")
	}
	if existing != nil && existing.UserID != userID {
		return errors.New("该账号已绑定贡献，无法重复贡献")
	}
	if existing != nil && existing.Status == "active" {
		return errors.New("该账号已绑定贡献，无法重复贡献")
	}
	if existing == nil {
		account, err := s.o.db.GetAccount(uid)
		if err != nil {
			return errors.New("账号归属查询失败，请重试")
		}
		if account != nil || s.o.manager(uid) != nil || s.o.wb.Pool.Get(uid) != nil {
			return errors.New("该账号已存在系统中，如需贡献请联系管理员处理")
		}
	}
	auth, _ := res["auth"].(map[string]any)
	if auth == nil {
		return errors.New("上游授权信息不完整")
	}
	access, _ := auth["accessToken"].(string)
	if access == "" {
		return errors.New("上游授权信息不完整")
	}
	if _, err := siterouting.ProfileForAuth(auth); err != nil {
		return errors.New("上游凭据站点无效")
	}
	session := map[string]any{"auth": auth, "account": res["account"]}
	data, err := json.Marshal(session)
	if err != nil {
		return errors.New("授权数据无效")
	}
	// Check before reserving ownership or retiring credentials, including retries.
	path := filepath.Join(s.o.wb.ProjectAuths, "workbuddy-"+safeUID(uid)+".info")
	if old, err := os.ReadFile(path); err == nil {
		var saved struct {
			Account struct {
				UID string `json:"uid"`
			} `json:"account"`
		}
		if existing == nil || json.Unmarshal(old, &saved) != nil || saved.Account.UID != uid {
			return errors.New("账号凭据文件冲突，请联系管理员处理")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("账号凭据文件无法读取，请联系管理员处理")
	}
	id := int64(0)
	if existing == nil {
		id, err = s.o.db.CreateContribution(userID, uid, "workbuddy", site, "verifying")
		if err != nil {
			return errors.New("贡献登记失败，请重试")
		}
	} else {
		id = existing.ID
		ok, err := s.o.db.SetContributionStatus(id, userID, existing.Status, "verifying", "")
		if err != nil || !ok {
			return errors.New("贡献状态已变更，请重试")
		}
	}
	// Failures remain owned but ineligible, so only this user can retry.
	if previous := s.o.manager(uid); previous != nil {
		if err := previous.Retire(false); err != nil {
			return errors.New("旧凭据停止失败，请重试")
		}
	}
	if err := os.MkdirAll(s.o.wb.ProjectAuths, 0700); err != nil {
		return errors.New("凭据保存失败，请重试")
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return errors.New("凭据保存失败，请重试")
	}
	if _, err := wbruntime.PersistAccount(s.o.db, session); err != nil {
		return errors.New("账号登记失败，请重试")
	}
	if err := s.o.db.SetAccountHidden(uid, false); err != nil {
		return errors.New("账号登记失败，请重试")
	}
	mgr := credentials.NewManager(path)
	s.o.wb.Pool.AddAccountDisabled(uid, mgr)
	s.o.setManager(uid, mgr)
	if err := s.o.db.ActivateContributionWithGroups(id, userID); err != nil {
		return errors.New("贡献资格登记失败，请重试")
	}
	s.o.wb.Pool.SetEnabled(uid, true, "")
	return nil
}

// apiErrMsg extracts a human-readable message from an apiError body.
func apiErrMsg(e *apiError) string {
	if em, ok := e.Body["error"].(map[string]any); ok {
		if msg, ok := em["message"].(string); ok {
			return msg
		}
	}
	return "未知错误"
}

// portalSiteFromAuth maps the auth domain to a coarse site label.
func portalSiteFromAuth(auth map[string]any) string {
	domain := ""
	if auth != nil {
		domain, _ = auth["domain"].(string)
	}
	switch {
	case strings.Contains(domain, "codebuddy"):
		return "codebuddy"
	default:
		return "workbuddy"
	}
}

func maskUID(uid string) string {
	if len(uid) <= 4 {
		return strings.Repeat("*", len(uid))
	}
	return uid[:2] + strings.Repeat("*", 6) + uid[len(uid)-2:]
}

func (s *Server) portalRevokeContribution(w http.ResponseWriter, r *http.Request) {
	user := portalCtx(r)
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	body, _ := readJSON(r)
	reason, _ := body["reason"].(string)
	if reason == "" {
		reason = "贡献者撤回共享"
	}
	s.o.wb.AccountMu.Lock()
	defer s.o.wb.AccountMu.Unlock()
	cons, err := s.o.db.UserContributions(user.ID)
	if err != nil {
		writeJSON(w, 500, errBody(500, "查询失败", "server_error").Body)
		return
	}
	var owned *store.Contribution
	for i := range cons {
		if cons[i].ID == id {
			owned = &cons[i]
			break
		}
	}
	if owned == nil {
		writeJSON(w, 404, errBody(404, "贡献不存在", "invalid_request_error").Body)
		return
	}
	ok, err := s.o.db.WithdrawContribution(id, user.ID, reason)
	if err != nil {
		writeJSON(w, 500, errBody(500, "操作失败", "server_error").Body)
		return
	}
	if !ok {
		writeJSON(w, 404, errBody(404, "贡献不存在或状态不允许撤回", "invalid_request_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) contributionCatalogStatus(uid string) string {
	a := s.o.wb.Pool.Get(uid)
	if a == nil || !a.Enabled {
		return "unavailable"
	}
	for _, model := range s.o.wb.Catalog.ListCached() {
		if s.o.modelEntryAccountUIDs(model)[uid] {
			return "ready"
		}
	}
	return "pending"
}

func (s *Server) portalRefreshContributionModels(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	cons, err := s.o.db.UserContributions(portalCtx(r).ID)
	if err != nil {
		writeAPIErr(w, errBody(503, "账号查询失败，请稍后重试", "server_error"))
		return
	}
	for _, c := range cons {
		if c.ID != id {
			continue
		}
		if c.Status != "active" && c.Status != "private" {
			writeAPIErr(w, errBody(409, "请先重新授权账号", "account_unavailable"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if !s.o.wb.Catalog.RefreshAccount(ctx, c.AccountUID) {
			w.Header().Set("Retry-After", "10")
			writeAPIErr(w, errBody(503, "模型目录暂未验证，请稍后重试；账号已保留", "catalog_pending"))
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "catalog_status": "ready"})
		return
	}
	writeAPIErr(w, errBody(404, "账号不存在", "not_found"))
}

func (s *Server) portalListContributions(w http.ResponseWriter, r *http.Request) {
	user := portalCtx(r)
	cons, err := s.o.db.UserContributions(user.ID)
	if err != nil {
		writeJSON(w, 500, errBody(500, "查询失败", "server_error").Body)
		return
	}
	out := []map[string]any{}
	for _, c := range cons {
		out = append(out, map[string]any{
			"id":             c.ID,
			"provider":       c.Provider,
			"site":           c.Site,
			"status":         c.Status,
			"account":        maskUID(c.AccountUID),
			"catalog_status": s.contributionCatalogStatus(c.AccountUID),
			"created_at":     c.CreatedAt,
		})
	}
	writeJSON(w, 200, map[string]any{"contributions": out})
}

func (s *Server) portalShareContribution(w http.ResponseWriter, r *http.Request) {
	if !s.o.cfg.PortalEnabled {
		writeAPIErr(w, errBody(403, "共享服务已关闭", "portal_disabled"))
		return
	}
	body, err := readJSON(r)
	if err != nil || body["accepted"] != true {
		writeAPIErr(w, errBody(400, "请先确认共享账号及额度的使用说明", "invalid_request_error"))
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeAPIErr(w, errBody(404, "账号不存在", "not_found"))
		return
	}
	s.o.wb.AccountMu.Lock()
	defer s.o.wb.AccountMu.Unlock()
	ok, err := s.o.db.RestoreContributionSharing(id, portalCtx(r).ID)
	if err != nil {
		writeAPIErr(w, errBody(503, "恢复共享失败，请重试", "server_error"))
		return
	}
	if !ok {
		writeAPIErr(w, errBody(404, "账号不存在或需要重新验证", "not_found"))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// --- 用量 ---

func (s *Server) portalUsage(w http.ResponseWriter, r *http.Request) {
	user := portalCtx(r)
	reqs, inTok, outTok, err := s.o.db.UserDailyUsage(user.ID, float64(localMidnightUnix()))
	if err != nil {
		writeAPIErr(w, errBody(503, "用量查询失败，请重试", "server_error"))
		return
	}
	known, err := s.o.db.UserDailyUsageKnown(user.ID, float64(localMidnightUnix()))
	if err != nil {
		writeAPIErr(w, errBody(503, "用量查询失败，请重试", "server_error"))
		return
	}
	apps, err := s.o.db.UserApps(user.ID)
	if err != nil {
		writeAPIErr(w, errBody(503, "用量查询失败，请重试", "server_error"))
		return
	}
	var input, output any = inTok, outTok
	if !known {
		input = nil
		output = nil
	}
	writeJSON(w, 200, map[string]any{
		"today": map[string]any{
			"requests":             reqs,
			"input_tokens":         input,
			"output_tokens":        output,
			"daily_limit_requests": s.o.cfg.PortalDailyRequests,
			"daily_limit_input":    s.o.cfg.PortalDailyInputTokens,
			"daily_limit_output":   s.o.cfg.PortalDailyOutputTokens,
		},
		"keys": apps,
	})
}

// localMidnightUnix is the start of the local day (usage day boundary).
func localMidnightUnix() int64 {
	n := time.Now()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location()).Unix()
}

// --- 管理端（/admin/portal/*，经 adminGuard） ---

// adminPortalOverview is the admin console's single fetch: users + invites +
// groups + account counts, so the WebUI portal panel stays one request.
func (s *Server) adminPortalOverview(w http.ResponseWriter, r *http.Request) {
	users, err := s.o.db.ListUsers()
	if err != nil {
		writeJSON(w, 500, errBody(500, "查询失败", "server_error").Body)
		return
	}
	invites, err := s.o.db.ListInviteCodes()
	if err != nil {
		writeJSON(w, 500, errBody(500, "查询失败", "server_error").Body)
		return
	}
	groups, err := s.portalGroupViews()
	if err != nil {
		writeJSON(w, 500, errBody(500, "查询失败", "server_error").Body)
		return
	}
	contributions, err := s.o.db.ListContributions()
	if err != nil {
		writeJSON(w, 500, errBody(500, "查询失败", "server_error").Body)
		return
	}
	rawAccounts, err := s.o.db.ListAccounts()
	if err != nil {
		writeJSON(w, 500, errBody(500, "查询失败", "server_error").Body)
		return
	}
	accounts := []map[string]any{}
	modes, err := s.o.db.PlatformSharingModes()
	if err != nil {
		writeAPIErr(w, errBody(503, "账号使用范围查询失败", "server_error"))
		return
	}
	owners := map[string]store.Contribution{}
	for _, c := range contributions {
		owners[c.AccountUID] = c
	}
	for _, a := range rawAccounts {
		uid := str2(a["uid"])
		mode := modes[uid]
		if mode == "" {
			mode = "private"
		}
		row := map[string]any{"uid": uid, "alias": a["alias"], "nickname": a["nickname"], "provider": a["provider"], "profile": a["profile"], "enabled": a["enabled"], "sharing_mode": mode, "owner_kind": "platform"}
		if c, ok := owners[uid]; ok {
			row["owner_kind"] = "user"
			row["contribution_user_id"] = c.UserID
			row["status"] = c.Status
			row["sharing_mode"] = "personal"
			if c.Status == "active" {
				row["sharing_mode"] = "personal_shared"
			}
		}
		accounts = append(accounts, row)
	}
	settings, err := s.o.db.GetSettings()
	if err != nil {
		writeAPIErr(w, errBody(503, "默认共享池查询失败", "server_error"))
		return
	}
	writeJSON(w, 200, map[string]any{"users": users, "invites": invites, "groups": groups, "contributions": contributions, "accounts": accounts, "account_count": len(accounts), "registration_mode": s.o.cfg.PortalRegistrationMode, "user_concurrency": s.portalUserLimit(0), "user_concurrency_max": s.modelsAdmission.sharedCapacity, "user_concurrency_overrides": portalConcurrencyOverrides(settings), "default_group_id": intOf(settings["portal_default_group"]), "default_auto_grant": settings["portal_default_auto_grant"] == "1"})
}

func (s *Server) adminPortalDefaultGroup(w http.ResponseWriter, r *http.Request) {
	body, err := readJSON(r)
	if err != nil {
		writeAPIErr(w, errBody(400, "无效请求", "invalid_request_error"))
		return
	}
	n, ok := body["group_id"].(float64)
	if !ok || n < 0 || n > 1e12 || n != float64(int64(n)) {
		writeAPIErr(w, errBody(400, "请选择有效共享池", "invalid_request_error"))
		return
	}
	auto, ok := body["auto_grant"].(bool)
	if !ok {
		writeAPIErr(w, errBody(400, "请明确自动授权规则", "invalid_request_error"))
		return
	}
	s.o.wb.AccountMu.Lock()
	defer s.o.wb.AccountMu.Unlock()
	if err := s.o.db.SetDefaultPortalGroup(int64(n), auto); err != nil {
		writeAPIErr(w, errBody(400, err.Error(), "invalid_request_error"))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) adminPortalAccountSharing(w http.ResponseWriter, r *http.Request) {
	body, err := readJSON(r)
	if err != nil {
		writeAPIErr(w, errBody(400, "无效请求", "invalid_request_error"))
		return
	}
	mode, _ := body["mode"].(string)
	raw, ok := body["group_ids"].([]any)
	if !ok {
		writeAPIErr(w, errBody(400, "请选择共享池列表", "invalid_request_error"))
		return
	}
	ids := []int64{}
	for _, value := range raw {
		n, ok := value.(float64)
		if !ok || n <= 0 || n > 1e12 || n != float64(int64(n)) {
			writeAPIErr(w, errBody(400, "共享池 ID 无效", "invalid_request_error"))
			return
		}
		ids = append(ids, int64(n))
	}
	s.o.wb.AccountMu.Lock()
	defer s.o.wb.AccountMu.Unlock()
	if err := s.o.db.SetPlatformAccountSharing(r.PathValue("uid"), mode, ids); err != nil {
		writeAPIErr(w, errBody(400, err.Error(), "invalid_request_error"))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) portalGroupViews() ([]map[string]any, error) {
	groups, err := s.o.db.ListResourceGroups()
	if err != nil {
		return nil, err
	}
	settings, err := s.o.db.GetSettings()
	if err != nil {
		return nil, err
	}
	disabled := map[string]bool{}
	for _, id := range parseJSONStringArray(settings["portal_disabled_models"]) {
		disabled[id] = true
	}
	catalog := s.o.wb.Catalog.ListCached()
	users, err := s.o.db.ListUsers()
	if err != nil {
		return nil, err
	}
	eligibleByGroup := map[int64]int{}
	for _, u := range users {
		if u.Status != "active" {
			continue
		}
		active, err := s.o.db.ActiveContributionUIDs(u.ID)
		if err != nil {
			return nil, err
		}
		if len(active) == 0 {
			continue
		}
		granted, err := s.o.db.GrantedGroups(u.ID)
		if err != nil {
			return nil, err
		}
		for _, group := range granted {
			eligibleByGroup[group.ID]++
		}
	}
	out := []map[string]any{}
	for _, g := range groups {
		accounts, err := s.o.db.GroupAccountUIDs(g.ID)
		if err != nil {
			return nil, err
		}
		grants, err := s.o.db.GroupUserIDs(g.ID)
		if err != nil {
			return nil, err
		}
		models := parseJSONStringArray(g.AllowedModels)
		if models == nil {
			models = []string{}
		}
		backed, err := s.o.db.GroupAccountUIDsWithActiveContribution(g.ID)
		if err != nil {
			return nil, err
		}
		ready := map[string]bool{}
		for _, uid := range backed {
			if a := s.o.wb.Pool.Get(uid); a != nil && a.Enabled && a.CooldownUntil <= nowSec() {
				ready[uid] = true
			}
		}
		usable := map[string]bool{}
		for _, model := range catalog {
			id := str2(model["id"])
			if disabled[id] || id == "auto" || !slices.Contains(models, id) {
				continue
			}
			for uid := range s.o.modelEntryAccountUIDs(model) {
				if ready[uid] && s.o.modelCooldownUntil(uid, id) <= nowSec() {
					usable[id] = true
					break
				}
			}
		}
		eligibleUsers := 0
		if g.Enabled && len(usable) > 0 {
			eligibleUsers = eligibleByGroup[g.ID]
		}
		out = append(out, map[string]any{"id": g.ID, "name": g.Name, "provider": g.Provider, "enabled": g.Enabled, "allowed_models": models, "accounts": accounts, "grants": grants, "created_at": g.CreatedAt, "ready_accounts": len(ready), "usable_models": len(usable), "eligible_users": eligibleUsers})
	}
	return out, nil
}

func (s *Server) adminPortalResetPassword(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	body, err := readJSON(r)
	if err != nil {
		writeJSON(w, 400, errBody(400, "bad json", "invalid_request_error").Body)
		return
	}
	password, _ := body["new_password"].(string)
	hash, err := portalauth.HashPassword(password)
	if err != nil {
		writeJSON(w, 400, errBody(400, err.Error(), "invalid_request_error").Body)
		return
	}
	if !s.identityStepUp(w, r) {
		return
	}
	if err := s.o.db.ManageIdentity(identityFromRequest(r).Actor, id, "password", hash); err != nil {
		writeJSON(w, 400, errBody(400, "用户不存在或重置失败", "invalid_request_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) adminPortalRevokeInvite(w http.ResponseWriter, r *http.Request) {
	ok, err := s.o.db.RevokeInviteCode(strings.ToUpper(r.PathValue("code")))
	if err != nil {
		writeJSON(w, 500, errBody(500, "撤销失败", "server_error").Body)
		return
	}
	if !ok {
		writeJSON(w, 404, errBody(404, "邀请码不存在或已使用", "invalid_request_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func portalConcurrencyOverrides(settings map[string]string) map[string]int {
	limits := map[string]int{}
	_ = json.Unmarshal([]byte(settings["portal_user_concurrency_limits"]), &limits)
	return limits
}

// portalUserLimit applies the user override within the static shared hard cap.
func (s *Server) portalUserLimit(id int64) int {
	limit := positiveOr(s.o.cfg.PortalUserConcurrency, 4)
	if settings, err := s.o.db.GetSettings(); err == nil {
		if n := portalConcurrencyOverrides(settings)[strconv.FormatInt(id, 10)]; id > 0 && n > 0 {
			limit = n
		}
	}
	maxCap := s.modelsAdmission.sharedCapacity
	if limit > 0 && maxCap > 0 && limit > maxCap {
		limit = maxCap
	}
	return positiveOr(limit, 1)
}

func (s *Server) adminPortalUserConcurrency(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	user, err := s.o.db.GetUser(id)
	if err != nil || user == nil {
		writeAPIErr(w, errBody(404, "用户不存在", "not_found"))
		return
	}
	body, err := readJSON(r)
	raw, ok := body["limit"].(float64)
	n := int(raw)
	upper := s.modelsAdmission.sharedCapacity
	if err != nil || !ok || raw != float64(n) || n < 0 || n > upper {
		writeAPIErr(w, errBody(400, "并发需为 0（默认）至共享执行上限之间的整数", "invalid_request_error"))
		return
	}
	s.o.wb.AccountMu.Lock()
	defer s.o.wb.AccountMu.Unlock()
	settings, err := s.o.db.GetSettings()
	if err != nil {
		writeAPIErr(w, errBody(503, "读取设置失败", "server_error"))
		return
	}
	limits := portalConcurrencyOverrides(settings)
	key := strconv.FormatInt(id, 10)
	if n == 0 {
		delete(limits, key)
	} else {
		limits[key] = n
	}
	encoded, _ := json.Marshal(limits)
	if err := s.o.db.SaveSettings(map[string]string{"portal_user_concurrency_limits": string(encoded)}); err != nil {
		writeAPIErr(w, errBody(503, "保存设置失败", "server_error"))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) adminPortalUsers(w http.ResponseWriter, r *http.Request) {
	users, err := s.o.db.ListUsers()
	if err != nil {
		writeJSON(w, 500, errBody(500, "查询失败", "server_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"users": users})
}

func (s *Server) adminPortalUserStatus(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	body, _ := readJSON(r)
	status, _ := body["status"].(string)
	if status != "active" && status != "disabled" {
		writeJSON(w, 400, errBody(400, "status 必须是 active 或 disabled", "invalid_request_error").Body)
		return
	}
	if !s.identityStepUp(w, r) {
		return
	}
	if err := s.o.db.ManageIdentity(identityFromRequest(r).Actor, id, "status", status); err != nil {
		writeAPIErr(w, errBody(400, err.Error(), "identity_rejected"))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) adminPortalInvites(w http.ResponseWriter, r *http.Request) {
	codes, err := s.o.db.ListInviteCodes()
	if err != nil {
		writeJSON(w, 500, errBody(500, "查询失败", "server_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"invites": codes})
}

func (s *Server) adminPortalCreateInvite(w http.ResponseWriter, r *http.Request) {
	body, _ := readJSON(r)
	days := 7.0
	if d, ok := body["valid_days"].(float64); ok && d > 0 && d <= 365 {
		days = d
	}
	code := genInviteCode()
	expires := float64(0)
	if days > 0 {
		expires = float64(time.Now().Unix()) + days*86400
	}
	if err := s.o.db.CreateInviteCode(code, expires); err != nil {
		writeJSON(w, 500, errBody(500, "保存失败", "server_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"code": code, "expires_at": expires})
}

// genInviteCode: 10 chars from Crockford-ish base32 (no 0/O/1/I confusables).
func genInviteCode() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	code := strings.ToUpper(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))
	return strings.Map(func(r rune) rune {
		switch r {
		case 'O':
			return 'Q'
		case 'I':
			return 'J'
		case '1':
			return 'L'
		case '0':
			return 'Z'
		}
		return r
	}, code[:10])
}

func (s *Server) adminPortalGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := s.portalGroupViews()
	if err != nil {
		writeJSON(w, 500, errBody(500, "查询失败", "server_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"groups": groups})
}

func (s *Server) adminPortalCreateGroup(w http.ResponseWriter, r *http.Request) {
	body, _ := readJSON(r)
	name, _ := body["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		writeJSON(w, 400, errBody(400, "分组名称需为 1–64 字符", "invalid_request_error").Body)
		return
	}
	id, err := s.o.db.CreateResourceGroup(name, "workbuddy", "")
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeJSON(w, 409, errBody(409, "同名分组已存在", "conflict").Body)
			return
		}
		writeJSON(w, 400, errBody(400, err.Error(), "invalid_request_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "ok": true})
}

func (s *Server) adminPortalGroupModels(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	body, _ := readJSON(r)
	arr, valid := body["allowed_models"].([]any)
	if !valid {
		writeAPIErr(w, errBody(400, "allowed_models 必须为字符串数组", "invalid_request_error"))
		return
	}
	models := []string{}
	for _, item := range arr {
		m, valid := item.(string)
		if !valid {
			writeAPIErr(w, errBody(400, "模型名称必须为字符串", "invalid_request_error"))
			return
		}
		if m = strings.TrimSpace(m); m != "" {
			m = s.o.resolveModel(m)
			if m == "auto" || strings.Contains(m, "/") {
				writeAPIErr(w, errBody(400, "共享模型需指定具体的 WorkBuddy 模型，不能使用 auto 或其他渠道", "invalid_request_error"))
				return
			}
			models = append(models, m)
		}
	}
	allowed := ""
	if len(models) > 0 {
		b, _ := json.Marshal(models)
		allowed = string(b)
	}
	if err := s.o.db.SetGroupModels(id, allowed); err != nil {
		writeJSON(w, 404, errBody(404, err.Error(), "invalid_request_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "allowed_models": models})
}

func (s *Server) adminPortalGroupEnable(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	body, _ := readJSON(r)
	enabled, _ := body["enabled"].(bool)
	if err := s.o.db.SetGroupEnabled(id, enabled); err != nil {
		writeJSON(w, 404, errBody(404, err.Error(), "invalid_request_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "enabled": enabled})
}

func (s *Server) adminPortalGroupAccounts(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	body, _ := readJSON(r)
	action, _ := body["action"].(string)
	uid, _ := body["account_uid"].(string)
	uid = strings.TrimSpace(uid)
	if uid == "" {
		writeJSON(w, 400, errBody(400, "缺少 account_uid", "invalid_request_error").Body)
		return
	}
	var err error
	switch action {
	case "add":
		c, checkErr := s.o.db.ContributionByAccount(uid)
		valid := checkErr == nil && c != nil && c.Status == "active" && c.Provider == "workbuddy"
		if checkErr == nil && c == nil {
			modes, e := s.o.db.PlatformSharingModes()
			valid = e == nil && (modes[uid] == "shared" || modes[uid] == "both")
		}
		if !valid {
			writeAPIErr(w, errBody(400, "仅可加入有效的 WorkBuddy 共享贡献账户", "invalid_request_error"))
			return
		}
		err = s.o.db.AddGroupAccount(id, uid)
	case "remove":
		err = s.o.db.RemoveGroupAccount(id, uid)
	default:
		writeJSON(w, 400, errBody(400, "action 必须是 add 或 remove", "invalid_request_error").Body)
		return
	}
	if err != nil {
		writeJSON(w, 500, errBody(500, "操作失败", "server_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) adminPortalGroupGrants(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	body, _ := readJSON(r)
	action, _ := body["action"].(string)
	userID, _ := body["user_id"].(float64)
	if userID <= 0 {
		writeJSON(w, 400, errBody(400, "缺少 user_id", "invalid_request_error").Body)
		return
	}
	var err error
	switch action {
	case "grant":
		err = s.o.db.GrantGroup(int64(userID), id)
	case "revoke":
		err = s.o.db.RevokeGroup(int64(userID), id)
	default:
		writeJSON(w, 400, errBody(400, "action 必须是 grant 或 revoke", "invalid_request_error").Body)
		return
	}
	if err != nil {
		writeJSON(w, 500, errBody(500, "操作失败", "server_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) adminPortalGroupDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	ok, _ := s.o.db.DeleteResourceGroup(id)
	writeJSON(w, 200, map[string]any{"ok": ok})
}
