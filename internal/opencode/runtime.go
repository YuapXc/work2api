package opencode

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"work2api/internal/core/provider"
)

// Name / Prefix constants for the opencode runtime.
const (
	runtimeName   = "opencode"
	runtimePrefix = "opencode/"
)

// Runtime is the opencode provider.Runtime implementation. When no config is
// found it stays inert: Ready() is false, Models() is empty, and Serve()
// returns an error.
type Runtime struct {
	cfg        Config
	logger     *slog.Logger
	transports *transportPool
	zenNodes   *nodePool
	goNodes    *nodePool
	anonymous  *anonymousPool
	catalog    *Catalog
	pricing    *PricingStore
	ready      bool
}

// compile-time assertion that Runtime satisfies the shared contract.
var _ provider.Runtime = (*Runtime)(nil)

// New builds the opencode runtime. It loads an opencode-style config from
// $OPENCODE_CONFIG when set, else <cwd>/opencode.json when present; with
// neither it returns an inert (not-ready) runtime and a nil error. A config
// file that exists but is malformed is a hard error.
func New(logger *slog.Logger) (*Runtime, error) {
	if logger == nil {
		logger = slog.Default()
	}
	path, explicit := configPath()
	if path == "" {
		return &Runtime{logger: logger}, nil
	}
	if !explicit {
		if _, err := os.Stat(path); err != nil {
			// No opencode.json in cwd: stay inert rather than fail startup.
			return &Runtime{logger: logger}, nil
		}
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	return newConfigured(cfg, path, logger)
}

func configPath() (string, bool) {
	if env := os.Getenv("OPENCODE_CONFIG"); env != "" {
		return env, true
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", false
	}
	return filepath.Join(cwd, "opencode.json"), false
}

func newConfigured(cfg Config, path string, logger *slog.Logger) (*Runtime, error) {
	transports, err := newTransportPool(cfg.RuntimeProxies(), cfg.Performance, cfg.Performance.AttemptTimeout(time.Duration(cfg.Retry.TimeoutSeconds)*time.Second))
	if err != nil {
		return nil, err
	}
	cooldown := time.Duration(cfg.Performance.FailureCooldownSeconds) * time.Second
	zenNodes, err := newNodePool(cfg.ZenKeys, transports, cooldown)
	if err != nil {
		return nil, err
	}
	goNodes, err := newNodePool(cfg.GoKeys, transports, cooldown)
	if err != nil {
		return nil, err
	}
	catalog := NewCatalog(cfg.Prefer, cfg.Models.Protocols)
	catalog.SetRefreshInterval(time.Duration(cfg.Models.RefreshSeconds) * time.Second)
	catalog.SetCachePath(CatalogCachePath(path))
	_ = catalog.LoadCache(CatalogCachePath(path))
	pricing := NewPricingStore(path, logger)
	catalog.SetPricingStore(pricing)

	rt := &Runtime{
		cfg:        cfg,
		logger:     logger,
		transports: transports,
		zenNodes:   zenNodes,
		goNodes:    goNodes,
		anonymous:  newAnonymousPool(cfg.Anonymous, transports, cooldown),
		catalog:    catalog,
		pricing:    pricing,
		ready:      true,
	}

	pricing.SetClientProvider(rt.healthyClients)

	// Background maintenance runs for the process lifetime. There is no explicit
	// shutdown hook in the Runtime contract, so a process-scoped context is used.
	ctx := context.Background()
	pricing.Start(ctx)
	rt.StartModelRefresh(ctx)
	rt.StartProxyHealthChecks(ctx)
	return rt, nil
}

// healthyClients exposes the currently healthy proxy transports so the pricing
// refresh can reuse them (most preferred first).
func (rt *Runtime) healthyClients() []*http.Client {
	if rt.transports == nil {
		return nil
	}
	clients := make([]*http.Client, 0, len(rt.transports.items))
	for _, proxy := range rt.transports.items {
		if proxy != nil && proxy.healthy.Load() {
			clients = append(clients, proxy.client)
		}
	}
	return clients
}

func (rt *Runtime) Name() string   { return runtimeName }
func (rt *Runtime) Prefix() string { return runtimePrefix }
func (rt *Runtime) Ready() bool    { return rt != nil && rt.ready }

// Models returns the discovered catalog, each ID namespaced "opencode/".
func (rt *Runtime) Models(ctx context.Context) []provider.CatalogModel {
	if !rt.Ready() {
		return nil
	}
	routes, _ := rt.catalog.AvailableModels(rt.zenNodes.Len() > 0, rt.goNodes.Len() > 0, rt.cfg.Anonymous)
	out := make([]provider.CatalogModel, 0, len(routes))
	for _, route := range routes {
		md := rt.catalog.MetadataForTier(route.ID, route.Tier)
		decision := rt.catalog.anonymousDecision(route.ID)
		extra := map[string]any{
			"free":            decision.Allowed,
			"tier":            string(route.Tier),
			"native_protocol": string(route.Protocol),
			"reasoning":       md.Reasoning,
			"tool_call":       md.ToolCall,
			"anonymous":       route.Anonymous,
			"pricing_known":   decision.Known,
		}
		if decision.InputCost != nil {
			extra["input_cost"] = *decision.InputCost
		}
		if decision.OutputCost != nil {
			extra["output_cost"] = *decision.OutputCost
		}
		out = append(out, provider.CatalogModel{
			ID:         runtimePrefix + route.ID,
			Name:       route.ID,
			Vision:     hasImageModality(md.InputModalities),
			Modalities: md.InputModalities,
			MaxOutput:  md.MaxOutput,
			Context:    md.ContextWindow,
			Extra:      extra,
		})
	}
	return out
}

func hasImageModality(modalities []string) bool {
	for _, m := range modalities {
		if m == "image" {
			return true
		}
	}
	return false
}
