package runtime

import (
	"context"
	"math"
	"strings"
	"time"
	"work2api/internal/core/provider"
	"work2api/internal/workbuddy/siterouting"
)

func (r *Runtime) AccountsForDisplay() []map[string]any {
	accounts := r.Pool.AllAccounts()
	dates, _ := r.db.CheckinDates()
	today := time.Now().Format("2006-01-02")
	for _, a := range accounts {
		uid, _ := a["uid"].(string)
		// 前端读取的是 checkin_today（见 webui Accounts.vue / types）——
		// 曾误写成 checked_in_today，导致签到成功后页面状态永不同步。
		a["checkin_today"] = dates[uid] == today
		a["label"] = AccountLabel(a)
		a["supports_checkin"] = a["site"] != "international"
	}
	return accounts
}

// siteLabel maps the internal site id to a Chinese display name. Kept on the
// backend so the frontend needn't maintain a duplicate mapping (upstream had
// this translation copy-pasted in three places).
func (r *Runtime) AttachModelAccounts(entries []map[string]any) {
	now := float64(time.Now().UnixNano()) / 1e9
	accounts := r.AccountsForDisplay()
	byProfile := map[string][]map[string]any{}
	for _, a := range accounts {
		p, _ := a["profile"].(string)
		byProfile[p] = append(byProfile[p], a)
	}
	// (uid, model) → cooldown_until for per-model daily-limit cooldowns
	mcd := map[string]float64{}
	if rows, err := r.db.ActiveModelCooldowns(now); err == nil {
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
					"label":          AccountLabel(a),
					"profile":        a["profile"],
					"site":           site,
					"site_label":     SiteLabel(site),
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
func AccountLabel(a map[string]any) string {
	if alias, ok := a["alias"].(string); ok && strings.TrimSpace(alias) != "" {
		return strings.TrimSpace(alias)
	}
	if nick, ok := a["nickname"].(string); ok && ReadableLabel(nick) {
		return nick
	}
	uid, _ := a["uid"].(string)
	if len(uid) > 8 {
		return uid[:8]
	}
	return uid
}
func ReadableLabel(text string) bool {
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
func AccountFloat(a map[string]any, key string) float64 {
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
func (r *Runtime) checkin(ctx context.Context) map[string]any {
	results := []map[string]any{}
	skipped := []string{}
	today := time.Now().Format("2006-01-02")
	dates, _ := r.db.CheckinDates()
	for _, a := range r.Pool.Accounts() {
		mgr := r.Manager(a.UID)
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
		res, err := BillingCheckin(ctx, mgr)
		if err == nil && (res.OK || res.Already) {
			_ = r.db.RecordCheckin(a.UID, today, "manual")
		}
		msg := res.Message
		if err != nil && msg == "" {
			msg = err.Error()
		}
		results = append(results, map[string]any{"uid": a.UID, "ok": res.OK, "message": msg, "already": res.Already})
	}
	r.RefreshAllCredits(ctx, r.db)
	return map[string]any{"ok": true, "results": results, "skipped": skipped, "accounts": r.AccountsForDisplay()}
}

func (r *Runtime) AdminCheckin(ctx context.Context) (map[string]any, error) {
	return r.checkin(ctx), ctx.Err()
}
func (r *Runtime) AdminRefreshCredits(ctx context.Context) (map[string]any, error) {
	r.RefreshAllCredits(ctx, r.db)
	return map[string]any{"ok": true, "accounts": r.AccountsForDisplay()}, ctx.Err()
}
func (r *Runtime) OwnsSessionObservations() bool { return true }

func (r *Runtime) AdminData(ctx context.Context) provider.AdminData {
	accounts := r.AccountsForDisplay()
	var remain, total float64
	healthy := 0
	for _, a := range accounts {
		remain += AccountFloat(a, "credits_remaining")
		total += AccountFloat(a, "credits_total")
		if h, _ := a["healthy"].(bool); h {
			healthy++
		}
	}
	models := r.Catalog.ListCached()
	r.AttachModelAccounts(models)
	resources := []provider.ResourceSummary{}
	for _, a := range accounts {
		alias, _ := a["alias"].(string)
		resources = append(resources, provider.ResourceMetadata(a, "account", map[string]provider.ResourceAction{
			"rename":   {Label: "别名", Enabled: true, Value: alias},
			"delete":   {Label: "删除", Enabled: true, Confirmation: "移出网关分发并保留历史用量；桌面原始凭据保留并持续隐藏，重新导入或授权可恢复。"},
			"enabled":  {Label: "启用", Enabled: true},
			"priority": {Label: "优先级", Enabled: true},
		}))
	}
	return provider.AdminData{Resources: resources, DisplayName: "WorkBuddy", Ready: len(accounts) > 0, Default: true, Capabilities: []string{"accounts", "models", "checkin", "credits", "oauth", "upload"}, Accounts: accounts, Models: models, Status: map[string]any{"account_count": len(accounts), "healthy_count": healthy, "model_count": len(models), "credits_remaining": math.Round(remain*100) / 100, "credits_total": math.Round(total*100) / 100}}
}
func SiteLabel(site string) string {
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
