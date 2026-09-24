package qoder

import (
	"fmt"

	"work2api/internal/core/provider"
	"work2api/internal/qoder/account"
	"work2api/internal/qoder/cosy"
)

// qoder supports adding an account via the PKCE device-authorization (scan)
// login it inherits from qoder2api. This restores the OAuth flow in the WebUI.
var _ provider.OAuthRuntime = (*Runtime)(nil)

// oauthState tracks one in-flight scan login. WaitLogin blocks up to 10 min, so
// it runs in a goroutine and updates this state; the WebUI polls OAuthPoll.
type oauthState struct {
	status  string         // "pending" | "ready" | "error"
	message string         // error detail / progress
	account map[string]any // set on ready
}

// OAuthOptions exposes the region choice (cn / global).
func (r *Runtime) OAuthOptions() []map[string]any {
	return []map[string]any{
		{
			"key":     "region",
			"label":   "区域",
			"default": "cn",
			"values": []map[string]any{
				{"value": "cn", "label": "国内"},
				{"value": "global", "label": "国际"},
			},
		},
	}
}

// OAuthBegin starts a scan login and kicks off the blocking WaitLogin in the
// background; on success the account (+ its device-token secret) is persisted to
// ~/.qoder2api and set active, so the next request builds a bridge for it.
func (r *Runtime) OAuthBegin(opts map[string]any) (map[string]any, error) {
	region := account.NormalizeRegion(str(opts["region"]))
	// Ensure the install salt exists before an OAuth account is used for signing.
	if salt, err := account.EnsureMachineSalt(); err == nil {
		cosy.SetInstallSalt(salt)
		r.mu.Lock()
		r.saltSet = true
		r.mu.Unlock()
	}
	sess, err := account.StartLogin(region)
	if err != nil {
		return nil, err
	}
	r.setOAuth(sess.LoginID, &oauthState{status: "pending"})
	go func(loginID string) {
		acct, err := account.WaitLogin(loginID)
		if err != nil {
			r.setOAuth(loginID, &oauthState{status: "error", message: err.Error()})
			return
		}
		if err := account.Save(acct); err != nil {
			r.setOAuth(loginID, &oauthState{status: "error", message: "保存账号失败：" + err.Error()})
			return
		}
		_ = account.SetActive(acct.ID)
		r.setOAuth(loginID, &oauthState{status: "ready", account: map[string]any{
			"id": acct.ID, "label": acct.Name, "region": string(acct.Region),
		}})
	}(sess.LoginID)
	return map[string]any{"login_id": sess.LoginID, "login_url": sess.LoginURL}, nil
}

// OAuthPoll reports the current login state.
func (r *Runtime) OAuthPoll(loginID string) (map[string]any, error) {
	st := r.getOAuth(loginID)
	if st == nil {
		return map[string]any{"status": "error", "message": "无此登录会话（可能已超时）"}, nil
	}
	out := map[string]any{"status": st.status}
	if st.message != "" {
		out["message"] = st.message
	}
	if st.account != nil {
		out["account"] = st.account
	}
	return out, nil
}

func (r *Runtime) setOAuth(loginID string, st *oauthState) {
	r.oauthMu.Lock()
	defer r.oauthMu.Unlock()
	if r.oauthStates == nil {
		r.oauthStates = map[string]*oauthState{}
	}
	r.oauthStates[loginID] = st
}

func (r *Runtime) getOAuth(loginID string) *oauthState {
	r.oauthMu.Lock()
	defer r.oauthMu.Unlock()
	return r.oauthStates[loginID]
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}
