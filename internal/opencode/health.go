package opencode

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
	"work2api/internal/core/provider"
)

const (
	proxyHealthCheckURL      = "https://cloudflare.com/cdn-cgi/trace"
	proxyHealthCheckInterval = 15 * time.Minute
	proxyHealthCheckTimeout  = 10 * time.Second
)

// syncProxyResult updates proxy health from real traffic. Only timeouts and
// connection refusals mark a proxy unavailable.
func (rt *Runtime) syncProxyResult(ctx context.Context, proxy *proxyTransport, status int, err error) bool {
	if proxy == nil || isLocalDNSFailure(err) {
		return false
	}
	if isProxyFailure(err) {
		rt.rebindFailedProxy(proxy)
		rt.verifyProxyAfterError(ctx, proxy, status)
		return true
	}
	if status >= 200 && status < 400 {
		wasHealthy := proxy.healthy.Swap(true)
		if !wasHealthy {
			rt.restoreProxy(proxy)
		}
		return false
	}
	if err != nil {
		rt.verifyProxyAfterError(ctx, proxy, status)
		return false
	}
	if status >= 400 && status < 600 {
		rt.verifyProxyAfterError(ctx, proxy, status)
	}
	return false
}

func (rt *Runtime) verifyProxyAfterError(ctx context.Context, proxy *proxyTransport, status int) {
	if !proxy.checking.CompareAndSwap(false, true) {
		return
	}
	checkCtx := context.WithoutCancel(ctx)
	go func() {
		result := rt.transports.checkClaimedProxy(checkCtx, proxy, proxyHealthCheckURL, proxyHealthCheckTimeout)
		rt.applyProxyHealthResult(result, "upstream HTTP response", status)
	}()
}

func (rt *Runtime) rebindFailedProxy(proxy *proxyTransport) (zenMoved, goMoved int) {
	if proxy == nil {
		return 0, 0
	}
	wasHealthy := proxy.healthy.Swap(false)
	return rt.rebindUnavailableProxy(proxy, wasHealthy)
}

func (rt *Runtime) rebindUnavailableProxy(proxy *proxyTransport, wasHealthy bool) (zenMoved, goMoved int) {
	zenMoved = rt.zenNodes.RebindProxy(proxy.index)
	goMoved = rt.goNodes.RebindProxy(proxy.index)
	if wasHealthy || zenMoved+goMoved > 0 {
		rt.logger.Warn("proxy became unavailable", "component", "opencode.proxy", "proxy", RedactURL(proxy.name), "zen_keys_moved", zenMoved, "go_keys_moved", goMoved)
	}
	return zenMoved, goMoved
}

func (rt *Runtime) restoreProxy(proxy *proxyTransport) (zenMoved, goMoved int) {
	if proxy == nil {
		return 0, 0
	}
	zenMoved = rt.zenNodes.RestoreProxy(proxy.index)
	goMoved = rt.goNodes.RestoreProxy(proxy.index)
	if zenMoved+goMoved > 0 {
		rt.logger.Info("proxy connectivity restored", "component", "opencode.proxy", "proxy", RedactURL(proxy.name), "zen_keys_moved", zenMoved, "go_keys_moved", goMoved)
	}
	return zenMoved, goMoved
}

func (rt *Runtime) StartProxyHealthChecks(ctx context.Context) {
	check := func() {
		results := rt.transports.CheckHealth(ctx, proxyHealthCheckURL, proxyHealthCheckTimeout)
		for _, result := range results {
			rt.applyProxyHealthResult(result, "scheduled health check", 0)
		}
	}
	go func() {
		ticker := time.NewTicker(proxyHealthCheckInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check()
			}
		}
	}()
}

func (rt *Runtime) applyProxyHealthResult(result proxyHealthResult, source string, upstreamStatus int) {
	if result.err == nil {
		if !result.wasHealthy {
			rt.restoreProxy(result.proxy)
		}
		return
	}
	if !result.failed {
		return
	}
	if rt.transports.hasHealthy() {
		zenMoved, goMoved := rt.rebindUnavailableProxy(result.proxy, result.wasHealthy)
		if result.wasHealthy || zenMoved+goMoved > 0 {
			rt.logger.Warn("proxy health check failed", "component", "opencode.proxy", "source", source, "upstream_status", upstreamStatus, "proxy", RedactURL(result.proxy.name), "error", result.err)
			return
		}
	}
}

// Explicit and background refreshes share one cancellable task per runtime.
func (rt *Runtime) RefreshModels(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	if rt.lifecycleCtx != nil {
		stop := context.AfterFunc(rt.lifecycleCtx, cancel)
		defer stop()
		if err := rt.lifecycleCtx.Err(); err != nil {
			return err
		}
	}
	if registry := rt.registry.Load(); registry != nil && !registry.IsCurrent(rt) {
		return errors.New("渠道配置已变更")
	}
	return rt.modelRefresh.Do(ctx, "catalog", rt.refreshModels)
}
func (rt *Runtime) refreshModels(ctx context.Context) error {
	previous := rt.catalog.Fingerprint()
	var zen, goModels []string
	var capabilities Capabilities
	var capabilitiesErr error
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); zen = rt.refreshZen(ctx) }()
	go func() { defer wg.Done(); goModels = rt.refreshTier(ctx, rt.cfg.Upstream.Go, rt.goNodes) }()
	go func() { defer wg.Done(); capabilities, capabilitiesErr = rt.refreshProtocolCapabilities(ctx) }()
	wg.Wait()
	saveMu.Lock()
	defer saveMu.Unlock()
	if registry := rt.registry.Load(); registry != nil && !registry.IsCurrent(rt) {
		return errors.New("渠道配置已变更")
	}
	if ctx.Err() != nil {
		rt.catalog.mu.Lock()
		rt.catalog.stale = true
		rt.catalog.mu.Unlock()
		return ctx.Err()
	}
	if zen == nil && goModels == nil {
		rt.catalog.mu.Lock()
		rt.catalog.stale = true
		rt.catalog.mu.Unlock()
		return errors.New("OpenCode 模型目录刷新失败，保留现有目录")
	}
	partial := capabilitiesErr != nil || (zen == nil && (rt.zenNodes.Len() > 0 || rt.cfg.Anonymous)) || (goModels == nil && rt.goNodes.Len() > 0)
	rt.catalog.ReplaceWithCapabilities(zen, goModels, capabilities.Protocols, capabilities.Unsupported, capabilities.Metadata)
	rt.catalog.mu.Lock()
	rt.catalog.stale = partial
	rt.catalog.mu.Unlock()
	if err := rt.catalog.SaveCache(); err != nil && rt.logger != nil {
		rt.logger.Warn("model catalog cache write failed", "error", err)
	}
	if rt.logger != nil && previous != rt.catalog.Fingerprint() {
		rt.logger.Info("model catalog refreshed", "component", "opencode.models", "models", len(rt.catalog.List()))
	}
	if partial {
		return provider.ErrPartialRefresh
	}
	return nil
}
func (rt *Runtime) StartModelRefresh(ctx context.Context) {
	go func() {
		refresh := func() {
			if err := rt.RefreshModels(ctx); err != nil && ctx.Err() == nil && rt.logger != nil {
				rt.logger.Warn("model catalog refresh failed", "component", "opencode.models", "error", err)
			}
		}
		refresh()
		ticker := time.NewTicker(time.Duration(rt.cfg.Models.RefreshSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
}

func (rt *Runtime) refreshProtocolCapabilities(ctx context.Context) (Capabilities, error) {
	if rt.transports == nil || len(rt.transports.items) == 0 {
		return FetchCapabilities(ctx, &http.Client{Timeout: 30 * time.Second}, CapabilitiesURL)
	}
	var lastErr error
	for _, proxy := range rt.transports.items {
		if ctx.Err() != nil {
			return Capabilities{}, ctx.Err()
		}
		if proxy == nil || !proxy.healthy.Load() {
			continue
		}
		capabilities, err := FetchCapabilities(ctx, proxy.client, CapabilitiesURL)
		if err == nil {
			return capabilities, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no healthy proxy available for OpenCode capability catalog")
	}
	return Capabilities{}, lastErr
}

func (rt *Runtime) refreshZen(ctx context.Context) []string {
	if models := rt.refreshTier(ctx, rt.cfg.Upstream.Zen, rt.zenNodes); models != nil {
		return models
	}
	if !rt.cfg.Anonymous {
		return nil
	}
	return rt.refreshAnonymousTier(ctx, rt.cfg.Upstream.Zen)
}

func (rt *Runtime) refreshAnonymousTier(ctx context.Context, base string) []string {
	cursor := rt.anonymous.CursorFor("")
	limit := rt.anonymous.Len()
	for attempt := 1; attempt <= limit; attempt++ {
		if ctx.Err() != nil {
			return nil
		}
		node := cursor.Next()
		if node == nil {
			break
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		models, status, err := FetchModels(refreshCtx, node.proxy.client, base, anonymousZenKey)
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			cancel()
			return nil
		}
		rt.syncProxyResult(refreshCtx, node.proxy, status, err)
		cancel()
		if err == nil {
			rt.anonymous.MarkSuccess(node)
			return models
		}
		rt.anonymous.MarkFailure(node, nil, err)
	}
	rt.logger.Warn("anonymous model catalog refresh failed", "component", "opencode.models", "upstream", RedactURL(base))
	return nil
}

func (rt *Runtime) refreshTier(ctx context.Context, base string, nodes *nodePool) []string {
	cursor := nodes.Cursor()
	for attempt := 0; attempt < rt.cfg.Retry.MaxAttempts; attempt++ {
		if ctx.Err() != nil {
			return nil
		}
		node := cursor.Next()
		if node == nil {
			return nil
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		proxy := nodes.Proxy(node)
		if proxy == nil {
			cancel()
			return nil
		}
		models, status, err := FetchModels(refreshCtx, proxy.client, base, node.key)
		if ctx.Err() != nil || errors.Is(err, context.Canceled) {
			cancel()
			return nil
		}
		rt.syncProxyResult(refreshCtx, proxy, status, err)
		cancel()
		if err == nil {
			nodes.MarkSuccess(node)
			return models
		}
		nodes.MarkFailure(node, nil, err)
	}
	return nil
}
