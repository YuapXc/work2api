package opencode

import (
	"context"
	"time"

	"work2api/internal/core/provider"
)

// API keys remain configuration resources; Go OAuth adds one administrator-owned subscription.
var _ provider.AdminRuntime = (*Runtime)(nil)

// AdminData reports opencode's key tiers, models and status for the WebUI.
func (rt *Runtime) AdminData(ctx context.Context) provider.AdminData {
	d := provider.AdminData{
		DisplayName:  "OpenCode",
		ConfigEditor: "opencode-tiers",
		Ready:        rt.Ready(),
		Capabilities: []string{"models", "config"},
	}
	if !rt.Ready() {
		d.Notes = "未配置：在账号页点「编辑配置」填 Zen/Go 密钥或启用匿名层；也可设 OPENCODE_CONFIG 或放 data/opencode/opencode.json（含 zen_keys / go_keys / anonymous）"
		d.Status = map[string]any{"account_count": 0, "model_count": 0}
		return d
	}

	// "accounts" here are the authenticated key tiers + the anonymous lane,
	// counts only (keys themselves are never exposed).
	accts := []map[string]any{
		{"id": "zen", "label": "Zen 层", "tier": "zen", "key_count": rt.zenNodes.Len()},
		{"id": "go", "label": "Go 层", "tier": "go", "key_count": rt.goKeyCount()},
	}
	if rt.cfg.Anonymous {
		accts = append(accts, map[string]any{"id": "anonymous", "label": "匿名（public）", "tier": "anonymous", "key_count": 1})
	}
	now := time.Now().UnixNano()
	for _, row := range accts {
		if active, _ := rt.subscriptionSnapshot(); active && row["tier"] == "go" {
			row["label"], row["available_count"], row["total_count"] = "Go 手动 Key · 已暂停", 0, 0
			continue
		}
		available, total := 0, 0
		if row["tier"] == "anonymous" {
			for _, node := range rt.anonymous.nodes {
				total++
				if node.proxy.healthy.Load() && node.cooldownUntil.Load() <= now {
					available++
				}
			}
		} else {
			pool := rt.zenNodes
			if row["tier"] == "go" {
				pool = rt.goNodes
			}
			for _, node := range pool.nodes {
				total++
				if p := pool.Proxy(node); p != nil && p.healthy.Load() && node.cooldownUntil.Load() <= now {
					available++
				}
			}
		}
		row["available_count"], row["total_count"] = available, total
	}
	seen := map[string]bool{}
	keyRows := map[string]map[string]any{}
	for _, tier := range []struct {
		name string
		pool *nodePool
	}{{"zen", rt.zenNodes}, {"go", rt.goNodes}} {
		if tier.name == "go" && rt.goKeyCount() == 0 {
			continue
		}
		for _, node := range tier.pool.nodes {
			uid := PortalKeyUID(node.key)
			available := false
			if p := tier.pool.Proxy(node); p != nil {
				available = p.healthy.Load() && node.cooldownUntil.Load() <= now
			}
			if seen[uid] {
				if available {
					keyRows[uid]["healthy"] = true
					delete(keyRows[uid], "cooldown_until")
				}
				continue
			}
			seen[uid] = true
			row := map[string]any{"id": uid, "label": "OpenCode " + tier.name + " · " + uid[len(uid)-8:], "source": "native", "credential": true, "key_count": 1, "enabled": true, "healthy": available}
			if until := node.cooldownUntil.Load(); !available && until > now {
				row["cooldown_until"] = float64(until) / 1e9
			}
			keyRows[uid] = row
			accts = append(accts, row)
		}
	}
	if row := rt.subscriptionAccount(); row != nil {
		accts = append(accts, row)
	}
	d.Accounts = accts
	d.Resources = []provider.ResourceSummary{}
	for _, row := range accts {
		kind := "key_tier"
		if row["tier"] == "anonymous" {
			kind = "anonymous"
		}
		if row["credential"] == true {
			resourceKind := "api_key"
			if row["subscription"] == true {
				resourceKind = "subscription"
			}
			d.Resources = append(d.Resources, provider.ResourceMetadata(row, resourceKind, map[string]provider.ResourceAction{}))
			continue
		}
		d.Resources = append(d.Resources, provider.ResourceMetadata(row, kind, map[string]provider.ResourceAction{
			"config": {Label: "编辑配置", Enabled: true},
		}))
	}

	models := []map[string]any{}
	for _, m := range rt.Models(ctx) {
		row := map[string]any{"id": m.ID, "name": m.Name, "context": m.Context, "max_output": m.MaxOutput, "vision": m.Vision}
		for k, v := range m.Extra {
			row[k] = v
		}
		models = append(models, row)
	}
	d.Models = models

	subscriptionCount := 0
	if active, _ := rt.subscriptionSnapshot(); active {
		subscriptionCount = 1
	}

	d.Status = map[string]any{
		"account_count": rt.zenNodes.Len() + rt.goKeyCount() + subscriptionCount,
		"model_count":   len(models),
		"prefer":        string(rt.cfg.Prefer),
		"anonymous":     rt.cfg.Anonymous,
	}
	return d
}
