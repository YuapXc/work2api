package runtime

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"work2api/internal/workbuddy/oauth"
)

func (r *Runtime) LoginSites() []string                           { return oauth.Sites() }
func (r *Runtime) BeginLogin(site string) (map[string]any, error) { return oauth.Begin(site) }
func (r *Runtime) PollLogin(state, site string) (map[string]any, error) {
	res, err := oauth.Poll(state, site)
	if err != nil || res["status"] != "ready" {
		return res, err
	}
	data, err := json.Marshal(map[string]any{"auth": res["auth"], "account": res["account"]})
	if err != nil {
		return nil, err
	}
	reg, ae := r.RegisterAuthUpload(data)
	if ae != nil {
		return nil, ae
	}
	reg["status"] = "ready"
	return reg, nil
}
func (r *Runtime) OAuthOptions() []map[string]any {
	values := []map[string]any{}
	for _, site := range r.LoginSites() {
		values = append(values, map[string]any{"value": site, "label": SiteLabel(map[string]string{"cn": "domestic", "intl": "international", "intl-codebuddy": "intl-codebuddy"}[site])})
	}
	return []map[string]any{{"key": "site", "label": "站点", "default": "cn", "values": values}}
}
func (r *Runtime) OAuthBegin(opts map[string]any) (map[string]any, error) {
	site, _ := opts["site"].(string)
	res, err := r.BeginLogin(site)
	if err != nil {
		return nil, err
	}
	data, _ := json.Marshal(map[string]any{"state": res["state"], "site": res["site"]})
	res["login_id"] = base64.RawURLEncoding.EncodeToString(data)
	res["login_url"] = res["authUrl"]
	return res, nil
}
func (r *Runtime) OAuthPoll(id string) (map[string]any, error) {
	data, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil || len(data) > 4096 {
		return nil, fmt.Errorf("无效登录标识")
	}
	var payload map[string]string
	if json.Unmarshal(data, &payload) != nil || payload["state"] == "" {
		return nil, fmt.Errorf("无效登录标识")
	}
	return r.PollLogin(payload["state"], payload["site"])
}
