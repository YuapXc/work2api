package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"work2api/internal/core/protocol"
	"work2api/internal/jsonutil"
)

func (rt *Runtime) doUpstream(ctx context.Context, route Route, bodies map[Tier][]byte, ids RequestIDs) (*http.Response, Route, error) {
	resp, effectiveRoute, attempts, err := rt.doUpstreamTiers(ctx, route, bodies, ids, 0)
	if err != nil || resp == nil || resp.StatusCode != http.StatusBadRequest {
		return resp, effectiveRoute, err
	}
	origBody := resp.Body
	errBody, readErr := io.ReadAll(io.LimitReader(origBody, 1<<20))
	drainAndClose(origBody)
	if readErr != nil {
		resp.Body = io.NopCloser(bytes.NewReader(errBody))
		return resp, effectiveRoute, nil
	}
	resp.Body = io.NopCloser(bytes.NewReader(errBody))
	if !isStaleReasoningReference(errBody) {
		return resp, effectiveRoute, nil
	}
	stripped, changed := stripStaleReasoningInputs(effectiveRoute, bodies)
	if !changed {
		return resp, effectiveRoute, nil
	}
	rt.logger.Info("retrying upstream without stale reasoning references", "component", "opencode.upstream", "request_id", ids.Request, "model", route.ID, "tier", effectiveRoute.Tier)
	retryResp, retryRoute, _, retryErr := rt.doUpstreamTiers(ctx, effectiveRoute, stripped, ids, attempts)
	if retryErr != nil || retryResp == nil || retryResp.StatusCode/100 != 2 {
		if retryResp != nil {
			drainAndClose(retryResp.Body)
		}
		fallback := *resp
		fallback.Body = io.NopCloser(bytes.NewReader(errBody))
		return &fallback, effectiveRoute, nil
	}
	return retryResp, retryRoute, nil
}

func isStaleReasoningReference(body []byte) bool {
	text := strings.ToLower(string(body))
	if !strings.Contains(text, "reasoning item") && !strings.Contains(text, "reasoning reference") {
		return false
	}
	for _, marker := range []string{"not found", "expir", "does not exist", "no longer"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func stripStaleReasoningInputs(route Route, bodies map[Tier][]byte) (map[Tier][]byte, bool) {
	changed := false
	out := make(map[Tier][]byte, len(bodies))
	for tier, body := range bodies {
		if len(body) == 0 || route.ProtocolFor(tier) != protocol.Responses {
			out[tier] = body
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			out[tier] = body
			continue
		}
		tierChanged := false
		if _, ok := payload["previous_response_id"]; ok {
			delete(payload, "previous_response_id")
			tierChanged = true
		}
		if raw, ok := payload["input"].([]any); ok {
			kept := make([]any, 0, len(raw))
			for _, item := range raw {
				if m, ok := item.(map[string]any); ok && jsonutil.StringAt(m, "type") == "reasoning" {
					tierChanged = true
					continue
				}
				kept = append(kept, item)
			}
			if tierChanged {
				payload["input"] = kept
			}
		}
		if !tierChanged {
			out[tier] = body
			continue
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			out[tier] = body
			continue
		}
		out[tier] = encoded
		changed = true
	}
	return out, changed
}

func (rt *Runtime) doUpstreamTiers(ctx context.Context, route Route, bodies map[Tier][]byte, ids RequestIDs, attemptOffset int) (*http.Response, Route, int, error) {
	var lastResponse *http.Response
	var lastErr error
	effectiveRoute := route
	attempts := attemptOffset
	if route.Anonymous {
		resp, err, used := rt.doAnonymousUpstream(ctx, route, bodies, ids, attempts)
		attempts += used
		if err == nil && resp != nil && resp.StatusCode/100 == 2 {
			return resp, route, attempts, nil
		}
		lastResponse, lastErr = resp, err
	}
	if ctx.Err() != nil {
		if lastResponse != nil {
			return lastResponse, effectiveRoute, attempts, nil
		}
		if lastErr == nil {
			lastErr = ctx.Err()
		}
		return nil, effectiveRoute, attempts, lastErr
	}
	keyTiers := route.KeyTiers
	if !route.Anonymous && len(keyTiers) == 0 && (route.Tier == TierZen || route.Tier == TierGo) {
		keyTiers = []Tier{route.Tier}
	}
	for _, tier := range keyTiers {
		if lastResponse != nil {
			drainAndClose(lastResponse.Body)
			lastResponse = nil
		}
		keyRoute := route
		keyRoute.Tier = tier
		keyRoute.Anonymous = false
		keyRoute.Protocol = route.ProtocolFor(tier)
		effectiveRoute = keyRoute
		resp, err, used := rt.doKeyUpstream(ctx, keyRoute, bodies, ids, attempts)
		attempts += used
		if err == nil && resp != nil && resp.StatusCode/100 == 2 {
			return resp, keyRoute, attempts, nil
		}
		lastResponse, lastErr = resp, err
	}
	if lastResponse != nil {
		return lastResponse, effectiveRoute, attempts, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no usable upstream route")
	}
	return nil, effectiveRoute, attempts, lastErr
}

// doAnonymousUpstream tries every currently available proxy at most once via
// the shared "public" credential lane.
func (rt *Runtime) doAnonymousUpstream(ctx context.Context, route Route, bodies map[Tier][]byte, ids RequestIDs, attemptOffset int) (*http.Response, error, int) {
	var lastResponse *http.Response
	var lastErr error
	cursor := rt.anonymous.CursorFor(ids.Session)
	limit := rt.anonymous.Len()
	attempts := 0
	body := bodies[TierZen]
	if len(body) == 0 {
		return nil, errors.New("no prepared Zen request body"), 0
	}
	body = prepareAnonymousBody(body, route.ProtocolFor(TierZen))
	for attempts < limit {
		if ctx.Err() != nil {
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			break
		}
		node := cursor.Next()
		if node == nil {
			break
		}
		attempts++
		if lastResponse != nil {
			drainAndClose(lastResponse.Body)
			lastResponse = nil
		}
		req, err := newUpstreamRequest(ctx, rt.cfg.Upstream.Zen, route.Protocol, body, ids, anonymousZenKey)
		if err != nil {
			return nil, err, attempts
		}
		resp, err := node.proxy.client.Do(req)
		if ctx.Err() != nil {
			lastResponse, lastErr = resp, err
			if lastErr == nil && lastResponse == nil {
				lastErr = ctx.Err()
			}
			break
		}
		rt.observeAnonymousResult(ctx, node, resp, err)
		if err == nil && resp.StatusCode/100 == 2 {
			return resp, nil, attempts
		}
		lastResponse = resp
		lastErr = err
	}
	if lastResponse != nil {
		return lastResponse, nil, attempts
	}
	if lastErr == nil {
		lastErr = errors.New("no healthy anonymous proxies available")
	}
	return nil, lastErr, attempts
}

const anonymousZenKey = "public"

// anonymousCoreTools are the tool names the free tier expects on an
// agent-shaped request. Requests without them are rejected with 403.
var anonymousCoreTools = []string{"bash", "edit", "glob", "grep", "read"}

// prepareAnonymousBody normalizes a body for the free tier: streaming enabled,
// include_usage for Chat, and the core agent tools present. System One decision
// payloads are forwarded verbatim.
func prepareAnonymousBody(body []byte, proto protocol.Protocol) []byte {
	if proto == protocol.SystemOne {
		return body
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return body
	}
	changed := false
	if streaming, ok := payload["stream"].(bool); !ok || !streaming {
		payload["stream"] = true
		changed = true
	}
	if ensureAnonymousChatUsage(payload, proto) {
		changed = true
	}
	if ensureAnonymousTools(payload, proto) {
		changed = true
	}
	if !changed {
		return body
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return body
	}
	return encoded
}

func ensureAnonymousChatUsage(payload map[string]any, proto protocol.Protocol) bool {
	if proto != protocol.Chat {
		return false
	}
	options, ok := payload["stream_options"].(map[string]any)
	if !ok {
		payload["stream_options"] = map[string]any{"include_usage": true}
		return true
	}
	includeUsage, ok := options["include_usage"].(bool)
	if ok && includeUsage {
		return false
	}
	options["include_usage"] = true
	return true
}

func ensureAnonymousTools(payload map[string]any, proto protocol.Protocol) bool {
	raw, exists := payload["tools"]
	if !exists {
		payload["tools"] = anonymousToolset(proto, nil)
		return true
	}
	items, ok := raw.([]any)
	if !ok {
		return false
	}
	present := make(map[string]bool, len(items))
	for _, item := range items {
		if name := anonymousToolName(proto, item); name != "" {
			present[name] = true
		}
	}
	missing := make([]string, 0, len(anonymousCoreTools))
	for _, name := range anonymousCoreTools {
		if !present[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return false
	}
	payload["tools"] = append(items, anonymousToolset(proto, missing)...)
	return true
}

func anonymousToolName(proto protocol.Protocol, item any) string {
	entry, ok := item.(map[string]any)
	if !ok {
		return ""
	}
	if proto == protocol.Chat {
		return jsonutil.StringAt(entry, "function", "name")
	}
	return jsonutil.StringAt(entry, "name")
}

func anonymousToolset(proto protocol.Protocol, names []string) []any {
	if names == nil {
		names = anonymousCoreTools
	}
	tools := make([]any, 0, len(names))
	for _, name := range names {
		tools = append(tools, anonymousTool(proto, name))
	}
	return tools
}

func anonymousTool(proto protocol.Protocol, name string) map[string]any {
	description := "Agent tool " + name
	parameters := map[string]any{"type": "object", "properties": map[string]any{}}
	switch proto {
	case protocol.Chat:
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": description,
				"parameters":  parameters,
			},
		}
	case protocol.Anthropic:
		return map[string]any{
			"name":         name,
			"description":  description,
			"input_schema": parameters,
		}
	default:
		return map[string]any{
			"type":        "function",
			"name":        name,
			"description": description,
			"parameters":  parameters,
		}
	}
}

// shapeKeyBody normalizes key-tier free-model bodies to agent shape, mirroring
// the anonymous lane. Paid models keep their original bodies.
func (rt *Runtime) shapeKeyBody(body []byte, route Route, tier Tier) ([]byte, bool) {
	if !rt.catalog.IsFreeModel(route.ID) {
		return body, false
	}
	shaped := prepareAnonymousBody(body, route.ProtocolFor(tier))
	return shaped, !bytes.Equal(shaped, body)
}

func (rt *Runtime) doKeyUpstream(ctx context.Context, route Route, bodies map[Tier][]byte, ids RequestIDs, attemptOffset int) (*http.Response, error, int) {
	var lastResponse *http.Response
	var lastErr error
	nodes := rt.zenNodes
	baseURL := rt.cfg.Upstream.Zen
	if route.Tier == TierGo {
		nodes = rt.goNodes
		baseURL = rt.cfg.Upstream.Go
	}
	cursor := nodes.CursorFor(ids.Session)
	if nodes.Len() == 0 {
		return nil, fmt.Errorf("no %s nodes configured", route.Tier), 0
	}
	attempts := 0
	body := bodies[route.Tier]
	if len(body) == 0 {
		return nil, fmt.Errorf("no prepared %s request body", route.Tier), 0
	}
	if shaped, changed := rt.shapeKeyBody(body, route, route.Tier); changed {
		body = shaped
	}
	for attempts < rt.cfg.Retry.MaxAttempts {
		if ctx.Err() != nil {
			if lastErr == nil {
				lastErr = ctx.Err()
			}
			break
		}
		node := cursor.Next()
		if node == nil {
			break
		}
		attempts++
		if lastResponse != nil {
			drainAndClose(lastResponse.Body)
			lastResponse = nil
		}
		req, err := newUpstreamRequest(ctx, baseURL, route.Protocol, body, ids, node.key)
		if err != nil {
			return nil, err, attempts
		}
		proxy := nodes.Proxy(node)
		if proxy == nil {
			lastErr = errors.New("upstream key has no proxy binding")
			break
		}
		resp, err := proxy.client.Do(req)
		if ctx.Err() != nil {
			lastResponse, lastErr = resp, err
			if lastErr == nil && lastResponse == nil {
				lastErr = ctx.Err()
			}
			break
		}
		rt.observeKeyResult(ctx, nodes, node, proxy, resp, err)
		if err == nil && resp.StatusCode/100 == 2 {
			return resp, nil, attempts
		}
		if isNonRetryableClientResponse(resp, err) {
			return resp, nil, attempts
		}
		lastResponse = resp
		lastErr = err
	}
	if lastResponse != nil {
		return lastResponse, nil, attempts
	}
	return nil, lastErr, attempts
}

func (rt *Runtime) observeKeyResult(ctx context.Context, nodes *nodePool, node *upstreamNode, proxy *proxyTransport, resp *http.Response, err error) {
	status := upstreamStatus(resp)
	proxyFailed := rt.syncProxyResult(ctx, proxy, status, err)
	if err == nil && status/100 == 2 || isNonRetryableClientResponse(resp, err) {
		nodes.MarkSuccess(node)
		return
	}
	if !proxyFailed || nodes.Proxy(node) == proxy {
		nodes.MarkFailure(node, resp, err)
	}
}

func (rt *Runtime) observeAnonymousResult(ctx context.Context, node *anonymousNode, resp *http.Response, err error) {
	status := upstreamStatus(resp)
	rt.syncProxyResult(ctx, node.proxy, status, err)
	if err == nil && status/100 == 2 {
		rt.anonymous.MarkSuccess(node)
	} else {
		rt.anonymous.MarkFailure(node, resp, err)
	}
}

func upstreamStatus(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

func newUpstreamRequest(ctx context.Context, baseURL string, proto protocol.Protocol, body []byte, ids RequestIDs, key string) (*http.Request, error) {
	endpoint := strings.TrimRight(baseURL, "/") + protocol.Path(proto)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", userAgent())
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-session", ids.Session)
	req.Header.Set("x-session-affinity", ids.Session)
	req.Header.Set("X-Session-Id", ids.Session)
	req.Header.Set("x-opencode-request", ids.Request)
	req.Header.Set("x-opencode-project", ids.Project)
	if ids.ParentSession != "" {
		req.Header.Set("x-parent-session-id", ids.ParentSession)
	}
	if proto == protocol.Anthropic {
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("anthropic-beta", "interleaved-thinking-2025-05-14,fine-grained-tool-streaming-2025-05-14")
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	return req, nil
}

func isNonRetryableClientResponse(resp *http.Response, err error) bool {
	return err == nil && resp != nil && resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests
}
