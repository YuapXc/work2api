package app

import (
	"net/http"
	"time"
	"work2api/internal/trae"
)

// Authentication, same-origin writes and request limits are enforced by portalGuard.
func (s *Server) portalTraeOAuth(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.o.runtimes.ByName("trae")
	if !ok {
		writeAPIErr(w, errBody(503, "Trae 渠道不可用", "provider_unavailable"))
		return
	}
	channel, ok := rt.(*trae.Runtime)
	if !ok {
		writeAPIErr(w, errBody(503, "Trae 渠道不可用", "provider_unavailable"))
		return
	}
	body, err := readJSON(r)
	if err != nil {
		writeAPIErr(w, errBody(400, "请求格式错误", "invalid_request_error"))
		return
	}
	owner := portalCtx(r).ID
	id := str2(body["task_id"])
	var result map[string]any
	switch r.PathValue("action") {
	case "begin":
		if !s.o.cfg.PortalEnabled || body["accepted"] != true {
			writeAPIErr(w, errBody(403, "请确认贡献及共享范围，且门户必须已启用", "forbidden"))
			return
		}
		result, err = channel.BeginOwnedLogin(map[string]any{"product": body["product"], "account_type": body["account_type"]}, owner)
		if err == nil {
			result = map[string]any{"task_id": result["login_id"], "auth_url": result["login_url"], "manual_callback": true, "expires_at": time.Now().Add(10 * time.Minute).Unix()}
		}
	case "complete":
		err = channel.CompleteOwnedLogin(id, str2(body["callback_url"]), owner)
		result = map[string]any{"ok": true}
	case "poll":
		result, err = channel.PollOwnedLogin(id, owner)
		if err == nil && result["status"] == "ready" {
			account, _ := result["account"].(map[string]any)
			result = map[string]any{"status": "ready", "account_uid_masked": maskUID(str2(account["uid"])), "models_verified": true}
		}
	case "cancel":
		err = channel.CancelOwnedLogin(id, owner)
		result = map[string]any{"ok": true}
	default:
		writeAPIErr(w, errBody(404, "操作不存在", "not_found"))
		return
	}
	if err != nil {
		writeAPIErr(w, errBody(400, err.Error(), "invalid_request_error"))
		return
	}
	writeJSON(w, 200, result)
}
