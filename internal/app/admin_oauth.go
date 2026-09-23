package app

import (
	"encoding/json"
	"net/http"

	"work2api/internal/workbuddy/oauth"
)

// adminOAuthBegin starts a browser device-authorization login and returns
// {state, authUrl, site} for the WebUI to render as a QR code.
func (s *Server) adminOAuthBegin(w http.ResponseWriter, r *http.Request) {
	body, _ := readJSON(r)
	site, _ := body["site"].(string)
	res, err := oauth.Begin(site)
	if err != nil {
		writeJSON(w, 502, errBody(502, "发起登录失败："+err.Error(), "upstream_error").body)
		return
	}
	writeJSON(w, 200, res)
}

// adminOAuthPoll polls a pending login once. On "ready" it persists the
// credential (auth file + pool + DB) and returns {status:"ready", uid, added};
// otherwise {status:"pending"}. A fatal upstream error ends the flow with 400.
func (s *Server) adminOAuthPoll(w http.ResponseWriter, r *http.Request) {
	body, _ := readJSON(r)
	state, _ := body["state"].(string)
	site, _ := body["site"].(string)
	if state == "" {
		writeJSON(w, 400, errBody(400, "缺少 state", "invalid_request_error").body)
		return
	}
	res, err := oauth.Poll(state, site)
	if err != nil {
		writeJSON(w, 400, errBody(400, "登录失败："+err.Error(), "auth_error").body)
		return
	}
	if res["status"] != "ready" {
		writeJSON(w, 200, res)
		return
	}
	// ready: persist {auth, account} exactly like an uploaded auth file.
	session := map[string]any{"auth": res["auth"], "account": res["account"]}
	data, _ := json.Marshal(session)
	reg, aerr := s.o.registerAuthUpload(data)
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	reg["status"] = "ready"
	writeJSON(w, 200, reg)
}

// adminOAuthSites lists the supported login sites for the WebUI dropdown.
func (s *Server) adminOAuthSites(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"sites": oauth.Sites()})
}
