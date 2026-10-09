package app

import (
	"crypto/subtle"
	"net/http"
	"strconv"
	"strings"
	"time"
	"work2api/internal/portal/portalauth"
	"work2api/internal/store"
)

type adminIdentityKey struct{}
type adminIdentity struct {
	Actor              store.IdentityActor `json:"-"`
	ID                 int64               `json:"id"`
	Username           string              `json:"username"`
	Role               string              `json:"role"`
	Recovery           bool                `json:"recovery"`
	MustChangePassword bool                `json:"must_change_password"`
	VerifiedAt         time.Time           `json:"-"`
}

func identityFromRequest(r *http.Request) adminIdentity {
	v, _ := r.Context().Value(adminIdentityKey{}).(adminIdentity)
	return v
}

func (s *Server) resolveAdminIdentity(r *http.Request) (adminIdentity, bool, bool) {
	token := r.Header.Get("X-Admin-Token")
	if token == "" {
		if value := r.Header.Get("Authorization"); strings.HasPrefix(value, "Bearer ") {
			token = strings.TrimSpace(value[7:])
		}
	}
	if s.o.cfg.AdminToken != "" && token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.o.cfg.AdminToken)) == 1 {
		return adminIdentity{Actor: store.IdentityActor{Recovery: true, Method: "token_header"}, Role: "owner", Recovery: true, VerifiedAt: time.Now()}, false, true
	}
	if cookie, err := r.Cookie(cookieName); err == nil && cookie.Value != "" && s.sessions.validate(cookie.Value) {
		s.sessions.mu.Lock()
		entry, exists := s.sessions.sessions[cookie.Value]
		s.sessions.mu.Unlock()
		if !exists {
			return adminIdentity{}, false, false
		}
		if entry.userID == 0 {
			return adminIdentity{Actor: store.IdentityActor{Recovery: true, Method: "recovery_session"}, Role: "owner", Recovery: true, VerifiedAt: entry.verifiedAt}, true, false
		}
		user, err := s.o.db.GetUser(entry.userID)
		if err != nil || user == nil || user.Status != "active" || !store.IsAdminRole(user.Role) || user.PasswordHash != entry.passwordHash || user.AuthVersion != entry.authVersion {
			s.sessions.drop(cookie.Value)
			return adminIdentity{}, false, false
		}
		return adminIdentity{Actor: store.IdentityActor{ID: user.ID, Version: user.AuthVersion, Method: "password"}, ID: user.ID, Username: user.Username, Role: user.Role, MustChangePassword: user.MustChangePassword, VerifiedAt: entry.verifiedAt}, true, false
	}
	return adminIdentity{}, false, false
}

func (s *Server) identityStepUp(w http.ResponseWriter, r *http.Request) bool {
	identity := identityFromRequest(r)
	if identity.Actor.Method == "" || identity.MustChangePassword || time.Since(identity.VerifiedAt) > 5*time.Minute {
		writeAPIErr(w, errBody(403, "请先验证当前管理密码或恢复 Token", "reauth_required"))
		return false
	}
	return true
}

func (s *Server) mountIdentity(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/identity", s.adminIdentityView)
	mux.HandleFunc("POST /admin/identity/reauth", s.adminIdentityReauth)
	mux.HandleFunc("POST /admin/identity/password", s.adminIdentityPassword)
	mux.HandleFunc("POST /admin/identity/owner", s.adminIdentityOwner)
	mux.HandleFunc("GET /admin/identity/audit", s.adminIdentityAudit)
	mux.HandleFunc("POST /admin/portal/users", s.adminIdentityCreate)
	mux.HandleFunc("POST /admin/portal/users/{id}/role", s.adminIdentityRole)
}
func (s *Server) adminIdentityView(w http.ResponseWriter, r *http.Request) {
	owner, err := s.o.db.HasOwner()
	if err != nil {
		writeAPIErr(w, errBody(503, "身份查询失败", "server_error"))
		return
	}
	writeJSON(w, 200, map[string]any{"identity": identityFromRequest(r), "owner_exists": owner})
}
func (s *Server) adminIdentityReauth(w http.ResponseWriter, r *http.Request) {
	identity := identityFromRequest(r)
	body, err := readJSON(r)
	if err != nil {
		writeAPIErr(w, errBody(400, "无效请求", "invalid_request_error"))
		return
	}
	if !s.loginLimiter.allow("reauth:" + strconv.FormatInt(identity.Actor.ID, 10)) {
		writeAPIErr(w, localOverload("login_attempts"))
		return
	}
	password, _ := body["password"].(string)
	token, _ := body["token"].(string)
	valid := false
	if identity.Recovery {
		valid = s.o.cfg.AdminToken != "" && token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(s.o.cfg.AdminToken)) == 1
	} else {
		user, e := s.o.db.GetUser(identity.Actor.ID)
		valid = e == nil && user != nil && user.Status == "active" && store.IsAdminRole(user.Role) && !user.MustChangePassword && user.AuthVersion == identity.Actor.Version && portalauth.VerifyPassword(user.PasswordHash, password)
	}
	if !valid {
		writeAPIErr(w, errBody(401, "验证失败", "invalid_credentials"))
		return
	}
	s.loginLimiter.clear("reauth:" + strconv.FormatInt(identity.Actor.ID, 10))
	if cookie, e := r.Cookie(cookieName); e == nil {
		s.sessions.mu.Lock()
		entry, ok := s.sessions.sessions[cookie.Value]
		if ok {
			entry.verifiedAt = time.Now()
			s.sessions.sessions[cookie.Value] = entry
		}
		s.sessions.mu.Unlock()
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (s *Server) adminIdentityPassword(w http.ResponseWriter, r *http.Request) {
	identity := identityFromRequest(r)
	if identity.Actor.ID <= 0 {
		writeAPIErr(w, errBody(400, "恢复入口不使用账号密码，请登录个人管理员账号", "invalid_request_error"))
		return
	}
	body, err := readJSON(r)
	if err != nil {
		writeAPIErr(w, errBody(400, "无效请求", "invalid_request_error"))
		return
	}
	if !s.loginLimiter.allow("reauth:" + strconv.FormatInt(identity.Actor.ID, 10)) {
		writeAPIErr(w, localOverload("login_attempts"))
		return
	}
	old, _ := body["old_password"].(string)
	next, _ := body["new_password"].(string)
	if err = portalauth.ChangePassword(s.o.db, identity.Actor.ID, old, next); err != nil {
		writeAPIErr(w, errBody(400, "改密失败，请检查当前密码与新密码格式", "invalid_request_error"))
		return
	}
	s.loginLimiter.clear("reauth:" + strconv.FormatInt(identity.Actor.ID, 10))
	if cookie, e := r.Cookie(cookieName); e == nil {
		s.sessions.drop(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: r.TLS != nil || s.o.cfg.AdminCookieSecure, SameSite: http.SameSiteLaxMode})
	writeJSON(w, 200, map[string]any{"ok": true, "login_required": true})
}
func (s *Server) adminIdentityCreate(w http.ResponseWriter, r *http.Request) {
	if !s.identityStepUp(w, r) {
		return
	}
	body, err := readJSON(r)
	if err != nil {
		writeAPIErr(w, errBody(400, "无效请求", "invalid_request_error"))
		return
	}
	name, _ := body["username"].(string)
	password, _ := body["password"].(string)
	role, _ := body["role"].(string)
	if role == "" {
		role = "user"
	}
	name, hash, err := portalauth.UserPasswordHash(name, password)
	if err != nil {
		writeAPIErr(w, errBody(400, err.Error(), "invalid_request_error"))
		return
	}
	id, err := s.o.db.CreateManagedUser(identityFromRequest(r).Actor, name, hash, role)
	if err != nil {
		writeAPIErr(w, errBody(400, err.Error(), "identity_rejected"))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "id": id})
}
func (s *Server) adminIdentityRole(w http.ResponseWriter, r *http.Request) {
	if !s.identityStepUp(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeAPIErr(w, errBody(400, "无效用户", "invalid_request_error"))
		return
	}
	body, err := readJSON(r)
	if err != nil {
		writeAPIErr(w, errBody(400, "无效请求", "invalid_request_error"))
		return
	}
	role, _ := body["role"].(string)
	if err = s.o.db.ManageIdentity(identityFromRequest(r).Actor, id, "role", role); err != nil {
		writeAPIErr(w, errBody(400, err.Error(), "identity_rejected"))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
func (s *Server) adminIdentityOwner(w http.ResponseWriter, r *http.Request) {
	if !s.identityStepUp(w, r) {
		return
	}
	var body struct {
		Target int64 `json:"target_id"`
		Claim  bool  `json:"claim"`
	}
	// readJSON shares the project's body limits; decode the small fixed fields.
	raw, err := readJSON(r)
	if err != nil {
		writeAPIErr(w, errBody(400, "无效请求", "invalid_request_error"))
		return
	}
	value, ok := raw["target_id"].(float64)
	if !ok || value <= 0 || value > 1e12 || value != float64(int64(value)) {
		writeAPIErr(w, errBody(400, "请选择管理员", "invalid_request_error"))
		return
	}
	body.Target = int64(value)
	body.Claim, _ = raw["claim"].(bool)
	if err = s.o.db.TransferOwner(identityFromRequest(r).Actor, body.Target, body.Claim); err != nil {
		writeAPIErr(w, errBody(400, err.Error(), "identity_rejected"))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "login_required": !identityFromRequest(r).Recovery})
}
func (s *Server) adminIdentityAudit(w http.ResponseWriter, r *http.Request) {
	rows, err := s.o.db.IdentityAudits()
	if err != nil {
		writeAPIErr(w, errBody(503, "审计查询失败", "server_error"))
		return
	}
	writeJSON(w, 200, map[string]any{"records": rows})
}
