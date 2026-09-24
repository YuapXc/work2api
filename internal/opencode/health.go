package opencode

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"
)

const (
	proxyHealthCheckURL      = "https://cloudflare.com/cdn-cgi/trace"
	proxyHealthCheckInterval = 15 * time.Minute
	proxyHealthCheckTimeout  = 10 * time.Second
)

// syncProxyResult updates proxy health from real traffic. Only timeouts and
// connection refusals mark a proxy unavailable.
func (rt *Runtime) syncProxyResult(ctx context.Context, proxy *proxyTransport, status int, err error) bool {
	if proxy == nil {
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

func (rt *Runtime) StartModelRefresh(ctx context.Context) {
	refresh := func() {
		var zen, goModels []string
		var capabilities Capabilities
		var capabilitiesErr error
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); zen = rt.refreshZen(ctx) }()
		go func() { defer wg.Done(); goModels = rt.refreshTier(ctx, rt.cfg.Upstream.Go, rt.goNodes) }()
		go func() {
			defer wg.Done()
			capabilityCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			capabilities, capabilitiesErr = rt.refreshProtocolCapabilities(capabilityCtx)
		}()
		wg.Wait()
		if ctx.Err() != nil {
			return
		}
		if capabilitiesErr != nil {
			rt.logger.Warn("OpenCode capability catalog refresh failed", "component", "opencode.models", "error", capabilitiesErr)
		}
		if zen != nil || goModels != nil {
			rt.catalog.ReplaceWithCapabilities(zen, goModels, capabilities.Protocols, capabilities.Unsupported, capabilities.Metadata)
			if ctx.Err() == nil {
				if err := rt.catalog.SaveCache(); err != nil {
					rt.logger.Warn("model catalog cache write failed", "component", "opencode.models", "error", err)
				}
			}
			rt.logger.Info("model catalog refreshed", "component", "opencode.models", "models", len(rt.catalog.List()))
		}
	}
	go func() {
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
		node := cursor.Next()
		if node == nil {
			break
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		models, status, err := FetchModels(refreshCtx, node.proxy.client, base, anonymousZenKey)
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
