package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"work2api/internal/core/provider"
	"work2api/internal/qoder/account"
	"work2api/internal/qoder/checkin"
)

// Optional admin capabilities: qoder exposes account/model management, checkin,
// quota and per-account actions. These are read/actioned through the generic
// /admin/providers/qoder endpoints so the WebUI treats every provider uniformly.
var (
	_ provider.AdminRuntime    = (*Runtime)(nil)
	_ provider.Checkiner       = (*Runtime)(nil)
	_ provider.CreditRefresher = (*Runtime)(nil)
	_ provider.AccountManager  = (*Runtime)(nil)
)

const quotaTTL = 30 * time.Second

// AdminData reports qoder's accounts (native ~/.qoder2api + locally-detected
// desktop credentials), models and a status summary for the WebUI. Each account
// row is enriched to mirror the workbuddy account table: quota (额度), checkin
// state (今日签到 / 连续), active/health status and source — everything qoder can
// actually provide. Quota is served from a short TTL cache and refreshed in the
// background on a miss, so page loads never block on the upstream billing call.
func (r *Runtime) AdminData(ctx context.Context) provider.AdminData {
	d := provider.AdminData{
		DisplayName:  "Qoder",
		Ready:        r.Ready(),
		Capabilities: []string{"accounts", "models", "checkin", "credits", "oauth", "local_detect"},
	}
	accts := []map[string]any{}
	if dataDirExists() {
		if list, err := account.List(); err == nil {
			for _, a := range list {
				accts = append(accts, r.accountRow(a.ID, a.Name, string(a.Region), "native", a.AuthMode, a.Active))
			}
		}
	}
	local := r.detectLocal()
	for i, c := range local {
		row := r.accountRow("qoder-local-"+c.Region, "本地 Qoder（"+c.Region+"）", c.Region, "local", "device", i == 0 && len(accts) == 0)
		accts = append(accts, row)
	}
	d.Accounts = accts

	models := []map[string]any{}
	for _, m := range r.Models(ctx) {
		row := map[string]any{"id": m.ID, "name": m.Name, "context": m.Context, "max_output": m.MaxOutput}
		for k, v := range m.Extra {
			row[k] = v
		}
		models = append(models, row)
	}
	d.Models = models

	region := ""
	if acct, _, err := r.pickAccount(); err == nil {
		region = string(acct.Region)
	}
	d.Status = map[string]any{
		"account_count": len(accts), "model_count": len(models), "region": region,
	}
	if !d.Ready {
		d.Notes = "未探测到 Qoder 桌面登录，也没有 ~/.qoder2api 账号；请在本机登录 Qoder 桌面端，或导入账号"
	}
	return d
}

// accountRow builds one enriched account row (aligned with the workbuddy columns).
func (r *Runtime) accountRow(id, label, region, source, authMode string, active bool) map[string]any {
	hasSecret := source == "local" || account.HasSecret(id)
	row := map[string]any{
		"id": id, "label": label, "region": region, "source": source,
		"auth_mode": authMode, "active": active, "has_secret": hasSecret,
		"healthy": hasSecret, // qoder has no cooldown pool; usable == has a secret
	}
	// checkin state from local history
	streak, _, totalCredits, claimedToday := checkin.LocalStats(id)
	row["checkin_today"] = claimedToday
	row["streak_days"] = streak
	row["checkin_total_credits"] = totalCredits
	// quota (cached; background refresh on miss)
	if q, ok := r.quotaFor(id, source, region); ok {
		row["credits_remaining"] = q.remaining
		row["credits_total"] = q.total
		row["plan"] = q.plan
		row["quota_exceeded"] = q.exceeded
		if q.expiresAt > 0 {
			row["credits_expire_at"] = q.expiresAt
		}
	}
	return row
}

// tokenFor resolves an account id to its device token (native secret or local).
func (r *Runtime) tokenFor(id, source, region string) string {
	if source == "local" {
		for _, c := range r.detectLocal() {
			if c.Region == region {
				return c.DeviceToken
			}
		}
		return ""
	}
	sec, err := account.GetSecret(id)
	if err != nil {
		return ""
	}
	return deviceTokenFromSecret(sec)
}

// quotaFor returns a cached quota snapshot, kicking a background refresh on a
// stale/absent entry so the next poll shows it without blocking this load.
func (r *Runtime) quotaFor(id, source, region string) (quotaEntry, bool) {
	r.quotaMu.Lock()
	e, ok := r.quotaCache[id]
	fresh := ok && time.Since(e.ts) < quotaTTL
	inflight := r.quotaInflight[id]
	if !fresh && !inflight {
		r.quotaInflight[id] = true
		go r.refreshQuota(id, source, region)
	}
	r.quotaMu.Unlock()
	return e, ok
}

// refreshQuota fetches and caches one account's quota.
func (r *Runtime) refreshQuota(id, source, region string) {
	defer func() {
		r.quotaMu.Lock()
		r.quotaInflight[id] = false
		r.quotaMu.Unlock()
	}()
	token := r.tokenFor(id, source, region)
	if token == "" {
		return
	}
	q, err := account.FetchQuota(token, account.NormalizeRegion(region))
	if err != nil || q == nil {
		return
	}
	e := quotaEntry{plan: q.Plan, exceeded: q.IsQuotaExceeded, expiresAt: q.ExpiresAt, ts: time.Now()}
	if q.UserQuota != nil {
		e.remaining += q.UserQuota.Remaining
		e.total += q.UserQuota.Total
	}
	if q.AddonQuota != nil {
		e.remaining += q.AddonQuota.Remaining
		e.total += q.AddonQuota.Total
	}
	r.quotaMu.Lock()
	r.quotaCache[id] = e
	r.quotaMu.Unlock()
}

// --- provider.AccountManager (per-account actions, native accounts only) ---

// ActivateAccount sets the active ~/.qoder2api account (qoder's analog of
// workbuddy priority: which account serves requests).
func (r *Runtime) ActivateAccount(id string) error {
	if err := account.SetActive(id); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.bridges, id) // rebuild bridge on next use
	r.mu.Unlock()
	return nil
}

// RenameAccount sets a native account's display name.
func (r *Runtime) RenameAccount(id, name string) error {
	list, err := account.List()
	if err != nil {
		return err
	}
	for i := range list {
		if list[i].ID == id {
			list[i].Name = strings.TrimSpace(name)
			return account.Save(&list[i])
		}
	}
	return fmt.Errorf("账号不存在：%s", id)
}

// DeleteAccount removes a native account and its cached bridge.
func (r *Runtime) DeleteAccount(id string) error {
	r.mu.Lock()
	delete(r.bridges, id)
	r.mu.Unlock()
	return account.Delete(id)
}

// AdminCheckin runs the campaigns checkin for every usable qoder credential.
func (r *Runtime) AdminCheckin(ctx context.Context) (map[string]any, error) {
	results := []map[string]any{}
	if dataDirExists() {
		for _, res := range checkin.CheckinAll() {
			results = append(results, checkinRow(res))
		}
	}
	for _, c := range r.detectLocal() {
		id := "qoder-local-" + c.Region
		res := checkin.CheckinWithToken(id, "本地 Qoder（"+c.Region+"）", c.DeviceToken)
		results = append(results, checkinRow(res))
	}
	return map[string]any{"ok": true, "results": results}, nil
}

func checkinRow(res checkin.CheckinResult) map[string]any {
	return map[string]any{
		"account_id": res.AccountID, "account": res.Account,
		"ok": res.Succeeded(), "status": res.Status, "message": res.Message,
		"amount": res.Amount, "streak_days": res.StreakDays,
	}
}

// AdminRefreshCredits fetches the picked account's quota. Quota is queried with a
// device token, so PAT-only accounts (whose secret is not a dt-/JSON device
// token) report unsupported rather than erroring.
func (r *Runtime) AdminRefreshCredits(ctx context.Context) (map[string]any, error) {
	acct, secret, err := r.pickAccount()
	if err != nil {
		return nil, err
	}
	token := deviceTokenFromSecret(secret)
	if token == "" {
		return map[string]any{"ok": false, "account_id": acct.ID, "unsupported": true,
			"message": "该账号为 PAT，暂不支持额度查询（仅设备令牌账号支持）"}, nil
	}
	q, err := account.FetchQuota(token, acct.Region)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "account_id": acct.ID, "quota": q}, nil
}

// deviceTokenFromSecret extracts a dt-… device token from a raw or JSON secret.
func deviceTokenFromSecret(secret string) string {
	s := strings.TrimSpace(secret)
	if strings.HasPrefix(s, "{") {
		var j struct {
			DeviceToken string `json:"device_token"`
		}
		if json.Unmarshal([]byte(s), &j) == nil {
			return j.DeviceToken
		}
		return ""
	}
	if strings.HasPrefix(s, "dt-") {
		return s
	}
	return ""
}
