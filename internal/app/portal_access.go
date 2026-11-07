package app

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"work2api/internal/core/provider"
	"work2api/internal/store"
)

// Free evidence affects model permission only. Account scopes include all
// supporting public credentials, retaining the channel's normal selection.
func (o *Orchestrator) defaultModelAccess(settings map[string]string) (map[string]bool, map[string]map[string]bool) {
	excluded, err := o.db.PrivatePoolExcludedUIDs()
	return o.defaultModelAccessIn(settings, excluded, err, nil)
}

func (o *Orchestrator) defaultModelAccessIn(settings map[string]string, excluded map[string]bool, exclusionErr error, requested []string) (map[string]bool, map[string]map[string]bool) {
	policy := store.DefaultPolicy(settings)
	aliases := parseModelAliases(settings["model_aliases"])
	resolve := func(m string) string {
		if v, ok := aliases[m]; ok {
			m = v
		}
		return strings.TrimPrefix(m, "workbuddy/")
	}
	defaults := map[string]bool{}
	catalog := map[string]map[string]bool{}
	discoverFree := policy.Mode == "auto"
	eligibleForFreeEvidence := func(uid string) bool { return exclusionErr == nil && !excluded[uid] }
	var selected map[string]map[string]bool
	if requested != nil {
		selected = map[string]map[string]bool{}
		for _, model := range requested {
			id := resolve(model)
			if rt, ok := o.runtimes.Resolve(id); ok {
				name := rt.Name()
				if selected[name] == nil {
					selected[name] = map[string]bool{}
				}
				selected[name][id] = true
			}
		}
	}
	for _, rt := range o.runtimes.Runtimes() {
		models := selected[rt.Name()]
		if selected != nil && len(models) == 0 {
			continue
		}
		portal, ok := rt.(provider.PortalProvider)
		source, hasCatalog := rt.(provider.PortalCatalog)
		if !ok || !portal.SupportsPortal() || !hasCatalog {
			continue
		}
		for _, model := range source.PortalModels(context.Background(), models, discoverFree, eligibleForFreeEvidence) {
			if models != nil && !models[model.ID] {
				continue
			}
			catalog[model.ID] = model.Accounts
			if discoverFree && model.FreeSource {
				defaults[model.ID] = true
			}
		}
	}
	for _, m := range policy.Include {
		defaults[resolve(m)] = true
	}
	for _, m := range policy.Exclude {
		delete(defaults, resolve(m))
	}
	delete(defaults, "auto")
	return defaults, catalog
}

func accessModels(body map[string]any, key string) ([]string, bool) {
	a, ok := body[key].([]any)
	if !ok {
		return nil, false
	}
	out := []string{}
	seen := map[string]bool{}
	for _, v := range a {
		m, ok := v.(string)
		m = strings.TrimSpace(m)
		if !ok || m == "" || m == "auto" || m == "workbuddy/auto" {
			return nil, false
		}
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out, true
}

func (s *Server) adminPortalModelPolicy(w http.ResponseWriter, r *http.Request) {
	body, err := readJSON(r)
	if err != nil {
		writeAPIErr(w, errBody(400, "无效请求", "invalid_request_error"))
		return
	}
	mode, _ := body["mode"].(string)
	include, a := accessModels(body, "include")
	exclude, b := accessModels(body, "exclude")
	if (mode != "auto" && mode != "manual") || !a || !b {
		writeAPIErr(w, errBody(400, "请选择自动或手工模式，并提供有效模型列表", "invalid_request_error"))
		return
	}
	raw, _ := json.Marshal(store.DefaultModelPolicy{Mode: mode, Include: include, Exclude: exclude})
	if err := s.o.db.SaveSettings(map[string]string{"portal_model_policy": string(raw)}); err != nil {
		writeAPIErr(w, errBody(503, "保存失败", "server_error"))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) adminPortalUserModels(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeAPIErr(w, errBody(400, "无效用户", "invalid_request_error"))
		return
	}
	body, err := readJSON(r)
	models, ok := accessModels(body, "models")
	remove, valid := accessModels(body, "remove_scoped")
	if err != nil || !ok || !valid {
		writeAPIErr(w, errBody(400, "请提供有效模型列表", "invalid_request_error"))
		return
	}
	existing, err := s.o.db.UserModelGrants(id)
	if err != nil {
		writeAPIErr(w, errBody(503, "授权查询失败", "server_error"))
		return
	}
	// Upgrading a scoped grant to the whole pool requires first explicitly
	// removing it, rather than silently expanding paid legacy permissions.
	for _, g := range existing {
		for _, m := range models {
			if m == g.Model && len(g.AccountUIDs) > 0 && g.AccountUIDs[0] != "*" {
				removed := false
				for _, v := range remove {
					removed = removed || v == m
				}
				if !removed {
					writeAPIErr(w, errBody(409, "该模型仍有历史账号范围，请先明确移除历史授权再重新授权", "conflict"))
					return
				}
			}
		}
	}
	if err := s.o.db.SetUserPublicModels(id, models, remove); err != nil {
		writeAPIErr(w, errBody(400, "用户不存在或保存失败", "invalid_request_error"))
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
