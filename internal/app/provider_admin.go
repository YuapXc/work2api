package app

import (
	"context"
	"net/http"
	"sort"

	"work2api/internal/core/provider"
)

func (s *Server) mountProviderAdmin(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/providers", s.adminProviders)
	mux.HandleFunc("GET /admin/providers/{name}", s.adminProviderDetail)
	mux.HandleFunc("POST /admin/providers/{name}/checkin", s.adminProviderCheckin)
	mux.HandleFunc("POST /admin/providers/{name}/credits/refresh", s.adminProviderCredits)
	mux.HandleFunc("GET /admin/providers/{name}/oauth/options", s.adminProviderOAuthOptions)
	mux.HandleFunc("POST /admin/providers/{name}/oauth/begin", s.adminProviderOAuthBegin)
	mux.HandleFunc("POST /admin/providers/{name}/oauth/poll", s.adminProviderOAuthPoll)
	mux.HandleFunc("POST /admin/providers/{name}/accounts/{id}/activate", s.adminProviderActivate)
	mux.HandleFunc("POST /admin/providers/{name}/accounts/{id}/rename", s.adminProviderRename)
	mux.HandleFunc("POST /admin/providers/{name}/accounts/import", s.adminProviderImportAccount)
	mux.HandleFunc("DELETE /admin/providers/{name}/accounts/{id}", s.adminProviderDeleteAccount)
	mux.HandleFunc("GET /admin/providers/{name}/config", s.adminProviderGetConfig)
	mux.HandleFunc("POST /admin/providers/{name}/config", s.adminProviderSaveConfig)
}

// configRuntime resolves a provider to its ConfigRuntime, or writes an error.
func (s *Server) configRuntime(w http.ResponseWriter, name string) (provider.ConfigRuntime, bool) {
	rt, ok := s.o.runtimes.ByName(name)
	if !ok {
		writeJSON(w, 404, errBody(404, "未知供应商："+name, "invalid_request_error").Body)
		return nil, false
	}
	cr, ok := rt.(provider.ConfigRuntime)
	if !ok {
		writeJSON(w, 400, errBody(400, name+" 不支持配置编辑", "invalid_request_error").Body)
		return nil, false
	}
	return cr, true
}

func (s *Server) adminProviderGetConfig(w http.ResponseWriter, r *http.Request) {
	cr, ok := s.configRuntime(w, r.PathValue("name"))
	if !ok {
		return
	}
	doc, err := cr.ConfigDoc()
	if err != nil {
		writeJSON(w, 500, errBody(500, err.Error(), "internal_error").Body)
		return
	}
	writeJSON(w, 200, doc)
}

func (s *Server) adminProviderSaveConfig(w http.ResponseWriter, r *http.Request) {
	cr, ok := s.configRuntime(w, r.PathValue("name"))
	if !ok {
		return
	}
	body, _ := readJSON(r)
	if err := cr.SaveConfigDoc(body); err != nil {
		writeJSON(w, 400, errBody(400, err.Error(), "invalid_request_error").Body)
		return
	}
	// 返回刷新后的配置视图（由可能已被替换的新运行时提供）
	if rt, ok := s.o.runtimes.ByName(r.PathValue("name")); ok {
		if ncr, ok := rt.(provider.ConfigRuntime); ok {
			if doc, err := ncr.ConfigDoc(); err == nil {
				writeJSON(w, 200, map[string]any{"ok": true, "config": doc})
				return
			}
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// accountManager resolves a provider to its AccountManager, or writes an error.
func (s *Server) accountManager(w http.ResponseWriter, name string) (provider.AccountManager, bool) {
	rt, ok := s.o.runtimes.ByName(name)
	if !ok {
		writeJSON(w, 404, errBody(404, "未知供应商："+name, "invalid_request_error").Body)
		return nil, false
	}
	am, ok := rt.(provider.AccountManager)
	if !ok {
		writeJSON(w, 400, errBody(400, name+" 不支持账号管理", "invalid_request_error").Body)
		return nil, false
	}
	return am, true
}

func (s *Server) adminProviderActivate(w http.ResponseWriter, r *http.Request) {
	am, ok := s.accountManager(w, r.PathValue("name"))
	if !ok {
		return
	}
	if err := am.ActivateAccount(r.PathValue("id")); err != nil {
		writeJSON(w, 400, errBody(400, err.Error(), "invalid_request_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) adminProviderRename(w http.ResponseWriter, r *http.Request) {
	am, ok := s.accountManager(w, r.PathValue("name"))
	if !ok {
		return
	}
	body, _ := readJSON(r)
	name, _ := body["name"].(string)
	if err := am.RenameAccount(r.PathValue("id"), name); err != nil {
		writeJSON(w, 400, errBody(400, err.Error(), "invalid_request_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) adminProviderDeleteAccount(w http.ResponseWriter, r *http.Request) {
	am, ok := s.accountManager(w, r.PathValue("name"))
	if !ok {
		return
	}
	if err := am.DeleteAccount(r.PathValue("id")); err != nil {
		writeJSON(w, 400, errBody(400, err.Error(), "invalid_request_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// adminProviderImportAccount adds an account from a pasted personal access
// token (the headless-server path; qoder PAT). The token is validated against
// the upstream before anything is persisted.
func (s *Server) adminProviderImportAccount(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.o.runtimes.ByName(r.PathValue("name"))
	if !ok {
		writeJSON(w, 404, errBody(404, "未知供应商："+r.PathValue("name"), "invalid_request_error").Body)
		return
	}
	imp, ok := rt.(provider.AccountImporter)
	if !ok {
		writeJSON(w, 400, errBody(400, r.PathValue("name")+" 不支持凭 token 添加账号", "invalid_request_error").Body)
		return
	}
	body, err := readJSON(r)
	if err != nil {
		writeJSON(w, 400, errBody(400, "bad json", "invalid_request_error").Body)
		return
	}
	token, _ := body["token"].(string)
	if token == "" {
		writeJSON(w, 400, errBody(400, "token 不能为空", "invalid_request_error").Body)
		return
	}
	delete(body, "token")
	acct, err := imp.AddAccountByToken(r.Context(), token, body)
	if err != nil {
		writeJSON(w, 502, errBody(502, err.Error(), "upstream_error").Body)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "account": acct})
}

// oauthRuntime resolves a provider name to its OAuthRuntime, or writes an error.
func (s *Server) oauthRuntime(w http.ResponseWriter, name string) (provider.OAuthRuntime, bool) {
	rt, ok := s.o.runtimes.ByName(name)
	if !ok {
		writeJSON(w, 404, errBody(404, "未知供应商："+name, "invalid_request_error").Body)
		return nil, false
	}
	or, ok := rt.(provider.OAuthRuntime)
	if !ok {
		writeJSON(w, 400, errBody(400, name+" 不支持扫码登录", "invalid_request_error").Body)
		return nil, false
	}
	return or, true
}

func (s *Server) adminProviderOAuthOptions(w http.ResponseWriter, r *http.Request) {
	or, ok := s.oauthRuntime(w, r.PathValue("name"))
	if !ok {
		return
	}
	writeJSON(w, 200, map[string]any{"options": or.OAuthOptions()})
}

func (s *Server) adminProviderOAuthBegin(w http.ResponseWriter, r *http.Request) {
	or, ok := s.oauthRuntime(w, r.PathValue("name"))
	if !ok {
		return
	}
	body, _ := readJSON(r)
	res, err := or.OAuthBegin(body)
	if err != nil {
		writeJSON(w, 502, errBody(502, err.Error(), "upstream_error").Body)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) adminProviderOAuthPoll(w http.ResponseWriter, r *http.Request) {
	or, ok := s.oauthRuntime(w, r.PathValue("name"))
	if !ok {
		return
	}
	body, _ := readJSON(r)
	loginID, _ := body["login_id"].(string)
	res, err := or.OAuthPoll(loginID)
	if err != nil {
		writeJSON(w, 502, errBody(502, err.Error(), "upstream_error").Body)
		return
	}
	writeJSON(w, 200, res)
}

// adminProviders returns a cross-provider summary for the overview cards.
func (s *Server) adminProviders(w http.ResponseWriter, r *http.Request) {
	list := []map[string]any{}
	rts := s.o.runtimes.Runtimes()
	sort.Slice(rts, func(i, j int) bool { return rts[i].Name() < rts[j].Name() })
	for _, rt := range rts {
		if ar, ok := rt.(provider.AdminRuntime); ok {
			list = append(list, runtimeSummary(rt.Name(), ar.AdminData(r.Context())))
		}
	}
	writeJSON(w, 200, map[string]any{"providers": list})
}

// adminProviderDetail returns one provider's full accounts + models + status.
func (s *Server) adminProviderDetail(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rt, ok := s.o.runtimes.ByName(name)
	if !ok {
		writeJSON(w, 404, errBody(404, "未知供应商："+name, "invalid_request_error").Body)
		return
	}
	ar, ok := rt.(provider.AdminRuntime)
	if !ok {
		writeJSON(w, 200, map[string]any{"name": name, "ready": rt.Ready()})
		return
	}
	writeJSON(w, 200, adminDataToMap(name, ar.AdminData(r.Context())))
}

func (s *Server) adminProviderCheckin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rt, ok := s.o.runtimes.ByName(name)
	if !ok {
		writeJSON(w, 404, errBody(404, "未知供应商："+name, "invalid_request_error").Body)
		return
	}
	ci, ok := rt.(provider.Checkiner)
	if !ok {
		writeJSON(w, 400, errBody(400, name+" 不支持签到", "invalid_request_error").Body)
		return
	}
	res, err := ci.AdminCheckin(r.Context())
	if err != nil {
		writeJSON(w, 502, errBody(502, err.Error(), "upstream_error").Body)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) adminProviderCredits(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	rt, ok := s.o.runtimes.ByName(name)
	if !ok {
		writeJSON(w, 404, errBody(404, "未知供应商："+name, "invalid_request_error").Body)
		return
	}
	cr, ok := rt.(provider.CreditRefresher)
	if !ok {
		writeJSON(w, 400, errBody(400, name+" 不支持额度查询", "invalid_request_error").Body)
		return
	}
	res, err := cr.AdminRefreshCredits(r.Context())
	if err != nil {
		writeJSON(w, 502, errBody(502, err.Error(), "upstream_error").Body)
		return
	}
	writeJSON(w, 200, res)
}

// runtimeSummary reduces an AdminData to the overview-card fields.
func runtimeSummary(name string, d provider.AdminData) map[string]any {
	return map[string]any{
		"name": name, "display_name": d.DisplayName, "ready": d.Ready,
		"default": d.Default, "capabilities": d.Capabilities, "status": d.Status,
		"notes": d.Notes,
	}
}

// adminDataToMap is the full-detail shape (adds accounts + models).
func adminDataToMap(name string, d provider.AdminData) map[string]any {
	m := runtimeSummary(name, d)
	m["accounts"] = d.Accounts
	m["models"] = d.Models
	return m
}

// workbuddyAdmin builds the default provider's summary (detail=false) or full
// detail (detail=true) from the existing pool + models, matching the AdminData
// shape the runtimes emit so the frontend treats all providers uniformly.
func (s *Server) workbuddyAdmin(detail bool) map[string]any {
	d := s.o.wb.AdminData(context.Background())
	if detail {
		return adminDataToMap("workbuddy", d)
	}
	return runtimeSummary("workbuddy", d)
}
func (s *Server) runWorkbuddyCheckin(ctx context.Context) map[string]any {
	res, _ := s.o.wb.AdminCheckin(ctx)
	return res
}
