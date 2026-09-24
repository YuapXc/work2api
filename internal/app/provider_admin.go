package app

import (
	"context"
	"net/http"
	"sort"
	"time"

	"work2api/internal/core/provider"
	"work2api/internal/workbuddy/siterouting"
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
	mux.HandleFunc("DELETE /admin/providers/{name}/accounts/{id}", s.adminProviderDeleteAccount)
}

// accountManager resolves a provider to its AccountManager, or writes an error.
func (s *Server) accountManager(w http.ResponseWriter, name string) (provider.AccountManager, bool) {
	rt, ok := provider.RuntimeByName(name)
	if !ok {
		writeJSON(w, 404, errBody(404, "未知供应商："+name, "invalid_request_error").body)
		return nil, false
	}
	am, ok := rt.(provider.AccountManager)
	if !ok {
		writeJSON(w, 400, errBody(400, name+" 不支持账号管理", "invalid_request_error").body)
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
		writeJSON(w, 400, errBody(400, err.Error(), "invalid_request_error").body)
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
		writeJSON(w, 400, errBody(400, err.Error(), "invalid_request_error").body)
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
		writeJSON(w, 400, errBody(400, err.Error(), "invalid_request_error").body)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// oauthRuntime resolves a provider name to its OAuthRuntime, or writes an error.
func (s *Server) oauthRuntime(w http.ResponseWriter, name string) (provider.OAuthRuntime, bool) {
	rt, ok := provider.RuntimeByName(name)
	if !ok {
		writeJSON(w, 404, errBody(404, "未知供应商："+name, "invalid_request_error").body)
		return nil, false
	}
	or, ok := rt.(provider.OAuthRuntime)
	if !ok {
		writeJSON(w, 400, errBody(400, name+" 不支持扫码登录", "invalid_request_error").body)
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
		writeJSON(w, 502, errBody(502, err.Error(), "upstream_error").body)
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
		writeJSON(w, 502, errBody(502, err.Error(), "upstream_error").body)
		return
	}
	writeJSON(w, 200, res)
}

// adminProviders returns a cross-provider summary for the overview cards.
func (s *Server) adminProviders(w http.ResponseWriter, r *http.Request) {
	list := []map[string]any{s.workbuddyAdmin(false)}
	rts := provider.Runtimes()
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
	if name == "workbuddy" {
		writeJSON(w, 200, s.workbuddyAdmin(true))
		return
	}
	rt, ok := provider.RuntimeByName(name)
	if !ok {
		writeJSON(w, 404, errBody(404, "未知供应商："+name, "invalid_request_error").body)
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
	if name == "workbuddy" {
		writeJSON(w, 200, s.runWorkbuddyCheckin(context.Background()))
		return
	}
	rt, ok := provider.RuntimeByName(name)
	if !ok {
		writeJSON(w, 404, errBody(404, "未知供应商："+name, "invalid_request_error").body)
		return
	}
	ci, ok := rt.(provider.Checkiner)
	if !ok {
		writeJSON(w, 400, errBody(400, name+" 不支持签到", "invalid_request_error").body)
		return
	}
	res, err := ci.AdminCheckin(r.Context())
	if err != nil {
		writeJSON(w, 502, errBody(502, err.Error(), "upstream_error").body)
		return
	}
	writeJSON(w, 200, res)
}

func (s *Server) adminProviderCredits(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "workbuddy" {
		s.o.refreshAllCredits(context.Background())
		writeJSON(w, 200, map[string]any{"ok": true, "accounts": s.accountsForDisplay()})
		return
	}
	rt, ok := provider.RuntimeByName(name)
	if !ok {
		writeJSON(w, 404, errBody(404, "未知供应商："+name, "invalid_request_error").body)
		return
	}
	cr, ok := rt.(provider.CreditRefresher)
	if !ok {
		writeJSON(w, 400, errBody(400, name+" 不支持额度查询", "invalid_request_error").body)
		return
	}
	res, err := cr.AdminRefreshCredits(r.Context())
	if err != nil {
		writeJSON(w, 502, errBody(502, err.Error(), "upstream_error").body)
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
	accounts := s.accountsForDisplay()
	var remain, total float64
	healthy := 0
	for _, a := range accounts {
		remain += accountFloat(a, "credits_remaining")
		total += accountFloat(a, "credits_total")
		if h, _ := a["healthy"].(bool); h {
			healthy++
		}
	}
	status := map[string]any{
		"account_count":     len(accounts),
		"healthy_count":     healthy,
		"model_count":       len(s.o.models.ListCached()),
		"credits_remaining": round2(remain),
		"credits_total":     round2(total),
	}
	m := map[string]any{
		"name": "workbuddy", "display_name": "WorkBuddy", "ready": len(accounts) > 0,
		"default": true, "status": status,
		"capabilities": []string{"accounts", "models", "checkin", "credits", "oauth", "upload"},
	}
	if detail {
		entries := s.o.models.ListCached()
		s.attachModelAccounts(entries)
		m["accounts"] = accounts
		m["models"] = entries
	}
	return m
}

// runWorkbuddyCheckin performs the workbuddy checkin loop (shared by the legacy
// /admin/checkin and the per-provider endpoint). International WorkBuddy accounts
// have no checkin activity and already-checked accounts are skipped.
func (s *Server) runWorkbuddyCheckin(ctx context.Context) map[string]any {
	results := []map[string]any{}
	skipped := []string{}
	today := time.Now().Format("2006-01-02")
	dates, _ := s.o.db.CheckinDates()
	for _, a := range s.o.pool.Accounts() {
		mgr := s.o.managers[a.UID]
		if mgr == nil {
			continue
		}
		if a.Provider == "workbuddy" && siterouting.ProfileSite(a.Profile) == "international" {
			continue
		}
		if dates[a.UID] == today {
			skipped = append(skipped, a.UID)
			results = append(results, map[string]any{"uid": a.UID, "ok": false, "message": "今日已签到", "already": true})
			continue
		}
		res, err := billingCheckin(ctx, mgr)
		if err == nil && (res.OK || res.Already) {
			_ = s.o.db.SetCheckinDate(a.UID, today)
		}
		msg := res.Message
		if err != nil && msg == "" {
			msg = err.Error()
		}
		results = append(results, map[string]any{"uid": a.UID, "ok": res.OK, "message": msg, "already": res.Already})
	}
	s.o.refreshAllCredits(ctx)
	return map[string]any{"ok": true, "results": results, "skipped": skipped, "accounts": s.accountsForDisplay()}
}
