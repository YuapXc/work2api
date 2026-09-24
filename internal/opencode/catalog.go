package opencode

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"work2api/internal/core/protocol"
)

// Metadata carries per-model capability fields surfaced through the catalog.
type Metadata struct {
	ContextWindow    int      `json:"context_window,omitempty"`
	MaxInput         int      `json:"max_input,omitempty"`
	MaxOutput        int      `json:"max_output,omitempty"`
	Reasoning        bool     `json:"reasoning,omitempty"`
	ToolCall         bool     `json:"tool_call,omitempty"`
	StructuredOutput bool     `json:"structured_output,omitempty"`
	InputModalities  []string `json:"input_modalities,omitempty"`
	OutputModalities []string `json:"output_modalities,omitempty"`
}

// Route describes how a model reaches an upstream, per tier.
type Route struct {
	ID        string
	Tier      Tier
	Protocol  protocol.Protocol
	Protocols map[Tier]protocol.Protocol
	Anonymous bool
	KeyTiers  []Tier
}

func (r Route) ProtocolFor(tier Tier) protocol.Protocol {
	if p := r.Protocols[tier]; p != "" {
		return p
	}
	return r.Protocol
}

type Catalog struct {
	mu              sync.RWMutex
	zen             map[string]bool
	goModels        map[string]bool
	protocols       map[string]protocol.Protocol
	nativeProtocols map[Tier]map[string]protocol.Protocol
	unsupported     map[Tier]map[string]bool
	modelMeta       map[Tier]map[string]Metadata
	updatedAt       time.Time
	prefer          Tier
	pricing         *PricingStore
	cachePath       string
	cacheSource     string
	stale           bool
	refreshAfter    time.Duration
}

type CatalogSnapshot struct {
	Zen         int       `json:"zen"`
	Go          int       `json:"go"`
	Total       int       `json:"total"`
	Exposed     int       `json:"exposed"`
	UpdatedAt   time.Time `json:"updated_at,omitempty"`
	CacheSource string    `json:"cache_source,omitempty"`
	Stale       bool      `json:"stale"`
}

func NewCatalog(prefer Tier, overrides map[string]string) *Catalog {
	protocols := make(map[string]protocol.Protocol, len(overrides))
	for model, p := range overrides {
		protocols[model] = protocol.Protocol(p)
	}
	return &Catalog{
		zen: map[string]bool{}, goModels: map[string]bool{}, protocols: protocols,
		nativeProtocols: map[Tier]map[string]protocol.Protocol{TierZen: {}, TierGo: {}},
		unsupported:     map[Tier]map[string]bool{TierZen: {}, TierGo: {}}, prefer: prefer,
		cacheSource: "none",
	}
}

func (c *Catalog) SetPricingStore(store *PricingStore) {
	c.mu.Lock()
	c.pricing = store
	c.mu.Unlock()
}

func (c *Catalog) SetCachePath(path string) {
	c.mu.Lock()
	c.cachePath = path
	c.mu.Unlock()
}

// CopyState transfers the in-memory catalog (model sets, per-tier native
// protocols, unsupported set, metadata, freshness) from source into c. Used on
// config hot-reload so the new runtime inherits the live catalog instead of
// starting from the on-disk cache — which, if missing/stale, would make every
// model look available on any tier and default to the Chat protocol until the
// first async refresh lands (mirrors upstream RuntimeManager.Apply CopyState).
func (c *Catalog) CopyState(source *Catalog) {
	if source == nil {
		return
	}
	source.mu.RLock()
	zen := cloneBools(source.zen)
	goModels := cloneBools(source.goModels)
	native := map[Tier]map[string]protocol.Protocol{TierZen: {}, TierGo: {}}
	unsupported := map[Tier]map[string]bool{TierZen: {}, TierGo: {}}
	for _, tier := range []Tier{TierZen, TierGo} {
		native[tier] = cloneProtocols(source.nativeProtocols[tier])
		unsupported[tier] = cloneBools(source.unsupported[tier])
	}
	meta := cloneModelMeta(source.modelMeta)
	updatedAt := source.updatedAt
	cacheSource := source.cacheSource
	stale := source.stale
	source.mu.RUnlock()

	c.mu.Lock()
	c.zen, c.goModels, c.nativeProtocols, c.unsupported = zen, goModels, native, unsupported
	c.modelMeta, c.updatedAt, c.cacheSource, c.stale = meta, updatedAt, cacheSource, stale
	c.mu.Unlock()
}

func (c *Catalog) SetRefreshInterval(interval time.Duration) {
	c.mu.Lock()
	c.refreshAfter = interval
	c.mu.Unlock()
}

func (c *Catalog) ReplaceWithCapabilities(zen, goModels []string, native map[Tier]map[string]protocol.Protocol, unsupported map[Tier]map[string]bool, metadata map[Tier]map[string]Metadata) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if zen != nil {
		c.zen = toSet(zen)
	}
	if goModels != nil {
		c.goModels = toSet(goModels)
	}
	if native != nil {
		for _, tier := range []Tier{TierZen, TierGo} {
			if protocols, ok := native[tier]; ok {
				c.nativeProtocols[tier] = cloneProtocols(protocols)
			}
		}
	}
	if unsupported != nil {
		for _, tier := range []Tier{TierZen, TierGo} {
			if models, ok := unsupported[tier]; ok {
				c.unsupported[tier] = cloneBools(models)
			}
		}
	}
	if metadata != nil {
		c.modelMeta = cloneModelMeta(metadata)
	}
	c.updatedAt = time.Now().UTC()
	c.cacheSource = "live"
	c.stale = false
}

func (c *Catalog) Route(model string, hasZenKeys, hasGoKeys, hasAnonymous bool) (Route, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.routeLocked(model, hasZenKeys, hasGoKeys, hasAnonymous)
}

func (c *Catalog) routeLocked(model string, hasZenKeys, hasGoKeys, hasAnonymous bool) (Route, error) {
	keyTiers := c.keyTierOrderLocked(model, hasZenKeys, hasGoKeys)
	decision := c.anonymousDecision(model)
	if hasAnonymous && decision.Allowed && (c.protocols[model] != "" || !c.unsupported[TierZen][model]) &&
		(len(c.zen) == 0 && len(c.goModels) == 0 || c.zen[model] || c.goModels[model]) {
		protocols := c.protocolsForLocked(model, keyTiers, true)
		return Route{ID: model, Tier: TierZen, Protocol: protocols[TierZen], Protocols: protocols, Anonymous: true, KeyTiers: keyTiers}, nil
	}
	if len(keyTiers) > 0 {
		protocols := c.protocolsForLocked(model, keyTiers, false)
		return Route{ID: model, Tier: keyTiers[0], Protocol: protocols[keyTiers[0]], Protocols: protocols, KeyTiers: keyTiers}, nil
	}
	return Route{}, fmt.Errorf("model %q is not available in the configured Zen or Go pools", model)
}

func (c *Catalog) protocolsForLocked(model string, keyTiers []Tier, includeZen bool) map[Tier]protocol.Protocol {
	protocols := make(map[Tier]protocol.Protocol, len(keyTiers)+1)
	if includeZen {
		protocols[TierZen] = c.protocolForLocked(model, TierZen)
	}
	for _, tier := range keyTiers {
		protocols[tier] = c.protocolForLocked(model, tier)
	}
	return protocols
}

func (c *Catalog) protocolForLocked(model string, tier Tier) protocol.Protocol {
	if p := c.protocols[model]; p != "" {
		return p
	}
	if p := c.nativeProtocols[tier][model]; p != "" {
		return p
	}
	return protocol.Chat
}

func (c *Catalog) keyTierOrderLocked(model string, hasZenKeys, hasGoKeys bool) []Tier {
	catalogPending := len(c.zen) == 0 && len(c.goModels) == 0
	available := func(tier Tier) bool {
		switch tier {
		case TierZen:
			return hasZenKeys && (catalogPending || c.zen[model]) && c.tierSupportedLocked(model, TierZen)
		case TierGo:
			return hasGoKeys && (catalogPending || c.goModels[model]) && c.tierSupportedLocked(model, TierGo)
		default:
			return false
		}
	}
	order := []Tier{TierZen, TierGo}
	if c.prefer == TierGo {
		order[0], order[1] = order[1], order[0]
	}
	result := make([]Tier, 0, len(order))
	for _, tier := range order {
		if available(tier) {
			result = append(result, tier)
		}
	}
	return result
}

func (c *Catalog) anonymousDecision(model string) AnonymousDecision {
	if c.pricing != nil {
		return c.pricing.Decide(model)
	}
	return AnonymousDecision{Allowed: isFreeModel(model), Source: "name_fallback_metadata_pending"}
}

// IsFreeModel reports whether the catalog considers the model free-tier.
func (c *Catalog) IsFreeModel(model string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.anonymousDecision(model).Allowed
}

func isFreeModel(model string) bool {
	return strings.Contains(strings.ToLower(model), "free")
}

func (c *Catalog) List() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ids := c.modelIDsLocked()
	models := ids[:0]
	for _, model := range ids {
		if c.supportedLocked(model) {
			models = append(models, model)
		}
	}
	return models
}

func (c *Catalog) modelIDsLocked() []string {
	seen := make(map[string]bool, len(c.zen)+len(c.goModels))
	for model := range c.zen {
		seen[model] = true
	}
	for model := range c.goModels {
		seen[model] = true
	}
	return sortedSetKeys(seen)
}

// AvailableModels provides discovery with the same route filtering.
func (c *Catalog) AvailableModels(hasZenKeys, hasGoKeys, hasAnonymous bool) ([]Route, CatalogSnapshot) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	ids := c.modelIDsLocked()
	routes := make([]Route, 0, len(ids))
	for _, model := range ids {
		if !c.supportedLocked(model) {
			continue
		}
		if route, err := c.routeLocked(model, hasZenKeys, hasGoKeys, hasAnonymous); err == nil {
			routes = append(routes, route)
		}
	}
	snapshot := c.snapshotLocked(ids)
	snapshot.Exposed = len(routes)
	return routes, snapshot
}

func (c *Catalog) snapshotLocked(ids []string) CatalogSnapshot {
	exposed := 0
	for _, model := range ids {
		if c.supportedLocked(model) {
			exposed++
		}
	}
	stale := c.stale
	if !c.updatedAt.IsZero() && c.refreshAfter > 0 {
		stale = stale || time.Since(c.updatedAt) > max(2*c.refreshAfter, time.Minute)
	}
	return CatalogSnapshot{
		Zen: len(c.zen), Go: len(c.goModels), Total: len(ids), Exposed: exposed,
		UpdatedAt: c.updatedAt, CacheSource: c.cacheSource, Stale: stale,
	}
}

func (c *Catalog) Supported(model string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.supportedLocked(model)
}

// MetadataForTier returns the per-model metadata for the tier that will serve
// the request (anonymous routes always resolve to TierZen).
func (c *Catalog) MetadataForTier(model string, tier Tier) Metadata {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.modelMeta[tier][model]
}

func (c *Catalog) supportedLocked(model string) bool {
	if len(c.zen) == 0 && len(c.goModels) == 0 {
		return true
	}
	if c.zen[model] && c.tierSupportedLocked(model, TierZen) {
		return true
	}
	if c.goModels[model] && c.tierSupportedLocked(model, TierGo) {
		return true
	}
	return false
}

func (c *Catalog) tierSupportedLocked(model string, tier Tier) bool {
	if c.protocols[model] != "" {
		return true
	}
	if c.unsupported[tier][model] {
		return false
	}
	if c.nativeProtocols[tier][model] != "" {
		return true
	}
	return len(c.zen) == 0 && len(c.goModels) == 0
}

func toSet(items []string) map[string]bool {
	out := make(map[string]bool, len(items))
	for _, item := range items {
		out[item] = true
	}
	return out
}

func cloneProtocols(source map[string]protocol.Protocol) map[string]protocol.Protocol {
	result := make(map[string]protocol.Protocol, len(source))
	for model, p := range source {
		result[model] = p
	}
	return result
}

func cloneBools(source map[string]bool) map[string]bool {
	result := make(map[string]bool, len(source))
	for model, value := range source {
		result[model] = value
	}
	return result
}

func cloneModelMeta(source map[Tier]map[string]Metadata) map[Tier]map[string]Metadata {
	result := map[Tier]map[string]Metadata{TierZen: {}, TierGo: {}}
	for _, tier := range []Tier{TierZen, TierGo} {
		for id, md := range source[tier] {
			result[tier][id] = md
		}
	}
	return result
}

func sortedSetKeys(source map[string]bool) []string {
	result := make([]string, 0, len(source))
	for model, available := range source {
		if available {
			result = append(result, model)
		}
	}
	sort.Strings(result)
	return result
}
