// Package models is the dynamic model-catalog service: it pulls available
// models from upstream per account and caches them. Ported from
// workbuddy_one/models.py.
package models

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"work2api/internal/workbuddy/httpclient"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/siterouting"
)

const (
	defaultTTL   = 3600
	failCooldown = 300
)

// modalityOverride forces the true modality of models the upstream mislabels.
var modalityOverride = map[string]bool{
	"hy4-preview": false, "hy3": false, "glm-5.3": false, "glm-5.2": false,
	"glm-5.1": false, "deepseek-v4-flash": false, "deepseek-v4-pro": false,
	"glm-5.3-flash": true, "glm-5v-turbo": true, "kimi-k3": true, "kimi-k3-1": true,
	"kimi-k2.7": true, "kimi-k2.6": true, "kimi-k2.5": true, "minimax-m3": true,
	"deepseek-v4.1-flash": true,
	// space-bunny 原生多模态（buddy-proxy 实测图片输入可用；id 必须全小写，
	// Space-Bunny/SPACE-BUNNY 均被上游拒为 11102）
	"space-bunny": true,
}

// Settings is the minimal settings source (for model_ttl_min).
type Settings interface {
	GetSettings() (map[string]string, error)
}

// catalogClient is the credential-manager capability models needs.
type catalogClient interface {
	CatalogHeaders() (map[string]string, error)
}

// catalogSource enumerates the per-profile catalog endpoints, richest first.
// 0.6.3 起上游双路拉取：官方客户端 /v3/config（国际版 CLI 白名单含
// deepseek-v4.1-flash 等，插件目录会漏）+ 区域插件目录。两路共享解析与
// 禁用标志，同 ID 先到先得（/v3/config 元数据优先），插件专属模型保留。
func catalogSources(profile string) ([]string, error) {
	ep, err := siterouting.EndpointForProfile(profile)
	if err != nil {
		return nil, err
	}
	v3 := ep + "/v3/config"
	legacy, err := siterouting.LegacyCatalogURLForProfile(profile)
	if err != nil || legacy == v3 {
		return []string{v3}, nil
	}
	return []string{v3, legacy}, nil
}

// Registry is the thread-safe dynamic model catalog.
type Registry struct {
	pool   *pool.Pool
	db     Settings
	client *http.Client

	refreshMu sync.Mutex
	mu        sync.Mutex
	models    []map[string]any
	reasoning map[string]map[string]any
	fetchedAt float64
	lastFail  float64
	source    string
	// catalogCache 按 (profile, uid, url) 保存各来源的成功快照：单一路失败
	// 不砍掉另一来源的专属模型（上游 0.6.3 _catalog_cache 同款）。
	catalogCache map[[3]string][]fetchedModel
}

// New builds a Registry over an account pool.
func New(p *pool.Pool, db Settings) *Registry {
	return NewWithCatalogClient(p, db, httpclient.New(20*time.Second))
}

// NewWithCatalogClient permits a caller-owned transport for catalog retrieval.
func NewWithCatalogClient(p *pool.Pool, db Settings, client *http.Client) *Registry {
	if client == nil {
		client = httpclient.New(20 * time.Second)
	}
	return &Registry{
		pool:         p,
		db:           db,
		client:       client,
		reasoning:    map[string]map[string]any{},
		catalogCache: map[[3]string][]fetchedModel{},
		source:       "static",
	}
}

func (r *Registry) ttl() int {
	if r.db != nil {
		if s, err := r.db.GetSettings(); err == nil {
			if m, err := strconv.Atoi(s["model_ttl_min"]); err == nil && m >= 1 && m <= 1440 {
				return m * 60
			}
		}
	}
	return defaultTTL
}

func nowSec() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// List returns current models (OpenAI /v1/models entries), refreshing if stale.
func (r *Registry) List() []map[string]any {
	now := nowSec()
	ttl := float64(r.ttl())
	r.mu.Lock()
	if len(r.models) > 0 && (now-r.fetchedAt) < ttl {
		out := withStandardAll(r.models)
		r.mu.Unlock()
		return out
	}
	inFailCooldown := r.lastFail != 0 && (now-r.lastFail) < failCooldown
	r.mu.Unlock()
	if inFailCooldown {
		return withStandardAll(r.fallback())
	}
	return r.Refresh()
}

// ListCached returns a read-only snapshot without triggering network refresh.
func (r *Registry) ListCached() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return withStandardAll(r.models)
}

// ClearCatalogSnapshots drops every per-source success snapshot (tests and
// administrative resets only): the next refresh republishes from live sources
// alone, without any fallback history.
func (r *Registry) ClearCatalogSnapshots() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.catalogCache = map[[3]string][]fetchedModel{}
}

// IDs returns the current model ids.
func (r *Registry) IDs() []string {
	list := r.List()
	out := make([]string, 0, len(list))
	for _, m := range list {
		out = append(out, str(m["id"]))
	}
	return out
}

// Source returns the current model source.
func (r *Registry) Source() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.source
}

// CreditsByRegion returns a model's per-site cost coefficients (e.g.
// {"domestic":0.03,"international":0}) from the cache; nil if unknown. A site
// absent from the map means the catalog gave no value there (not free).
func (r *Registry) CreditsByRegion(model string) map[string]float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.models {
		if str(m["id"]) == model {
			if cbr, ok := m["credits_by_region"].(map[string]float64); ok && len(cbr) > 0 {
				out := make(map[string]float64, len(cbr))
				for k, v := range cbr {
					out[k] = v
				}
				return out
			}
			return nil
		}
	}
	return nil
}

// Refresh force-refreshes the model cache.
func (r *Registry) Refresh() []map[string]any {
	return r.RefreshContext(context.Background())
}

func (r *Registry) RefreshContext(ctx context.Context) []map[string]any {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()
	fetched := r.fetchFromUpstreamContext(ctx)
	if ctx.Err() != nil {
		return withStandardAll(r.fallback())
	}
	if fetched == nil {
		r.mu.Lock()
		r.lastFail = nowSec()
		if len(r.models) > 0 {
			r.source = "dynamic"
		} else {
			r.source = "empty"
		}
		r.mu.Unlock()
		return r.fallback()
	}
	r.mu.Lock()
	r.models = nil
	r.reasoning = map[string]map[string]any{}
	for _, f := range fetched {
		r.models = append(r.models, f.entry)
		if f.reasoning != nil {
			r.reasoning[f.id] = f.reasoning
		}
	}
	r.fetchedAt = nowSec()
	r.lastFail = 0
	r.source = "dynamic"
	out := withStandardAll(r.models)
	r.mu.Unlock()
	log.Printf("模型列表已刷新: %d 个", len(fetched))
	return out
}

// ReasoningEfforts returns supported effort levels for a model (nil if unknown).
func (r *Registry) ReasoningEfforts(model string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reasoningEffortsLocked(model)
}

// EffortsTable returns the dynamic per-model effort table for reasoning
// downgrade (model → supported efforts).
func (r *Registry) EffortsTable() map[string][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string][]string{}
	for id := range r.reasoning {
		if eff := r.reasoningEffortsLocked(id); eff != nil {
			out[id] = eff
		}
	}
	return out
}

func (r *Registry) reasoningEffortsLocked(model string) []string {
	cfg := r.reasoning[model]
	if cfg == nil {
		return nil
	}
	var efforts []string
	if se, ok := cfg["supportedEfforts"].([]string); ok {
		efforts = append(efforts, se...)
	}
	if boolOf(cfg["onlyReasoning"]) {
		filtered := efforts[:0]
		for _, level := range efforts {
			normalized := strings.ToLower(strings.TrimSpace(level))
			if normalized != "off" && normalized != "none" {
				filtered = append(filtered, level)
			}
		}
		efforts = filtered
	}
	// A default rung is not a supported set. Unknown thinking levels must pass
	// through; off compatibility is handled separately for each request.
	if len(efforts) == 0 {
		return nil
	}
	// onlyReasoning models cannot disable thinking: never offer off even if the
	// catalog also claims canDisableThinking — the catalog field ranks below the
	// stronger onlyReasoning semantic constraint (measured on glm-5.2: both
	// regions accept "off" yet still emit reasoning, so the catalog's
	// onlyReasoning:false is not honored by the model server).
	if canDisable, _ := cfg["canDisableThinking"].(bool); canDisable && !boolOf(cfg["onlyReasoning"]) && !contains(efforts, "off") {
		efforts = append(efforts, "off")
	}
	if len(efforts) == 0 {
		return nil
	}
	return efforts
}

// RequestEfforts separates explicit support from default-rung off compatibility.
// A known catalog with unknown support returns a non-nil empty table so callers
// do not silently substitute the cold-start support table.
func (r *Registry) RequestEfforts(model, requested string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg, known := r.reasoning[model]
	if !known {
		return nil
	}
	if levels := r.reasoningEffortsLocked(model); levels != nil {
		return levels
	}
	requested = strings.ToLower(strings.TrimSpace(requested))
	if (requested == "off" || requested == "none") && boolOf(cfg["onlyReasoning"]) {
		for _, key := range []string{"defaultEffort", "effort"} {
			if level := str(cfg[key]); level != "" {
				return []string{level}
			}
		}
	}
	return []string{}
}

// MaxOutputTokens returns a model's max output token limit (0 if unknown).
func (r *Registry) MaxOutputTokens(model string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, m := range r.models {
		if str(m["id"]) == model {
			if n, ok := toInt(m["max_output_tokens"]); ok {
				return n
			}
		}
	}
	return 0
}

func (r *Registry) fallback() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return cloneAll(r.models)
}

type fetchedModel struct {
	id        string
	entry     map[string]any
	reasoning map[string]any
	// fresh 标记本路是否为本次真实观测（false = 成功快照回落）：
	// 合并时真实观测优先于快照，与跨账号的「真实观测可覆盖失败回落」同口径。
	fresh bool
}

func (r *Registry) fetchFromUpstream() []fetchedModel {
	return r.fetchFromUpstreamContext(context.Background())
}

func (r *Registry) fetchFromUpstreamContext(ctx context.Context) []fetchedModel {
	var accounts []*pool.Account
	for _, a := range r.pool.Accounts() {
		// 手动停用（含共享池「贡献验证中」）的账号不参与目录拉取：
		// 对齐上游 0.6.3，停用账号不应被一次目录请求唤醒/触发风控。
		if a.Enabled {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		if a := r.pool.Pick(nil, nil, 0); a != nil {
			accounts = []*pool.Account{a}
		}
	}
	if len(accounts) == 0 {
		return nil
	}
	merged := map[string]fetchedModel{}
	// Preserve only previously confirmed UID mappings on a failed refresh.
	// Never infer access from another account's site/profile.
	r.mu.Lock()
	previous := cloneAll(r.models)
	r.mu.Unlock()
	var order []string
	modelProfiles := map[string][]string{}
	modelAccounts := map[string][]string{}
	// 每模型每站点的成本系数（domestic/international）。合并是先到先得、会丢掉落败
	// 站点的 credits，这里单独按站点各记一份，让界面能如实显示"同模型两站点不同价"，
	// 也供成本优先选号用。nil（该站点没给值）不记，绝不当 0。
	creditsByRegion := map[string]map[string]float64{}
	anyOK := false
	fresh := map[string]bool{}
	freshCredits := map[string]bool{}
	for _, account := range accounts {
		if ctx.Err() != nil {
			return nil
		}
		got := r.fetchOneContext(ctx, account)
		observed := got != nil
		if got == nil {
			for _, old := range previous {
				uids, _ := old["account_uids"].([]string)
				if !contains(uids, account.UID) || str(old["id"]) == "auto" {
					continue
				}
				old = cloneModelEntry(old)
				region := siterouting.ProfileSite(account.Profile)
				delete(old, "credits")
				if costs, ok := old["credits_by_region"].(map[string]float64); ok {
					if cost, exists := costs[region]; exists {
						old["credits"] = cost
					}
				}
				reasoning, _ := old["reasoning"].(map[string]any)
				got = append(got, fetchedModel{id: str(old["id"]), entry: old, reasoning: reasoning})
			}
		} else {
			anyOK = true
		}
		region := siterouting.ProfileSite(account.Profile)
		for _, fm := range got {
			// Combine capability restrictions after selecting fresh metadata.
			previous, exists := merged[fm.id]
			locked := boolOf(fm.reasoning["onlyReasoning"]) || boolOf(previous.reasoning["onlyReasoning"])
			if !exists {
				merged[fm.id] = fm
				order = append(order, fm.id)
			} else if observed && fm.fresh && !fresh[fm.id] {
				merged[fm.id] = fm
			}
			chosen := merged[fm.id]
			if locked {
				chosen.reasoning["onlyReasoning"] = true
				chosen.entry["reasoning"] = chosen.reasoning
			}
			if observed && fm.fresh {
				fresh[fm.id] = true
			}
			if c, ok := fm.entry["credits"].(float64); ok {
				if creditsByRegion[fm.id] == nil {
					creditsByRegion[fm.id] = map[string]float64{}
				}
				costKey := fm.id + "\x00" + region
				if _, seen := creditsByRegion[fm.id][region]; !seen || observed && fm.fresh && !freshCredits[costKey] {
					creditsByRegion[fm.id][region] = c
				}
				if observed && fm.fresh {
					freshCredits[costKey] = true
				}
			}
			if !contains(modelProfiles[fm.id], account.Profile) {
				modelProfiles[fm.id] = append(modelProfiles[fm.id], account.Profile)
			}
			if !contains(modelAccounts[fm.id], account.UID) {
				modelAccounts[fm.id] = append(modelAccounts[fm.id], account.UID)
			}
		}
	}
	if !anyOK {
		return nil
	}
	out := []fetchedModel{}
	for _, id := range order {
		fm := merged[id]
		fm.entry["profiles"] = modelProfiles[id]
		fm.entry["account_uids"] = modelAccounts[id]
		if cbr := creditsByRegion[id]; len(cbr) > 0 {
			fm.entry["credits_by_region"] = cbr
		}
		out = append(out, fm)
	}
	// ensure "auto"
	wbProfiles := map[string]struct{}{}
	var wbUIDs []string
	for _, a := range accounts {
		if a.Provider == "workbuddy" {
			wbProfiles[a.Profile] = struct{}{}
			wbUIDs = append(wbUIDs, a.UID)
		}
	}
	hasAuto := false
	for _, fm := range out {
		if fm.id == "auto" {
			hasAuto = true
			fm.entry["profiles"] = sortedSet(wbProfiles)
			fm.entry["account_uids"] = wbUIDs
		}
	}
	if len(wbUIDs) > 0 && !hasAuto {
		autoEntry := entry("auto", "Auto", 0, 0)
		autoEntry["reasoning"] = map[string]any{"supportsReasoning": true, "onlyReasoning": true}
		autoEntry["modality"] = "text"
		autoEntry["profiles"] = sortedSet(wbProfiles)
		autoEntry["account_uids"] = wbUIDs
		out = append([]fetchedModel{{id: "auto", entry: autoEntry, reasoning: map[string]any{"supportsReasoning": true, "onlyReasoning": true}}}, out...)
	}
	return out
}

// RefreshAccount verifies only this account and merges its confirmed model IDs.
// Do not extend the TTL of other accounts or infer permissions from a profile.
func (r *Registry) RefreshAccount(ctx context.Context, uid string) bool {
	if !r.refreshMu.TryLock() {
		return false
	}
	defer r.refreshMu.Unlock()
	account := r.pool.Get(uid)
	if account == nil || !account.Enabled {
		return false
	}
	got := r.fetchOneContext(ctx, account)
	if got == nil {
		return false
	}
	if account.Provider == "workbuddy" {
		hasAuto := false
		for _, f := range got {
			if f.id == "auto" {
				hasAuto = true
			}
		}
		if !hasAuto {
			e := entry("auto", "Auto", 0, 0)
			e["reasoning"] = map[string]any{"supportsReasoning": true, "onlyReasoning": true}
			got = append(got, fetchedModel{id: "auto", entry: e, reasoning: e["reasoning"].(map[string]any)})
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	updated := []map[string]any{}
	byID := map[string]map[string]any{}
	for _, old := range cloneAll(r.models) {
		uids, _ := old["account_uids"].([]string)
		remaining := []string{}
		for _, id := range uids {
			if id != uid {
				remaining = append(remaining, id)
			}
		}
		if len(remaining) == 0 {
			delete(r.reasoning, str(old["id"]))
			continue
		}
		old["account_uids"] = remaining
		updated = append(updated, old)
		byID[str(old["id"])] = old
	}
	for _, f := range got {
		e := byID[f.id]
		if e == nil {
			e = f.entry
			e["account_uids"] = []string{}
			e["profiles"] = []string{}
			byID[f.id] = e
			updated = append(updated, e)
			if f.reasoning != nil {
				r.reasoning[f.id] = f.reasoning
			}
		}
		ids, _ := e["account_uids"].([]string)
		e["account_uids"] = append(ids, uid)
		profiles, _ := e["profiles"].([]string)
		if !contains(profiles, account.Profile) {
			e["profiles"] = append(profiles, account.Profile)
		}
		if cost, ok := f.entry["credits"].(float64); ok {
			oldCosts, _ := e["credits_by_region"].(map[string]float64)
			cbr := map[string]float64{}
			for region, value := range oldCosts {
				cbr[region] = value
			}
			if cbr == nil {
				cbr = map[string]float64{}
			}
			cbr[siterouting.ProfileSite(account.Profile)] = cost
			e["credits_by_region"] = cbr
		}
	}
	r.models = updated
	r.source = "dynamic"
	r.lastFail = 0
	return true
}

func (r *Registry) fetchOne(account *pool.Account) []fetchedModel {
	return r.fetchOneContext(context.Background(), account)
}

func (r *Registry) fetchOneContext(ctx context.Context, account *pool.Account) []fetchedModel {
	if account.Provider == "qoder" {
		return nil // qoder deferred
	}
	cc, ok := account.Mgr.(catalogClient)
	if !ok {
		return nil
	}
	headers, err := cc.CatalogHeaders()
	if err != nil {
		return nil
	}
	sources, err := catalogSources(account.Profile)
	if err != nil {
		return nil
	}
	merged := map[string]fetchedModel{}
	var order []string
	freshAny := false
	for _, url := range sources {
		fetched := r.fetchCatalogURL(ctx, url, headers, account)
		key := [3]string{account.Profile, account.UID, url}
		r.mu.Lock()
		if fetched != nil {
			r.catalogCache[key] = fetched
		}
		entries := fetched
		if entries == nil {
			// 单路失败回落成功快照。注意空目录（cli 白名单为空）是合法响应：
			// 它表示「该来源一个模型都不给」，必须覆盖旧快照（含清空），
			// 否则上游下架的模型会借快照永远留存（REVOKED 语义）。
			entries = r.catalogCache[key]
		}
		r.mu.Unlock()
		if fetched != nil {
			freshAny = true
		}
		for _, fm := range entries {
			fm.entry = cloneModelEntry(fm.entry)
			fm.reasoning, _ = fm.entry["reasoning"].(map[string]any)
			fm.fresh = fetched != nil
			previous, exists := merged[fm.id]
			if !exists {
				merged[fm.id] = fm
				order = append(order, fm.id)
			} else if fm.fresh && !previous.fresh {
				merged[fm.id] = fm
			}
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	if !freshAny { // 全部来源失败不能伪装成一次成功刷新：冷却账号、交给失败退避。
		if ctx.Err() == nil {
			r.pool.OnFailure(account.UID, 60)
		}
		return nil
	}
	out := make([]fetchedModel, 0, len(order))
	for _, id := range order {
		out = append(out, merged[id])
	}
	return out
}

// fetchCatalogURL 拉取单路目录并共享 CLI 白名单/禁用标志/元数据解析。
// 失败返回 nil（HTTP 错误、业务错误码、畸形响应、无 cli 白名单均算失败）。
func (r *Registry) fetchCatalogURL(ctx context.Context, url string, headers map[string]string, account *pool.Account) []fetchedModel {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := r.client.Do(req)
	if err != nil || resp == nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("models api status %d (%s)", resp.StatusCode, account.Profile)
		return nil
	}
	var data map[string]any
	if json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&data) != nil {
		return nil
	}
	// 业务错误码（上游按 code 字段报错时 HTTP 仍是 200）：不信任其中的模型清单。
	if code, hasCode := data["code"]; hasCode {
		switch c := code.(type) {
		case float64:
			if c != 0 {
				return nil
			}
		case string:
			if c != "0" {
				return nil
			}
		case nil:
		default:
			return nil
		}
	}
	d, _ := data["data"].(map[string]any)
	if d == nil {
		return nil
	}
	modelsRaw, _ := d["models"].([]any)
	agents, _ := d["agents"].([]any)
	var cliIDs []any
	cliFound := false
	for _, ag := range agents {
		if a, ok := ag.(map[string]any); ok && a["name"] == "cli" {
			cliIDs, cliFound = a["models"].([]any)
			break
		}
	}
	if !cliFound {
		return nil
	}
	dyn := map[string]map[string]any{}
	for _, m := range modelsRaw {
		if mm, ok := m.(map[string]any); ok {
			if id := str(mm["id"]); id != "" {
				dyn[id] = mm
			}
		}
	}
	out := []fetchedModel{}
	for _, midAny := range cliIDs {
		mid := str(midAny)
		m := dyn[mid]
		if m == nil {
			continue
		}
		if disabled, _ := m["disabled"].(bool); disabled {
			continue
		}
		e := entry(mid, str(m["name"]), intOf(m["maxInputTokens"]), intOf(m["maxOutputTokens"]))
		reasoning := extractReasoning(m)
		e["reasoning"] = reasoning
		for k, v := range extractCaps(m) {
			e[k] = v
		}
		e["profile"] = account.Profile
		out = append(out, fetchedModel{id: mid, entry: e, reasoning: reasoning})
	}
	// 空目录（cli 白名单存在但为空 / 全部被 disabled 过滤）是合法响应：
	// 「该来源一个模型都不给」，必须作为成功结果覆盖成功快照（含清空），
	// 与失败 nil 严格区分——否则上游下架的模型会借快照永远留存。
	return out
}

func entry(mid, name string, ctx, maxtok int) map[string]any {
	if name == "" {
		name = mid
	}
	return map[string]any{
		"id": mid, "object": "model", "created": 1700000000, "owned_by": "workbuddy",
		"name": name, "context_length": ctx, "max_output_tokens": maxtok,
	}
}

func withStandardFields(e map[string]any) map[string]any {
	multimodal := e["modality"] == "multimodal"
	if si, ok := e["supportsImages"].(bool); ok && si {
		multimodal = true
	}
	e["vision"] = multimodal
	e["image"] = multimodal
	e["supports_image"] = multimodal
	if multimodal {
		e["modalities"] = []string{"text", "image"}
		e["input_modalities"] = []string{"text", "image"}
	} else {
		e["modalities"] = []string{"text"}
		e["input_modalities"] = []string{"text"}
	}
	e["output_modalities"] = []string{"text"}
	return e
}

func withStandardAll(models []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		out = append(out, withStandardFields(cloneMap(m)))
	}
	return out
}

func extractReasoning(m map[string]any) map[string]any {
	cfg := map[string]any{
		"supportsReasoning": boolOf(m["supportsReasoning"]),
		"onlyReasoning":     boolOf(m["onlyReasoning"]),
	}
	if r, ok := m["reasoning"].(map[string]any); ok {
		if _, has := r["canDisableThinking"]; has {
			cfg["canDisableThinking"] = boolOf(r["canDisableThinking"])
		}
		if de, ok := r["defaultEffort"].(string); ok && de != "" {
			cfg["defaultEffort"] = de
		}
		if se, ok := r["supportedEfforts"].([]any); ok && len(se) > 0 {
			var list []string
			for _, x := range se {
				list = append(list, str(x))
			}
			cfg["supportedEfforts"] = list
		}
		// `effort`: the domestic catalog uses it instead of defaultEffort
		// (measured across both regions, 18 models carry it; it never co-occurs
		// with supportedEfforts). It records the DEFAULT rung the model runs at,
		// NOT the supported set — upstream does not validate reasoning_effort
		// values, so this is recorded for display/merge ranking only and must
		// never feed effort clamping.
		if _, hasDefault := cfg["defaultEffort"]; !hasDefault {
			if e, ok := r["effort"].(string); ok && e != "" {
				cfg["effort"] = e
			}
		}
	}
	return cfg
}

func extractCaps(m map[string]any) map[string]any {
	mid := str(m["id"])
	supportsImages := boolOf(m["supportsImages"])
	imgDisabled := boolOf(m["disabledMultimodal"])
	var modality string
	if forced, ok := modalityOverride[mid]; ok {
		supportsImages = forced
		if forced {
			modality = "multimodal"
		} else {
			modality = "text"
		}
	} else if supportsImages && !imgDisabled {
		modality = "multimodal"
	} else {
		modality = "text"
	}
	var credits any
	switch cr := m["credits"].(type) {
	case string:
		s := strings.TrimSpace(strings.ReplaceAll(cr, "x", ""))
		if s != "" {
			fields := strings.Fields(s)
			if len(fields) > 0 {
				if f, err := strconv.ParseFloat(fields[0], 64); err == nil {
					credits = f
				}
			}
		}
	case float64:
		credits = cr
	}
	desc := str(m["descriptionZh"])
	if desc == "" {
		desc = str(m["descriptionEn"])
	}
	// 上游 catalog 的 tags 里偶带营销标记（如 "badge:夜间折扣:#3B82F6"），
	// 原样透传给 WebUI 做价格时段提醒展示。
	var tags []any
	if t, ok := m["tags"].([]any); ok {
		tags = t
	}
	return map[string]any{
		"modality":         modality,
		"supportsToolCall": boolOf(m["supportsToolCall"]),
		"supportsImages":   supportsImages,
		"credits":          credits,
		"temperature":      m["temperature"],
		"top_p":            m["top_p"],
		"vendor":           m["vendor"],
		"description":      desc,
		"tags":             tags,
	}
}

// --- helpers ---

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func boolOf(v any) bool {
	b, _ := v.(bool)
	return b
}

func intOf(v any) int {
	if n, ok := toInt(v); ok {
		return n
	}
	return 0
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case string:
		n, err := strconv.Atoi(x)
		return n, err == nil
	}
	return 0, false
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func sortedSet(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func cloneAll(models []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		out = append(out, cloneModelEntry(m))
	}
	return out
}

// Entries cross refresh/publication boundaries; all mutable nested containers
// are copied, including capability metadata and per-region costs.
func cloneModelEntry(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneCatalogValue(v)
	}
	return out
}
func cloneCatalogValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneModelEntry(x)
	case map[string]float64:
		out := make(map[string]float64, len(x))
		for k, v := range x {
			out[k] = v
		}
		return out
	case []string:
		return append([]string(nil), x...)
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = cloneCatalogValue(v)
		}
		return out
	default:
		return v
	}
}
