package opencode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"work2api/internal/core/provider"
)

func PortalKeyUID(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "opencode:" + hex.EncodeToString(sum[:])
}
func (rt *Runtime) SupportsPortal() bool { return true }

func (rt *Runtime) PortalModels(_ context.Context, models map[string]bool, discoverFree bool, _ func(string) bool) []provider.PortalModel {
	var out []provider.PortalModel
	for _, m := range rt.catalogModels(models) {
		uids, _ := m.Extra["account_uids"].([]string)
		out = append(out, provider.NewPortalModel(m.ID, uids, discoverFree && m.Extra["upstream_free"] == true))
	}
	return out
}

func (rt *Runtime) routeAccountUIDs(route Route) []string {
	ids := []string{}
	if route.Anonymous {
		ids = append(ids, "opencode:anonymous")
	}
	tiers := route.KeyTiers
	if len(tiers) == 0 && !route.Anonymous {
		tiers = []Tier{route.Tier}
	}
	for _, tier := range tiers {
		p := rt.zenNodes
		if tier == TierGo {
			p = rt.goNodes
		}
		if p == nil {
			continue
		}
		for _, n := range p.nodes {
			if rt.keySupportsModel(PortalKeyUID(n.key), route.ID) {
				ids = append(ids, PortalKeyUID(n.key))
			}
		}
	}
	return ids
}

// Validate credentials without making a billed model call, then let the core
// reserve ownership before persisting them. Config writes remain serialized.
func (rt *Runtime) AddAccountByToken(ctx context.Context, token string, opts map[string]any) (map[string]any, error) {
	token = strings.TrimSpace(token)
	tier, _ := opts["tier"].(string)
	if token == "" || token == "public" || (tier != "zen" && tier != "go") {
		return nil, fmt.Errorf("请选择 Zen/Go 并填写个人 API Key，匿名入口不能贡献")
	}
	cfg := rt.baseConfig()
	base := cfg.Upstream.Zen
	if tier == "go" {
		base = cfg.Upstream.Go
	}
	transports, err := newTransportPool(cfg.RuntimeProxies(), cfg.Performance, 0)
	if err != nil {
		return nil, err
	}
	if len(transports.items) == 0 {
		return nil, fmt.Errorf("没有可用传输")
	}
	defer func() {
		for _, item := range transports.items {
			item.client.CloseIdleConnections()
		}
	}()
	models, _, err := FetchModels(ctx, transports.items[0].client, base, token)
	if err != nil {
		return nil, fmt.Errorf("验证 OpenCode Key 失败：%w", err)
	}
	id := PortalKeyUID(token)
	if claim, ok := opts["claim"].(func(string) error); ok {
		if err := claim(id); err != nil {
			return nil, err
		}
	}
	keys := cfg.ZenKeys
	field := "zen_keys"
	if tier == "go" {
		keys = cfg.GoKeys
		field = "go_keys"
	}
	found := false
	for _, key := range keys {
		found = found || key == token
	}
	if !found {
		keys = append(keys, token)
	}
	keyModels := map[string][]string{}
	for uid, list := range cfg.KeyModels {
		keyModels[uid] = append([]string{}, list...)
	}
	keyModels[id] = append([]string{}, models...)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := rt.SaveConfigDoc(map[string]any{field: keys, "key_models": keyModels}); err != nil {
		return nil, err
	}
	// Refresh discovery on the replacement rather than the stopped instance.
	if reg := rt.registry.Load(); reg != nil {
		if current, ok := reg.ByName(runtimeName); ok {
			if live, ok := current.(*Runtime); ok {
				_ = live.RefreshModels(ctx)
			}
		}
	}
	return map[string]any{"id": id, "label": "OpenCode " + tier, "tier": tier, "model_count": len(models)}, nil
}

// Imported credentials use their validated catalog; legacy administrator keys
// retain the original tier catalog behavior until independently refreshed.
func (rt *Runtime) keySupportsModel(uid, model string) bool {
	rt.keyModelsMu.RLock()
	models, known := rt.keyModels[uid]
	if !known {
		models, known = rt.cfg.KeyModels[uid]
	}
	rt.keyModelsMu.RUnlock()
	if !known {
		return true
	}
	for _, m := range models {
		if m == model {
			return true
		}
	}
	return false
}

func (rt *Runtime) HasPortalKey(uid string) bool {
	cfg := rt.baseConfig()
	for _, key := range append(append([]string{}, cfg.ZenKeys...), cfg.GoKeys...) {
		if PortalKeyUID(key) == uid {
			return true
		}
	}
	return false
}
