package trae

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"work2api/internal/core/provider"
	"work2api/internal/statebackup"
	"work2api/internal/trae/auth"
	"work2api/internal/trae/upstream"
)

type catalogSnapshot struct {
	Version   int                  `json:"version"`
	Account   string               `json:"account"`
	Product   string               `json:"product"`
	FetchedAt time.Time            `json:"fetched_at"`
	Models    []upstream.ModelInfo `json:"models"`
}

func (r *Runtime) catalogPath(id, product string) string {
	return filepath.Join(r.dir, ".catalog-"+id+"-"+product+".json")
}

func (r *Runtime) loadCatalog(id, product string, a *accountState) {
	f, err := os.Open(r.catalogPath(id, product))
	if err != nil {
		return
	}
	defer f.Close()
	var snapshot catalogSnapshot
	if json.NewDecoder(io.LimitReader(f, 8<<20)).Decode(&snapshot) != nil || snapshot.Version != 1 || snapshot.Account != id || snapshot.Product != product || snapshot.FetchedAt.IsZero() || snapshot.FetchedAt.After(time.Now().Add(5*time.Minute)) {
		return
	}
	for _, model := range snapshot.Models {
		if model.ID == "" || model.Name == "" || model.ContextWindow < 0 || model.MaxTokens < 0 {
			return
		}
	}
	a.Catalogs[product] = snapshot.Models
	a.CatalogTimes[product] = snapshot.FetchedAt
	if snapshot.FetchedAt.After(a.CatalogAt) {
		a.CatalogAt = snapshot.FetchedAt
	}
}

// The caller holds writeMu and the backup writer gate. Separate snapshots keep
// token refresh from overwriting discovery data; deletion removes both files.
func (r *Runtime) writeCatalog(id, product string, models []upstream.ModelInfo, at time.Time) error {
	raw, err := json.Marshal(catalogSnapshot{Version: 1, Account: id, Product: product, FetchedAt: at, Models: models})
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(r.dir, ".catalog-write-*")
	if err != nil {
		return errors.New("Trae 模型目录快照保存失败")
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("Trae 模型目录快照保存失败")
	}
	if os.Rename(f.Name(), r.catalogPath(id, product)) != nil {
		return errors.New("Trae 模型目录快照保存失败")
	}
	return nil
}

func (r *Runtime) commitCatalog(id, product string, identity *auth.Auth, models []upstream.ModelInfo) error {
	r.writeMu.Lock()
	defer r.writeMu.Unlock()
	leave := statebackup.Enter()
	defer leave()
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.accounts[id]
	if a == nil || a.Identities[product] != identity {
		return nil
	}
	at := time.Now()
	err := r.writeCatalog(id, product, models, at)
	a.Catalogs[product] = models
	if a.CatalogTimes == nil {
		a.CatalogTimes = map[string]time.Time{}
	}
	if a.CatalogErrors == nil {
		a.CatalogErrors = map[string]bool{}
	}
	a.CatalogTimes[product], a.CatalogErrors[product] = at, err != nil
	a.CatalogAt = at
	a.mergeModels()
	return err
}

func (r *Runtime) markCatalogFailure(id, product string, a *accountState) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.accounts[id] == a {
		if a.CatalogErrors == nil {
			a.CatalogErrors = map[string]bool{}
		}
		a.CatalogErrors[product] = true
	}
}

func catalogStale(a *accountState) bool {
	for product := range a.Identities {
		if a.CatalogErrors[product] || time.Since(a.CatalogTimes[product]) > 30*time.Minute {
			return true
		}
	}
	return false
}

// A merged model advertises image support when at least one eligible identity
// declares it. Dispatch prefers that identity for images. Unknown beats false
// when no identity declares support; missing metadata is never text-only proof.
func unionBool(a, b *bool) *bool {
	if a != nil && *a || b != nil && *b {
		yes := true
		return &yes
	}
	if a == nil || b == nil {
		return nil
	}
	no := false
	return &no
}

func mergeModelInfo(a, b upstream.ModelInfo) upstream.ModelInfo {
	a.Pricing = append(append([]upstream.ModelPrice(nil), a.Pricing...), b.Pricing...)
	a.EntitlementCosts = append(append([]float64(nil), a.EntitlementCosts...), b.EntitlementCosts...)
	a.Multimodal = unionBool(a.Multimodal, b.Multimodal)
	a.ToolResponseMultimodal = unionBool(a.ToolResponseMultimodal, b.ToolResponseMultimodal)
	a.ThinkingControl = unionBool(a.ThinkingControl, b.ThinkingControl)
	a.ReasoningOptions = slices.Clone(a.ReasoningOptions)
	for _, option := range b.ReasoningOptions {
		if !slices.Contains(a.ReasoningOptions, option) {
			a.ReasoningOptions = append(a.ReasoningOptions, option)
		}
	}
	if a.DefaultReasoning != b.DefaultReasoning {
		a.DefaultReasoning = ""
	}
	if b.Capability == "reasoning_model" || a.Capability == "" {
		a.Capability = b.Capability
	}
	if b.ContextWindow > 0 && (a.ContextWindow == 0 || b.ContextWindow < a.ContextWindow) {
		a.ContextWindow = b.ContextWindow
	}
	if b.MaxTokens > 0 && (a.MaxTokens == 0 || b.MaxTokens < a.MaxTokens) {
		a.MaxTokens = b.MaxTokens
	}
	return a
}

func boolValue(v *bool) any {
	if v == nil {
		return nil
	}
	return *v
}
func applyModelCapabilities(entry *provider.CatalogModel, m upstream.ModelInfo) {
	for key, value := range pricingMetadata(m.Pricing) {
		entry.Extra[key] = value
	}
	entry.Extra["entitlement_usage_cost"] = m.EntitlementCosts
	entry.Vision = m.Multimodal != nil && *m.Multimodal
	entry.Extra["vision"] = boolValue(m.Multimodal)
	entry.Extra["vision_verified"] = m.Multimodal != nil
	entry.Extra["capabilities_verified"] = m.Multimodal != nil
	entry.Extra["capability_source"] = "upstream_catalog"
	entry.Extra["model_capability"] = m.Capability
	entry.Extra["thinking_control"] = boolValue(m.ThinkingControl)
	entry.Extra["tool_response_multimodal"] = boolValue(m.ToolResponseMultimodal)
	entry.Extra["reasoning_options"] = slices.Clone(m.ReasoningOptions)
	entry.Extra["reasoning_default"] = m.DefaultReasoning
	entry.Extra["reasoning"] = nil
	if m.Capability == "reasoning_model" || m.Capability == "chat_model" || len(m.ReasoningOptions) > 0 {
		reasoning := map[string]any{"supportsReasoning": m.Capability == "reasoning_model" || len(m.ReasoningOptions) > 0}
		if efforts := m.ReasoningEfforts(); len(efforts) > 0 {
			reasoning["supportedEfforts"] = efforts
		}
		if native, ok := m.NativeEffort(m.DefaultReasoning); ok {
			reasoning["defaultEffort"] = upstream.NativeEffortName(native)
		}
		entry.Extra["reasoning"] = reasoning
	}
	entry.Modalities = nil
	if m.Multimodal != nil {
		entry.Modalities = []string{"text"}
		if *m.Multimodal {
			entry.Modalities = append(entry.Modalities, "image")
		}
	}
	entry.Extra["input_modalities"] = entry.Modalities
}

func mergeCatalogCapabilities(entry *provider.CatalogModel, m upstream.ModelInfo) {
	var vision, thinking, tool *bool
	if value, ok := entry.Extra["vision"].(bool); ok {
		vision = &value
	}
	if value, ok := entry.Extra["thinking_control"].(bool); ok {
		thinking = &value
	}
	if value, ok := entry.Extra["tool_response_multimodal"].(bool); ok {
		tool = &value
	}
	previous := upstream.ModelInfo{Multimodal: vision, ThinkingControl: thinking, ToolResponseMultimodal: tool, Capability: text(entry.Extra["model_capability"]), ContextWindow: int64(entry.Context), MaxTokens: int64(entry.MaxOutput)}
	previous.ReasoningOptions, _ = entry.Extra["reasoning_options"].([]string)
	previous.DefaultReasoning, _ = entry.Extra["reasoning_default"].(string)
	previous.EntitlementCosts, _ = entry.Extra["entitlement_usage_cost"].([]float64)
	previous.Pricing, _ = entry.Extra["price_factors"].([]upstream.ModelPrice)
	merged := mergeModelInfo(previous, m)
	entry.Context, entry.MaxOutput = int(merged.ContextWindow), int(merged.MaxTokens)
	applyModelCapabilities(entry, merged)
}

func hasImageInput(body map[string]any) bool {
	msgs, _ := body["messages"].([]any)
	for _, msg := range msgs {
		message, _ := msg.(map[string]any)
		content, _ := message["content"].([]any)
		for _, block := range content {
			if b, ok := block.(map[string]any); ok && (b["type"] == "image_url" || b["type"] == "image" || b["type"] == "input_image") {
				return true
			}
		}
	}
	return false
}

// Successful activity extends affinity, bounded by idle TTL and LRU eviction.
func (r *Runtime) rememberAffinity(session, account string) {
	if r.affinities == nil {
		r.affinities = map[string]string{}
	}
	if r.affinityUsed == nil {
		r.affinityUsed = map[string]time.Time{}
	}
	if r.affinityOrder == nil {
		r.affinityOrder = map[string]uint64{}
	}
	now := time.Now()
	for id, at := range r.affinityUsed {
		if now.Sub(at) > 24*time.Hour {
			delete(r.affinities, id)
			delete(r.affinityUsed, id)
			delete(r.affinityOrder, id)
		}
	}
	if _, existing := r.affinities[session]; !existing && len(r.affinities) >= 256 {
		oldest := ""
		for id := range r.affinities {
			if oldest == "" || r.affinityOrder[id] < r.affinityOrder[oldest] {
				oldest = id
			}
		}
		delete(r.affinities, oldest)
		delete(r.affinityUsed, oldest)
		delete(r.affinityOrder, oldest)
	}
	r.affinities[session], r.affinityUsed[session] = account, now
	r.affinityClock++
	r.affinityOrder[session] = r.affinityClock
}
