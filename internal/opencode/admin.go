package opencode

import (
	"context"

	"work2api/internal/core/provider"
)

// opencode exposes read-only admin data: it has no persisted accounts (its
// "accounts" are config key tiers) and no checkin/credits. Management = editing
// the config file, surfaced as the "config" capability + a Notes hint.
var _ provider.AdminRuntime = (*Runtime)(nil)

// AdminData reports opencode's key tiers, models and status for the WebUI.
func (rt *Runtime) AdminData(ctx context.Context) provider.AdminData {
	d := provider.AdminData{
		DisplayName:  "OpenCode",
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
		{"id": "go", "label": "Go 层", "tier": "go", "key_count": rt.goNodes.Len()},
	}
	if rt.cfg.Anonymous {
		accts = append(accts, map[string]any{"id": "anonymous", "label": "匿名（public）", "tier": "anonymous", "key_count": 1})
	}
	d.Accounts = accts

	models := []map[string]any{}
	for _, m := range rt.Models(ctx) {
		row := map[string]any{"id": m.ID, "name": m.Name, "context": m.Context, "max_output": m.MaxOutput, "vision": m.Vision}
		for k, v := range m.Extra {
			row[k] = v
		}
		models = append(models, row)
	}
	d.Models = models

	d.Status = map[string]any{
		"account_count": rt.zenNodes.Len() + rt.goNodes.Len(),
		"model_count":   len(models),
		"prefer":        string(rt.cfg.Prefer),
		"anonymous":     rt.cfg.Anonymous,
	}
	return d
}
