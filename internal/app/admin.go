package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"work2api/internal/core/provider"
)

func roundN(v float64, places int) float64 {
	p := math.Pow(10, float64(places))
	return math.Round(v*p) / p
}
func round1(v float64) float64 { return roundN(v, 1) }
func round2(v float64) float64 { return roundN(v, 2) }
func round4(v float64) float64 { return roundN(v, 4) }

func parseFloatDefault(s string, def float64) float64 {
	if f, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil {
		return f
	}
	return def
}

func (s *Server) mountAdmin(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/login", s.handleAdminLogin)
	mux.HandleFunc("POST /admin/logout", s.handleAdminLogout)
	mux.HandleFunc("GET /admin/overview", s.adminOverview)
	mux.HandleFunc("GET /admin/accounts", s.adminAccounts)
	mux.HandleFunc("POST /admin/accounts/upload", s.adminUpload)
	mux.HandleFunc("GET /admin/oauth/sites", s.adminOAuthSites)
	mux.HandleFunc("POST /admin/oauth/begin", s.adminOAuthBegin)
	mux.HandleFunc("POST /admin/oauth/poll", s.adminOAuthPoll)
	mux.HandleFunc("POST /admin/accounts/{uid}/enable", s.adminEnable)
	mux.HandleFunc("POST /admin/accounts/{uid}/disable", s.adminDisable)
	mux.HandleFunc("POST /admin/accounts/{uid}/priority", s.adminPriority)
	mux.HandleFunc("POST /admin/accounts/{uid}/alias", s.adminAlias)
	mux.HandleFunc("DELETE /admin/accounts/{uid}", s.adminDeleteAccount)
	mux.HandleFunc("GET /admin/apps", s.adminApps)
	mux.HandleFunc("POST /admin/apps", s.adminCreateApp)
	mux.HandleFunc("GET /admin/apps/{id}/key", s.adminAppKey)
	mux.HandleFunc("POST /admin/apps/{id}/toggle", s.adminToggleApp)
	mux.HandleFunc("POST /admin/apps/{id}/models", s.adminSetAppModels)
	mux.HandleFunc("DELETE /admin/apps/{id}", s.adminDeleteApp)
	mux.HandleFunc("POST /admin/credits/refresh", s.adminRefreshCredits)
	mux.HandleFunc("POST /admin/checkin", s.adminCheckin)
	mux.HandleFunc("GET /admin/checkin/history", s.adminCheckinHistory)
	mux.HandleFunc("GET /admin/usage/summary", s.adminUsageSummary)
	mux.HandleFunc("GET /admin/usage/timeseries", s.adminUsageTimeseries)
	mux.HandleFunc("GET /admin/usage/recent", s.adminUsageRecent)
	mux.HandleFunc("GET /admin/usage/filters", s.adminUsageFilters)
	mux.HandleFunc("GET /admin/usage/{id}", s.adminUsageDetail)
	mux.HandleFunc("GET /admin/models", s.adminModels)
	mux.HandleFunc("GET /admin/models/catalog", s.adminModelCatalog)
	mux.HandleFunc("GET /admin/health", s.adminHealth)
	mux.HandleFunc("POST /admin/models", s.adminRefreshModels)
	mux.HandleFunc("POST /admin/models/refresh", s.adminRefreshModels)
	mux.HandleFunc("GET /admin/models/benchmarks", s.adminBenchmarks)
	mux.HandleFunc("POST /admin/models/benchmarks/refresh", s.adminBenchmarksRefresh)
	mux.HandleFunc("POST /admin/models/test", s.adminModelTest)
	mux.HandleFunc("GET /admin/settings", s.adminGetSettings)
	mux.HandleFunc("POST /admin/settings", s.adminSaveSettings)
	mux.HandleFunc("GET /admin/credentials/export", s.adminExportCredentials)
	s.mountProviderAdmin(mux)
}

func (s *Server) accountsForDisplay() []map[string]any {
	accounts := s.o.pool.AllAccounts()
	dates, _ := s.o.db.CheckinDates()
	today := time.Now().Format("2006-01-02")
	for _, a := range accounts {
		uid, _ := a["uid"].(string)
		// 前端读取的是 checkin_today（见 webui Accounts.vue / types）——
		// 曾误写成 checked_in_today，导致签到成功后页面状态永不同步。
		a["checkin_today"] = dates[uid] == today
		a["label"] = accountLabel(a)
	}
	return accounts
}

// siteLabel maps the internal site id to a Chinese display name. Kept on the
// backend so the frontend needn't maintain a duplicate mapping (upstream had
// this translation copy-pasted in three places).
func siteLabel(site string) string {
	switch site {
	case "domestic":
		return "国内"
	case "international":
		return "国际"
	case "qoder-cn":
		return "Qoder 国内"
	case "qoder-global":
		return "Qoder 国际"
	case "":
		return ""
	default:
		return site
	}
}

// decorateByAccount enriches usage["by_account"] rows with display label / site
// so the Usage page "按账号统计" can render names directly. Must be applied by
// every endpoint returning by_account (overview + usage summary), else that
// column is blank. Accounts deleted since the log was written fall back to a
// uid short code and are flagged removed. Modifies usage in place.
func decorateByAccount(usage map[string]any, accounts []map[string]any) map[string]any {
	byUID := map[string]map[string]any{}
	for _, a := range accounts {
		if uid, _ := a["uid"].(string); uid != "" {
			byUID[uid] = a
		}
	}
	rows, _ := usage["by_account"].([]map[string]any)
	for _, row := range rows {
		uid, _ := row["account_uid"].(string)
		a := byUID[uid]
		if uid != "" && a != nil {
			site, _ := a["site"].(string)
			row["label"] = accountLabel(a)
			row["site"] = site
			row["site_label"] = siteLabel(site)
			row["profile"] = a["profile"]
			row["enabled"] = a["enabled"]
			row["healthy"] = a["healthy"]
			row["removed"] = false
		} else {
			label := uid
			if len(label) > 8 {
				label = label[:8]
			}
			if label == "" {
				label = "（未知）"
			}
			row["label"] = label
			row["site"] = nil
			row["site_label"] = nil
			row["profile"] = nil
			row["enabled"] = nil
			row["healthy"] = nil
			row["removed"] = true
		}
	}
	return usage
}

func accountLabel(a map[string]any) string {
	if alias, ok := a["alias"].(string); ok && strings.TrimSpace(alias) != "" {
		return strings.TrimSpace(alias)
	}
	if nick, ok := a["nickname"].(string); ok && readableLabel(nick) {
		return nick
	}
	uid, _ := a["uid"].(string)
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}

func readableLabel(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	for _, ch := range text {
		if ch < 0x20 || ch == 0x7F || (ch >= 0xE000 && ch <= 0xF8FF) || ch == 0xFFFD {
			return false
		}
	}
	return true
}

func (s *Server) adminOverview(w http.ResponseWriter, r *http.Request) {
	accounts := s.accountsForDisplay()
	summary, _ := s.o.db.UsageSummary()
	writeJSON(w, 200, map[string]any{
		"admission":          s.modelsAdmission.snapshot(),
		"request_body_bytes": s.bodies.usage(),
		"accounts":           accounts,
		"alerts":             s.creditAlerts(accounts),
		"usage":              decorateByAccount(summary, accounts),
		"recent":             s.recentLight(),
		"prediction":         s.creditPrediction(accounts),
		"model_count":        len(s.o.models.ListCached()),
		"model_source":       s.o.models.Source(),
	})
}

func (s *Server) recentLight() []map[string]any {
	rows, _ := s.o.db.UsageRecent(10, "", "", nil, "", true, 0, "")
	if rows == nil {
		return []map[string]any{}
	}
	return rows
}

func accountFloat(a map[string]any, key string) float64 {
	switch v := a[key].(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	case *float64:
		// billing.summarizePackages 的内存态 expire_at 先是 *float64，JSON 归一化
		// 才解引用；creditAlerts 走内存路径（pool.AllAccounts）必须接住指针形态。
		if v != nil {
			return *v
		}
	}
	return 0
}

func accountString(a map[string]any, key string) string {
	if s, ok := a[key].(string); ok {
		return s
	}
	return ""
}

// creditPrediction estimates how many tokens the remaining credits can buy,
// weighting each model's tokens-per-credit by its share of historical usage
// (per-model rates differ hugely, so a global average would be badly skewed).
func (s *Server) creditPrediction(accounts []map[string]any) map[string]any {
	var remaining float64
	for _, a := range accounts {
		remaining += accountFloat(a, "credits_remaining")
	}
	stats, totalCredits, totalTokens := s.o.db.UsageCreditStats()
	pm := []map[string]any{}
	var blended float64
	haveBlended := false
	for _, m := range stats {
		credits, _ := m["credits"].(float64)
		tokens, _ := m["tokens"].(float64)
		if credits < 0.05 { // too little history for a trustworthy ratio
			continue
		}
		tpc := tokens / credits
		weight := 0.0
		if totalTokens > 0 {
			weight = tokens / totalTokens
		}
		var predicted float64
		if remaining > 0 {
			predicted = round1(remaining * tpc)
		}
		pm = append(pm, map[string]any{
			"model": m["model"], "credits": round2(credits), "tokens": int64(tokens),
			"tokens_per_credit": round1(tpc), "weight": round4(weight),
			"predicted_tokens": int64(predicted),
		})
		blended += weight * tpc
		haveBlended = true
	}
	out := map[string]any{
		"remaining_credits": round2(remaining),
		"credits_used":      round2(totalCredits),
		"tokens_used":       int64(totalTokens),
		"models":            pm,
	}
	if haveBlended {
		out["tokens_per_credit"] = round1(blended)
		if remaining > 0 {
			out["predicted_tokens"] = int64(round1(remaining * blended))
		} else {
			out["predicted_tokens"] = nil
		}
	} else {
		out["tokens_per_credit"] = nil
		out["predicted_tokens"] = nil
	}
	return out
}

// creditAlerts reports the current low-balance / near-expiry state for display
// (read-only; scheduler owns the actual webhook push). Uses the same thresholds
// as the scheduler but without its 6h dedupe — opening the page should always
// reflect the live state.
func (s *Server) creditAlerts(accounts []map[string]any) []map[string]any {
	alerts := []map[string]any{}
	settings, _ := s.o.db.GetSettings()
	threshold := parseFloatDefault(settings["alert_threshold_percent"], 20)
	expiryDays := parseFloatDefault(settings["alert_expiry_days"], 7)
	var remaining, capacity float64
	for _, a := range accounts {
		remaining += accountFloat(a, "credits_remaining")
		capacity += accountFloat(a, "credits_total")
	}
	if capacity > 0 {
		pct := remaining / capacity * 100
		if pct < threshold {
			alerts = append(alerts, map[string]any{
				"level": "warning", "kind": "balance",
				"message": fmt.Sprintf("总剩余积分 %.0f/%.0f（%.1f%%），低于预警阈值 %.0f%%", remaining, capacity, pct, threshold),
			})
		}
	}
	now := float64(time.Now().UnixNano()) / 1e9
	for _, a := range accounts {
		if enabled, _ := a["enabled"].(bool); !enabled {
			continue
		}
		// 按资源包粒度提醒（与 WebUI 积分构成浮窗同源）：账号级 credits_expire_at
		// 是最早一个包的到期时间，把整个账号余额说成"将过期"是错的——到期的只是
		// 那一个包。remain<=0 的已耗尽包不计。credit_packages 在内存路径是
		// []map[string]any（pool 直通），JSON round-trip 后是 []any，两种都要接住。
		var pkgs []map[string]any
		switch v := a["credit_packages"].(type) {
		case []map[string]any:
			pkgs = v
		case []any:
			for _, raw := range v {
				if m, ok := raw.(map[string]any); ok {
					pkgs = append(pkgs, m)
				}
			}
		}
		for _, p := range pkgs {
			rem := accountFloat(p, "remain")
			exp := accountFloat(p, "expire_at")
			if exp == 0 || rem <= 0 {
				continue
			}
			daysLeft := (exp - now) / 86400
			if daysLeft >= 0 && daysLeft <= expiryDays {
				alerts = append(alerts, map[string]any{
					"level": "info", "kind": "expiry", "uid": a["uid"],
					"message": fmt.Sprintf("账号 %s 的资源包「%s」剩 %.0f 积分将于 %s 过期（%.1f 天后）",
						accountLabel(a), accountString(p, "name"), rem,
						time.Unix(int64(exp), 0).Format("01-02 15:04"), daysLeft),
				})
			}
		}
	}
	return alerts
}

func (s *Server) adminAccounts(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"accounts": s.accountsForDisplay()})
}

func (s *Server) adminEnable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	s.o.pool.SetEnabled(uid, true, "")
	_ = s.o.db.SetAccountState(uid, map[string]any{"enabled": 1, "disabled_reason": ""})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) adminDisable(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	body, _ := readJSON(r)
	reason, _ := body["reason"].(string)
	if reason == "" {
		reason = "手动停用"
	}
	s.o.pool.SetEnabled(uid, false, reason)
	_ = s.o.db.SetAccountState(uid, map[string]any{"enabled": 0, "disabled_reason": reason})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) adminPriority(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	body, _ := readJSON(r)
	p := intOf(body["priority"])
	s.o.pool.SetPriority(uid, p)
	_ = s.o.db.SetAccountState(uid, map[string]any{"priority": p})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) adminAlias(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	body, _ := readJSON(r)
	alias, _ := body["alias"].(string)
	s.o.pool.SetAlias(uid, alias)
	_ = s.o.db.SetAccountState(uid, map[string]any{"alias": strings.TrimSpace(alias)})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) adminDeleteAccount(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if err := s.o.deleteAccount(uid); err != nil {
		writeJSON(w, 500, errBody(500, err.Error(), "server_error").body)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "uid": uid})
}

func (s *Server) adminUpload(w http.ResponseWriter, r *http.Request) {
	var data []byte
	// Accept both a multipart "file" field (WebUI upload) and a raw JSON body.
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		f, _, ferr := r.FormFile("file")
		if ferr != nil {
			writeJSON(w, 400, errBody(400, "缺少 file 字段", "invalid_request_error").body)
			return
		}
		defer f.Close()
		var err error
		data, err = io.ReadAll(io.LimitReader(f, 4*1024*1024))
		if err != nil {
			writeJSON(w, 400, errBody(400, "read failed", "invalid_request_error").body)
			return
		}
	} else {
		var err error
		data, err = io.ReadAll(io.LimitReader(r.Body, 4*1024*1024))
		if err != nil {
			writeJSON(w, 400, errBody(400, "read failed", "invalid_request_error").body)
			return
		}
	}
	res, aerr := s.o.registerAuthUpload(data)
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	res["ok"] = true
	writeJSON(w, 200, res)
}

// adminBenchmarks returns AA evaluations keyed by namespaced model id, for the
// Models page. Read-only against the cache; it triggers a live fetch only when
// the cache is empty (first use after the key is set), so the 20s AA call never
// rides the normal request path — the scheduler warms/refreshes it otherwise.
func (s *Server) adminBenchmarks(w http.ResponseWriter, r *http.Request) {
	if !s.o.bench.Configured() {
		writeJSON(w, 200, map[string]any{"configured": false, "models": map[string]any{}})
		return
	}
	if !s.o.bench.HasCache() {
		s.o.bench.Refresh()
	}
	out := map[string]any{}
	for _, m := range s.o.benchTargets(r.Context()) {
		if res := s.o.bench.Map(m.provider, m.id, m.name); res != nil {
			out[m.id] = res
		}
	}
	writeJSON(w, 200, map[string]any{"configured": true, "models": out})
}

func (s *Server) adminBenchmarksRefresh(w http.ResponseWriter, r *http.Request) {
	if !s.o.bench.Configured() {
		writeJSON(w, 400, errBody(400, "未配置 Artificial Analysis API Key", "invalid_request_error").body)
		return
	}
	s.o.bench.Refresh()
	writeJSON(w, 200, map[string]any{"ok": true, "configured": true})
}

func (s *Server) adminApps(w http.ResponseWriter, r *http.Request) {
	apps, err := s.o.db.ListApps()
	if err != nil {
		writeJSON(w, 500, errBody(500, err.Error(), "server_error").body)
		return
	}
	writeJSON(w, 200, map[string]any{"apps": apps})
}

func (s *Server) adminCreateApp(w http.ResponseWriter, r *http.Request) {
	body, _ := readJSON(r)
	name, _ := body["name"].(string)
	if strings.TrimSpace(name) == "" {
		writeJSON(w, 400, errBody(400, "name is required", "invalid_request_error").body)
		return
	}
	note, _ := body["note"].(string)
	allowed, err := normalizeAllowedModels(body["allowed_models"])
	if err != nil {
		writeJSON(w, 400, errBody(400, err.Error(), "invalid_request_error").body)
		return
	}
	key := s.o.genAPIKey()
	enc, _ := s.o.crypto.Encrypt(key)
	id, err := s.o.db.CreateApp(name, s.o.hashKey(key), key[:min(10, len(key))]+"…", note, enc, allowed, 0)
	if err != nil {
		writeJSON(w, 400, errBody(400, err.Error(), "invalid_request_error").body)
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "app_id": id, "name": name, "key": key, "ok": true})
}

// normalizeAllowedModels validates a client-supplied model allowlist: nil/absent
// means unrestricted (""), a list must be non-empty strings; the result is the
// JSON array string persisted in apps.allowed_models.
func normalizeAllowedModels(v any) (string, error) {
	if v == nil {
		return "", nil
	}
	arr, ok := v.([]any)
	if !ok {
		return "", fmt.Errorf("allowed_models 必须是字符串数组")
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return "", fmt.Errorf("allowed_models 含空项，必须是非空字符串数组")
		}
		out = append(out, strings.TrimSpace(s))
	}
	if len(out) == 0 {
		return "", fmt.Errorf("allowed_models 不能为空数组；解除限制请明确传 null")
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (s *Server) adminAppKey(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	enc, _ := s.o.db.GetAppKeyEnc(id)
	if enc == "" {
		writeJSON(w, 200, map[string]any{"ok": false, "key": nil, "unavailable": true, "message": "该应用未加密存储 Key（旧版创建），无法查看"})
		return
	}
	hash, _ := s.o.db.GetAppKeyHash(id)
	key := s.o.crypto.DecryptVerified(enc, hash)
	if key == "" {
		writeJSON(w, 200, map[string]any{"ok": false, "key": nil, "unavailable": true, "message": "Key 解密或哈希验证失败，请检查原主密钥备份；无法恢复时需重建 API Key"})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "key": key})
}

func (s *Server) adminToggleApp(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	enabled, err := s.o.db.ToggleApp(id)
	if err != nil {
		writeJSON(w, 404, errBody(404, "not found", "invalid_request_error").body)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "enabled": enabled})
}

func (s *Server) adminDeleteApp(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	ok, _ := s.o.db.DeleteApp(id)
	writeJSON(w, 200, map[string]any{"ok": ok})
}

// adminSetAppModels replaces an app's model allowlist. Body: {allowed_models:
// ["id", ...]} — absent/null clears the restriction (all models allowed); an
// empty array is rejected so "no restriction" and "no models" can't be
// confused.
func (s *Server) adminSetAppModels(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	body, _ := readJSON(r)
	allowed, err := normalizeAllowedModels(body["allowed_models"])
	if err != nil {
		writeJSON(w, 400, errBody(400, err.Error(), "invalid_request_error").body)
		return
	}
	// 预校验：目录里是否真有这个模型，给配置人即时反馈。仅警告不阻断：目录是
	// 动态的，冷门模型可能只是缓存里暂时没有；别名与 runtime 前缀模型放行。
	warnings := []string{}
	aliases := map[string]string{}
	if settings, _ := s.o.db.GetSettings(); settings != nil {
		aliases = parseModelAliases(settings["model_aliases"])
	}
	known := map[string]bool{}
	for _, entry := range s.modelCatalog(r.Context()) {
		known[str2(entry["id"])] = true
	}
	for _, m := range decodeAllowedModels(allowed) {
		if _, isAlias := aliases[m]; isAlias {
			continue
		}
		if !known[m] && !isNamespacedModel(m) {
			warnings = append(warnings, "模型 "+m+" 不在当前模型目录中，请确认拼写")
		}
	}
	if err := s.o.db.SetAppModels(id, allowed); err != nil {
		writeJSON(w, 404, errBody(404, err.Error(), "invalid_request_error").body)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "allowed_models": decodeAllowedModels(allowed), "warnings": warnings})
}

// isNamespacedModel reports whether the id carries a runtime namespace prefix
// (e.g. "qoder/claude-…", "opencode/gpt-…"); those live in runtime catalogs,
// not the workbuddy model cache.
func isNamespacedModel(id string) bool {
	for _, rt := range provider.Runtimes() {
		if strings.HasPrefix(id, rt.Name()+"/") {
			return true
		}
	}
	return false
}

// decodeAllowedModels parses the stored JSON back to a list for responses.
func decodeAllowedModels(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return []string{}
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return []string{}
	}
	return out
}

func (s *Server) adminRefreshCredits(w http.ResponseWriter, r *http.Request) {
	s.o.refreshAllCredits(context.Background())
	writeJSON(w, 200, map[string]any{"ok": true, "accounts": s.accountsForDisplay()})
}

// adminCheckin is the legacy global (workbuddy) checkin endpoint, kept for the
// existing WebUI. It delegates to the shared workbuddy checkin helper.
func (s *Server) adminCheckin(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.runWorkbuddyCheckin(context.Background()))
}

// adminCheckinHistory serves the per-account check-in calendar data:
// {uid: ["YYYY-MM-DD", ...]} for the last N days (default 35, ?days=).
func (s *Server) adminCheckinHistory(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if days <= 0 || days > 366 {
		days = 35
	}
	since := time.Now().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	history, err := s.o.db.CheckinHistory(since)
	if err != nil {
		writeJSON(w, 500, errBody(500, "读取签到历史失败: "+err.Error(), "internal_error").body)
		return
	}
	if history == nil {
		history = map[string][]string{}
	}
	today := time.Now().Format("2006-01-02")
	calendars := map[string]provider.CheckinCalendar{"workbuddy": {History: history, Today: today, WindowDate: today, Timezone: time.Now().Location().String()}}
	var warnings []string
	for _, rt := range provider.Runtimes() {
		if historian, ok := rt.(provider.CheckinHistorian); ok {
			cal, err := historian.CheckinHistory(r.Context(), days)
			if err != nil {
				warnings = append(warnings, rt.Name()+": "+err.Error())
				continue
			}
			calendars[rt.Name()] = cal
		}
	}
	writeJSON(w, 200, map[string]any{"days": days, "since": since, "history": history, "calendars": calendars, "warnings": warnings})
}

func (s *Server) adminUsageSummary(w http.ResponseWriter, r *http.Request) {
	sum, _ := s.o.db.UsageSummary()
	writeJSON(w, 200, decorateByAccount(sum, s.accountsForDisplay()))
}

func (s *Server) adminUsageTimeseries(w http.ResponseWriter, r *http.Request) {
	gran := r.URL.Query().Get("granularity")
	if gran == "" {
		gran = "hour"
	}
	points, _ := strconv.Atoi(r.URL.Query().Get("points"))
	if points <= 0 {
		points = 24
	}
	ts, _ := s.o.db.UsageTimeseries(gran, points, r.URL.Query().Get("model"))
	writeJSON(w, 200, map[string]any{"granularity": gran, "points": points, "data": ts})
}

func (s *Server) adminUsageRecent(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(q.Get("page_size"))
	if pageSize <= 0 {
		pageSize = 20
	}
	var appName *string
	if v := q.Get("app_name"); q.Has("app_name") {
		appName = &v
	}
	rows, _ := s.o.db.UsageRecent(pageSize, q.Get("protocol"), q.Get("model"), appName, q.Get("status"), true, (page-1)*pageSize, q.Get("search"))
	total, _ := s.o.db.UsageCount(q.Get("protocol"), q.Get("model"), appName, q.Get("status"), q.Get("search"))
	writeJSON(w, 200, map[string]any{"records": rows, "total": total, "page": page, "page_size": pageSize})
}

func (s *Server) adminUsageFilters(w http.ResponseWriter, r *http.Request) {
	f, _ := s.o.db.UsageFilters()
	writeJSON(w, 200, f)
}

func (s *Server) adminUsageDetail(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.PathValue("id"))
	rec, _ := s.o.db.GetUsage(id)
	if rec == nil {
		writeJSON(w, 404, errBody(404, "not found", "invalid_request_error").body)
		return
	}
	// 前端 usageDetail 读的是 {record: ...}；此前直接返回 rec 顶层字段，
	// 前端取 .record 为 undefined → 详情弹窗空白。包一层修正。
	writeJSON(w, 200, map[string]any{"record": rec})
}

func (s *Server) adminModels(w http.ResponseWriter, r *http.Request) {
	entries := s.o.models.ListCached()
	s.attachModelAccounts(entries)
	writeJSON(w, 200, map[string]any{"models": entries, "source": s.o.models.Source()})
}

func (s *Server) adminModelCatalog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"models": s.modelCatalog(r.Context())})
}

func (s *Server) adminHealth(w http.ResponseWriter, r *http.Request) {
	healthy := s.o.pool.HealthyCount(nil)
	available := 0
	if healthy > 0 {
		available++
	}
	total := 1
	for _, rt := range provider.Runtimes() {
		total++
		if rt.Ready() {
			available++
		}
	}
	writeJSON(w, 200, map[string]any{"status": "ok", "available_providers": available, "total_providers": total})
}

// attachModelAccounts enriches each model entry with an "accounts" array: the
// exact accounts that returned this model (from account_uids), joined with live
// pool state (enabled / healthy / cooldown) so the WebUI "可用账号" tags render.
// Without this the frontend always saw "暂无可用账号". Modifies entries in place.
func (s *Server) attachModelAccounts(entries []map[string]any) {
	now := float64(time.Now().UnixNano()) / 1e9
	accounts := s.accountsForDisplay()
	byProfile := map[string][]map[string]any{}
	for _, a := range accounts {
		p, _ := a["profile"].(string)
		byProfile[p] = append(byProfile[p], a)
	}
	// (uid, model) → cooldown_until for per-model daily-limit cooldowns
	mcd := map[string]float64{}
	if rows, err := s.o.db.ActiveModelCooldowns(now); err == nil {
		for _, row := range rows {
			uid, _ := row["account_uid"].(string)
			model, _ := row["model"].(string)
			until, _ := row["cooldown_until"].(float64)
			mcd[uid+"\x00"+model] = until
		}
	}
	for _, e := range entries {
		id, _ := e["id"].(string)
		var profiles []string
		switch pv := e["profiles"].(type) {
		case []string:
			profiles = pv
		case []any:
			for _, x := range pv {
				if s, ok := x.(string); ok {
					profiles = append(profiles, s)
				}
			}
		}
		if len(profiles) == 0 {
			if p, _ := e["profile"].(string); p != "" {
				profiles = []string{p}
			}
		}
		allowed := map[string]struct{}{}
		hasAllowed := false
		switch uv := e["account_uids"].(type) {
		case []string:
			hasAllowed = true
			for _, u := range uv {
				allowed[u] = struct{}{}
			}
		case []any:
			hasAllowed = true
			for _, x := range uv {
				if u, ok := x.(string); ok {
					allowed[u] = struct{}{}
				}
			}
		}
		supporters := []map[string]any{}
		for _, p := range profiles {
			for _, a := range byProfile[p] {
				uid, _ := a["uid"].(string)
				if hasAllowed {
					if _, ok := allowed[uid]; !ok {
						continue
					}
				}
				modelCd := mcd[uid+"\x00"+id]
				healthy, _ := a["healthy"].(bool)
				coolUntil, _ := a["cooldown_until"].(float64)
				if modelCd > coolUntil {
					coolUntil = modelCd
				}
				site, _ := a["site"].(string)
				supporters = append(supporters, map[string]any{
					"uid":            uid,
					"label":          accountLabel(a),
					"profile":        a["profile"],
					"site":           site,
					"site_label":     siteLabel(site),
					"enabled":        a["enabled"],
					"healthy":        healthy && modelCd <= now,
					"cooldown_until": coolUntil,
					"model_cooldown": modelCd > now,
				})
			}
		}
		e["accounts"] = supporters
	}
}

func (s *Server) adminRefreshModels(w http.ResponseWriter, r *http.Request) {
	models := s.o.models.Refresh()
	warnings := s.o.refreshRuntimeModels(r.Context())
	writeJSON(w, 200, map[string]any{"ok": true, "models": models, "source": s.o.models.Source(), "count": len(models), "warnings": warnings})
}

func (s *Server) adminGetSettings(w http.ResponseWriter, r *http.Request) {
	settings, _ := s.o.db.GetSettings()
	writeJSON(w, 200, settingsForClient(settings))
}

func (s *Server) adminSaveSettings(w http.ResponseWriter, r *http.Request) {
	body, err := readJSON(r)
	if err != nil {
		writeAPIErr(w, errBody(400, "请求体格式错误", "invalid_request_error"))
		return
	}
	kv := map[string]string{}
	for k, v := range body {
		kv[k] = toStrLoose(v)
	}
	if raw, present := kv["portal_disabled_models"]; present {
		var models []string
		if json.Unmarshal([]byte(raw), &models) != nil || models == nil {
			writeAPIErr(w, errBody(400, "禁用模型必须是 JSON 字符串数组", "invalid_request_error"))
			return
		}
	}
	// User concurrency can only be changed through the validated owner-management endpoint.
	delete(kv, "portal_user_concurrency_limits")
	// 勾选「清除已保存的密钥」时前端不再发 aa_api_key，这里显式清空。
	if b, ok := body["clear_aa_api_key"].(bool); ok && b {
		kv["aa_api_key"] = ""
	}
	delete(kv, "clear_aa_api_key")
	if err := s.o.db.SaveSettings(kv); err != nil {
		writeJSON(w, 400, errBody(400, err.Error(), "invalid_request_error").body)
		return
	}
	settings, _ := s.o.db.GetSettings()
	writeJSON(w, 200, settingsForClient(settings))
}

// settingsForClient never ships the raw aa_api_key to the browser; it derives a
// masked preview + enabled flag the settings page renders instead.
func settingsForClient(settings map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range settings {
		if k == "aa_api_key" {
			continue
		}
		out[k] = v
	}
	key := settings["aa_api_key"]
	out["aa_enabled"] = key != ""
	out["aa_api_key_masked"] = maskSecret(key)
	return out
}

// maskSecret keeps the last 4 chars of a credential for display, hiding the rest.
func maskSecret(key string) string {
	if key == "" {
		return ""
	}
	if len(key) <= 4 {
		return strings.Repeat("•", len(key))
	}
	return strings.Repeat("•", 4) + key[len(key)-4:]
}
