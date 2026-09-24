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
	path       string // resolved config path (for display + hot-reload writes)
	cancel     context.CancelFunc
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
func New(logger *slog.Logger, dataDir string) (*Runtime, error) {
	if logger == nil {
		logger = slog.Default()
	}
	path, explicit := configPath(dataDir)
	// savePath is where the WebUI editor persists, even when currently inert.
	savePath := path
	if savePath == "" {
		savePath = "opencode.json"
	}
	if path == "" {
		return &Runtime{logger: logger, path: savePath}, nil
	}
	if !explicit {
		// 迁移：老版本把 opencode.json 放在工作目录根，现在默认落在 <dataDir>/opencode/。
		// 新路径不存在但根目录有旧文件时，搬过去（连同缓存），保持根目录整洁且不丢配置。
		if _, err := os.Stat(path); err != nil {
			migrateLegacyConfig(path, logger)
		}
		if _, err := os.Stat(path); err != nil {
			// 仍无配置：保持 inert，不因缺文件而启动失败。
			return &Runtime{logger: logger, path: savePath}, nil
		}
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		return nil, err
	}
	return newConfigured(cfg, path, logger)
}

// configPath 解析 opencode 配置路径：$OPENCODE_CONFIG 显式优先，否则默认落在
// <dataDir>/opencode/opencode.json（三个文件——配置 + 两个缓存——同目录聚合，
// 不再散在项目根）。dataDir 为空时退回工作目录下的 opencode/ 子目录。
func configPath(dataDir string) (string, bool) {
	if env := os.Getenv("OPENCODE_CONFIG"); env != "" {
		return env, true
	}
	base := dataDir
	if base == "" {
		if cwd, err := os.Getwd(); err == nil {
			base = cwd
		} else {
			return "", false
		}
	}
	return filepath.Join(base, "opencode", "opencode.json"), false
}

// migrateLegacyConfig 把旧的 <cwd>/opencode.json（及其两个缓存）搬到新路径。
// 尽力而为：任一步失败就放弃迁移（调用方会退回 inert），不影响启动。
func migrateLegacyConfig(newPath string, logger *slog.Logger) {
	cwd, err := os.Getwd()
	if err != nil {
		return
	}
	legacy := filepath.Join(cwd, "opencode.json")
	if legacy == newPath {
		return
	}
	if _, err := os.Stat(legacy); err != nil {
		return // 无旧文件
	}
	if err := os.MkdirAll(filepath.Dir(newPath), 0o755); err != nil {
		return
	}
	if err := os.Rename(legacy, newPath); err != nil {
		return
	}
	// 缓存文件跟随迁移（失败无妨，会在新路径重建）。
	for _, suffix := range []string{".models.catalog.json", ".models.dev.json"} {
		_ = os.Rename(legacy+suffix, newPath+suffix)
	}
	if logger != nil {
		logger.Info("opencode 配置已从项目根迁移到 data 目录", "from", legacy, "to", newPath)
	}
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
		path:       path,
		transports: transports,
		zenNodes:   zenNodes,
		goNodes:    goNodes,
		anonymous:  newAnonymousPool(cfg.Anonymous, transports, cooldown),
		catalog:    catalog,
		pricing:    pricing,
		ready:      true,
	}

	pricing.SetClientProvider(rt.healthyClients)

	// Background maintenance runs until the runtime is replaced (hot-reload) or
	// the process exits. A cancelable context lets a reloaded runtime stop the
	// old instance's loops instead of leaking a goroutine set per config save.
	ctx, cancel := context.WithCancel(context.Background())
	rt.cancel = cancel
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
