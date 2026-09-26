// Package models is the dynamic model-catalog service: it pulls available
// models from upstream per account and caches them. Ported from
// workbuddy_one/models.py.
package models

import (
	"context"
	"encoding/json"
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
}

// Settings is the minimal settings source (for model_ttl_min).
type Settings interface {
	GetSettings() (map[string]string, error)
}

// catalogClient is the credential-manager capability models needs.
type catalogClient interface {
	CatalogHeaders() (map[string]string, error)
}

// Registry is the thread-safe dynamic model catalog.
type Registry struct {
	pool   *pool.Pool
	db     Settings
	client *http.Client

	mu        sync.Mutex
	models    []map[string]any
	reasoning map[string]map[string]any
	fetchedAt float64
	lastFail  float64
	source    string
}

// New builds a Registry over an account pool.
func New(p *pool.Pool, db Settings) *Registry {
	return &Registry{
		pool:      p,
		db:        db,
		client:    httpclient.New(20 * time.Second), // no proxy (mirrors trust_env=False)
		reasoning: map[string]map[string]any{},
		source:    "static",
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
	fetched := r.fetchFromUpstream()
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
	cfg := r.reasoning[model]
	r.mu.Unlock()
	if cfg == nil {
		return nil
	}
	var efforts []string
	if se, ok := cfg["supportedEfforts"].([]string); ok {
		efforts = append(efforts, se...)
	}
	if len(efforts) == 0 {
		if de, ok := cfg["defaultEffort"].(string); ok && de != "" {
			efforts = []string{de}
		}
	}
	if canDisable, _ := cfg["canDisableThinking"].(bool); canDisable && !contains(efforts, "off") {
		efforts = append(efforts, "off")
	}
	if len(efforts) == 0 {
		return nil
	}
	return efforts
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
	if len(efforts) == 0 {
		if de, ok := cfg["defaultEffort"].(string); ok && de != "" {
			efforts = []string{de}
		}
	}
	if canDisable, _ := cfg["canDisableThinking"].(bool); canDisable && !contains(efforts, "off") {
		efforts = append(efforts, "off")
	}
	if len(efforts) == 0 {
		return nil
	}
	return efforts
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
}

func (r *Registry) fetchFromUpstream() []fetchedModel {
	var accounts []*pool.Account
	for _, a := range r.pool.Accounts() {
		if a.Enabled {
			accounts = append(accounts, a)
		}
	}
	if len(accounts) == 0 {
		if a := r.pool.Pick(nil, nil); a != nil {
			accounts = []*pool.Account{a}
		}
	}
	if len(accounts) == 0 {
		return nil
	}
	merged := map[string]fetchedModel{}
	var order []string
	modelProfiles := map[string][]string{}
	modelAccounts := map[string][]string{}
	// 每模型每站点的成本系数（domestic/international）。合并是先到先得、会丢掉落败
	// 站点的 credits，这里单独按站点各记一份，让界面能如实显示"同模型两站点不同价"，
	// 也供成本优先选号用。nil（该站点没给值）不记，绝不当 0。
	creditsByRegion := map[string]map[string]float64{}
	anyOK := false
	for _, account := range accounts {
		got := r.fetchOne(account)
		if got == nil {
			continue
		}
		anyOK = true
		region := siterouting.ProfileSite(account.Profile)
		for _, fm := range got {
			if _, exists := merged[fm.id]; !exists {
				merged[fm.id] = fm
				order = append(order, fm.id)
			}
			if c, ok := fm.entry["credits"].(float64); ok {
				if creditsByRegion[fm.id] == nil {
					creditsByRegion[fm.id] = map[string]float64{}
				}
				if _, seen := creditsByRegion[fm.id][region]; !seen {
					creditsByRegion[fm.id][region] = c
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
	if !anyOK || len(order) == 0 {
		return nil
	}
	var out []fetchedModel
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

func (r *Registry) fetchOne(account *pool.Account) []fetchedModel {
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
	url, err := siterouting.CatalogURLForProfile(account.Profile)
	if err != nil {
		return nil
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		r.pool.OnFailure(account.UID, 60)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Printf("models api status %d (%s)", resp.StatusCode, account.Profile)
		r.pool.OnFailure(account.UID, 60)
		return nil
	}
	var data map[string]any
	if json.NewDecoder(resp.Body).Decode(&data) != nil {
		return nil
	}
	d, _ := data["data"].(map[string]any)
	if d == nil {
		return nil
	}
	modelsRaw, _ := d["models"].([]any)
	agents, _ := d["agents"].([]any)
	var cliIDs []any
	for _, ag := range agents {
		if a, ok := ag.(map[string]any); ok && a["name"] == "cli" {
			cliIDs, _ = a["models"].([]any)
			break
		}
	}
	if len(cliIDs) == 0 {
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
	var out []fetchedModel
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
	if len(out) == 0 {
		return nil
	}
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
	return map[string]any{
		"modality":         modality,
		"supportsToolCall": boolOf(m["supportsToolCall"]),
		"supportsImages":   supportsImages,
		"credits":          credits,
		"temperature":      m["temperature"],
		"top_p":            m["top_p"],
		"vendor":           m["vendor"],
		"description":      desc,
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
		out = append(out, cloneMap(m))
	}
	return out
}
