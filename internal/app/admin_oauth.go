package app

import (
	"net/http"
)

// adminOAuthBegin starts a browser device-authorization login and returns
// {state, authUrl, site} for the WebUI to render as a QR code.
func (s *Server) adminOAuthBegin(w http.ResponseWriter, r *http.Request) {
	body, _ := readJSON(r)
	site, _ := body["site"].(string)
	res, err := s.o.wb.BeginLogin(site)
	if err != nil {
		writeJSON(w, 502, errBody(502, "发起登录失败："+err.Error(), "upstream_error").Body)
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
		writeJSON(w, 400, errBody(400, "缺少 state", "invalid_request_error").Body)
		return
	}
	res, err := s.o.wb.PollLogin(state, site)
	if err != nil {
		writeJSON(w, 400, errBody(400, "登录失败："+err.Error(), "auth_error").Body)
		return
	}
	if res["status"] != "ready" {
		writeJSON(w, 200, res)
		return
	}
	writeJSON(w, 200, res)
}

// adminOAuthSites lists the supported login sites for the WebUI dropdown.
func (s *Server) adminOAuthSites(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"sites": s.o.wb.LoginSites()})
}
