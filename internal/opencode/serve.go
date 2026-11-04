package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"work2api/internal/core/protocol"
	"work2api/internal/core/provider"
	"work2api/internal/jsonutil"
	"work2api/internal/streamwatch"
)

func externalProtocol(p provider.Protocol) protocol.Protocol {
	switch p {
	case provider.ProtocolAnthropic:
		return protocol.Anthropic
	case provider.ProtocolSystemOne:
		return protocol.SystemOne
	case provider.ProtocolResponses:
		return protocol.Responses
	default:
		return protocol.Chat
	}
}

// Serve handles one inference request end-to-end, writing the client response
// to req.Writer and returning a usage report for the caller to log.
func (rt *Runtime) Serve(ctx context.Context, req provider.ServeRequest) (result provider.UsageReport, serveErr error) {
	if !rt.Ready() {
		return provider.UsageReport{}, errors.New("opencode 未配置")
	}
	external := externalProtocol(req.Protocol)
	w := req.Writer
	payload := req.Payload
	if payload == nil {
		payload = map[string]any{}
	}

	rawModel := jsonutil.StringAt(payload, "model")
	model := strings.TrimPrefix(rawModel, runtimePrefix)
	if model == "" {
		protocol.WriteError(w, external, http.StatusBadRequest, "model is required", "invalid_request_error", "model")
		return provider.UsageReport{Status: "error", Error: "model is required"}, nil
	}
	// Route and catalog lookups use the bare (un-namespaced) id, and the
	// upstream body must carry it too.
	payload["model"] = model

	if !rt.catalog.Supported(model) {
		msg := "the model uses an upstream protocol that opencode does not expose"
		protocol.WriteError(w, external, http.StatusBadRequest, msg, "invalid_request_error", "model")
		return provider.UsageReport{Status: "error", Error: msg}, nil
	}
	route, err := rt.catalog.Route(model, rt.zenNodes.Len() > 0, rt.goNodes.Len() > 0, rt.cfg.Anonymous)
	if err != nil {
		protocol.WriteError(w, external, http.StatusBadRequest, err.Error(), "invalid_request_error", "model")
		return provider.UsageReport{Status: "error", Error: err.Error()}, nil
	}

	if external == protocol.SystemOne && route.Protocol != protocol.SystemOne {
		msg := "该模型不使用 SystemOne 决策协议"
		protocol.WriteError(w, external, http.StatusBadRequest, msg, "invalid_request_error", "model")
		return provider.UsageReport{Status: "error", Error: msg}, nil
	}
	stream := jsonutil.BoolAt(payload, "stream")
	ids := deriveCallerRequestIDs(req)
	accountUID := ""
	ids.AccountUID = &accountUID
	defer func() { result.AccountUID = accountUID }()

	// System One decision payloads share no shape with the message bridge, so
	// they are relayed verbatim to every tier.
	if route.Protocol == protocol.SystemOne {
		return rt.serveSystemOne(ctx, w, external, payload, route, ids)
	}

	payload, toolNames := aliasClientTools(payload, external, route.Anonymous)
	compatibilityTools := route.Anonymous || rt.catalog.IsFreeModel(model)
	payload = describeClientTools(payload, external, toolNames, compatibilityTools)
	if len(toolNames) > 0 || compatibilityTools {
		blocked := map[string]bool{}
		for _, original := range toolNames {
			blocked[original] = true
		}
		if compatibilityTools {
			for _, name := range anonymousCoreTools {
				blocked[name] = true
			}
		}
		writer := &toolAliasWriter{ResponseWriter: w, names: toolNames, blocked: blocked}
		w = writer
		defer func() {
			if err := writer.Finish(); err != nil {
				result.Status = "error"
				result.Error = err.Error()
				serveErr = err
			}
		}()
	}
	bodies, bodyLimits, err := rt.prepareRouteBodiesWithLimits(external, route, payload)
	if err != nil {
		protocol.WriteError(w, external, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
		return provider.UsageReport{Status: "error", Error: err.Error()}, nil
	}

	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(rt.cfg.Retry.TimeoutSeconds)*time.Second)
	defer cancel()
	resp, upstreamRoute, err := rt.doUpstream(requestCtx, route, bodies, ids)
	if err != nil {
		rt.logger.Warn("all upstream attempts failed", "component", "opencode.upstream", "request_id", ids.Request, "model", model, "error", err)

		if protocol.ClientCanceled(ctx, err) {
			w.WriteHeader(499)
			return provider.UsageReport{Status: "incomplete", Error: "client canceled"}, nil
		}
		if errors.Is(err, errProxyUnavailable) {
			protocol.WriteError(w, external, http.StatusServiceUnavailable, "configured proxy route unavailable", "proxy_error", ids.Request)
			return provider.UsageReport{Status: "error", Error: "proxy route unavailable"}, nil
		}
		if errors.Is(err, context.DeadlineExceeded) {
			protocol.WriteError(w, external, http.StatusGatewayTimeout, "upstream request timed out", "upstream_timeout", ids.Request)
			return provider.UsageReport{Status: "error", Error: "upstream timeout"}, nil
		}
		var api *provider.APIError
		if errors.As(err, &api) {
			protocol.WriteError(w, external, api.Status, "当前模型或账号权限已改变，请重新发起请求", "account_not_allowed", ids.Request)
			return provider.UsageReport{Status: "error", Error: api.Error()}, api
		}
		protocol.WriteError(w, external, http.StatusBadGateway, "all upstream attempts failed", "upstream_error", ids.Request)
		return provider.UsageReport{Status: "error", Error: "upstream error"}, nil
	}
	defer resp.Body.Close()
	defer func() { result.EffectiveLimits = bodyLimits[upstreamRoute.Tier] }()

	w.Header().Set("x-request-id", ids.Request)
	if resp.StatusCode/100 != 2 {
		msg := copyErrorResponse(w, external, resp, ids.Request)
		return provider.UsageReport{Status: "error", Error: msg}, nil
	}

	if stream {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(resp.StatusCode)
		// 看门狗：空闲 180s / 总时长 30min，防上游建流后挂住（三通道同款）。
		watch := streamwatch.NewWatch(resp.Body, ctx, 0, 0)
		defer watch.Close()
		var usage protocol.Usage
		var known bool
		if external == upstreamRoute.Protocol {
			usage, known, err = protocol.ForwardStream(ctx, w, watch.Reader(resp.Body), upstreamRoute.Protocol, model)
		} else {
			usage, known, err = protocol.TranscodeStream(ctx, w, watch.Reader(resp.Body), upstreamRoute.Protocol, external, model)
		}
		report := usageReport(usage, known)
		if protocol.ClientCanceled(ctx, err) {
			report.Status, report.Error = "incomplete", "client canceled"
		}
		if err != nil && !protocol.ClientCanceled(ctx, err) {
			rt.logger.Warn("downstream stream ended with an error", "component", "opencode.stream", "request_id", ids.Request, "model", model, "error", err)
			report.Status = "error"
			report.Error = err.Error()
		}
		return report, nil
	}

	var responseBody []byte
	var usage protocol.Usage
	var known bool
	// Free models (and the anonymous lane) are force-streamed upstream even when
	// the client asked for a single JSON document; collapse the SSE back.
	if upstreamRoute.Anonymous || rt.catalog.IsFreeModel(upstreamRoute.ID) {
		watch := streamwatch.NewWatch(resp.Body, ctx, 0, 0)
		defer watch.Close()
		responseBody, usage, known, err = protocol.CollapseStreamWithUsageContext(ctx, watch.Reader(resp.Body), upstreamRoute.Protocol, model)
		if err != nil {
			rt.logger.Warn("stream collapse failed", "component", "opencode.conversion", "request_id", ids.Request, "model", model, "error", err)
			protocol.WriteError(w, external, http.StatusBadGateway, "unsupported upstream response", "upstream_error", ids.Request)
			report := usageReport(usage, known)
			report.Status, report.Error = "error", "collapse failed"
			return report, nil
		}
	} else {
		responseBody, err = streamwatch.ReadBody(ctx, resp.Body, 0)
		if err != nil {
			protocol.WriteError(w, external, http.StatusBadGateway, "failed to read upstream response", "upstream_error", ids.Request)
			return provider.UsageReport{Status: "error", Error: "read upstream failed"}, nil
		}
		usage, known = protocol.ResponseUsage(upstreamRoute.Protocol, responseBody)
	}
	if external != upstreamRoute.Protocol {
		responseBody, err = protocol.ConvertResponse(upstreamRoute.Protocol, external, responseBody)
		if err != nil {
			rt.logger.Warn("response protocol conversion failed", "component", "opencode.conversion", "request_id", ids.Request, "model", model, "error", err)
			protocol.WriteError(w, external, http.StatusBadGateway, "unsupported upstream response", "upstream_error", ids.Request)
			report := usageReport(usage, known)
			report.Status, report.Error = "error", "conversion failed"
			return report, nil
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(responseBody)
	return usageReport(usage, known), nil
}

// serveSystemOne relays a System One decision payload verbatim to every tier.
func (rt *Runtime) serveSystemOne(ctx context.Context, w http.ResponseWriter, external protocol.Protocol, payload map[string]any, route Route, ids RequestIDs) (provider.UsageReport, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		protocol.WriteError(w, external, http.StatusBadRequest, "request contains unsupported JSON values", "invalid_request_error", "")
		return provider.UsageReport{Status: "error", Error: "bad payload"}, nil
	}
	bodies := make(map[Tier][]byte, len(route.KeyTiers)+1)
	bodies[route.Tier] = body
	for _, tier := range route.KeyTiers {
		bodies[tier] = body
	}
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(rt.cfg.Retry.TimeoutSeconds)*time.Second)
	defer cancel()
	resp, upstreamRoute, err := rt.doUpstream(requestCtx, route, bodies, ids)
	if err != nil {

		if protocol.ClientCanceled(ctx, err) {
			w.WriteHeader(499)
			return provider.UsageReport{Status: "incomplete", Error: "client canceled"}, nil
		}
		if errors.Is(err, errProxyUnavailable) {
			protocol.WriteError(w, protocol.SystemOne, http.StatusServiceUnavailable, "configured proxy route unavailable", "proxy_error", ids.Request)
			return provider.UsageReport{Status: "error", Error: "proxy route unavailable"}, nil
		}
		if errors.Is(err, context.DeadlineExceeded) {
			protocol.WriteError(w, protocol.SystemOne, http.StatusGatewayTimeout, "upstream request timed out", "upstream_timeout", ids.Request)
			return provider.UsageReport{Status: "error", Error: "upstream timeout"}, nil
		}
		var api *provider.APIError
		if errors.As(err, &api) {
			protocol.WriteError(w, protocol.SystemOne, api.Status, "当前模型或账号权限已改变，请重新发起请求", "account_not_allowed", ids.Request)
			return provider.UsageReport{Status: "error", Error: api.Error()}, api
		}
		protocol.WriteError(w, protocol.SystemOne, http.StatusBadGateway, "all upstream attempts failed", "upstream_error", ids.Request)
		return provider.UsageReport{Status: "error", Error: "upstream error"}, nil
	}
	defer resp.Body.Close()
	w.Header().Set("x-request-id", ids.Request)
	if resp.StatusCode/100 != 2 {
		msg := copyErrorResponse(w, protocol.SystemOne, resp, ids.Request)
		return provider.UsageReport{Status: "error", Error: msg}, nil
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		w.Header().Set("Content-Type", ct)
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(resp.StatusCode)
		watch := streamwatch.NewWatch(resp.Body, ctx, 0, 0)
		defer watch.Close()
		_, err = io.Copy(flushingWriter{w}, watch.Reader(resp.Body))
		report := usageReport(protocol.Usage{}, false)
		if protocol.ClientCanceled(ctx, err) {
			report.Status, report.Error = "incomplete", "client canceled"
		} else if err != nil {
			report.Status, report.Error = "error", err.Error()
		}
		return report, nil
	}
	responseBody, err := streamwatch.ReadBody(ctx, resp.Body, 0)
	if err != nil {
		protocol.WriteError(w, protocol.SystemOne, http.StatusBadGateway, "failed to read upstream response", "upstream_error", ids.Request)
		return provider.UsageReport{Status: "error", Error: "read upstream failed"}, nil
	}
	usage, known := protocol.ResponseUsage(upstreamRoute.Protocol, responseBody)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(responseBody)
	return usageReport(usage, known), nil
}

type flushingWriter struct{ http.ResponseWriter }

func (w flushingWriter) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	if err == nil {
		err = http.NewResponseController(w.ResponseWriter).Flush()
		if errors.Is(err, http.ErrNotSupported) {
			err = nil
		}
	}
	return n, err
}

func (rt *Runtime) prepareRouteBodies(from protocol.Protocol, route Route, input map[string]any) (map[Tier][]byte, error) {
	bodies, _, err := rt.prepareRouteBodiesWithLimits(from, route, input)
	return bodies, err
}

func (rt *Runtime) prepareRouteBodiesWithLimits(from protocol.Protocol, route Route, input map[string]any) (map[Tier][]byte, map[Tier]map[string]int, error) {
	tiers := make([]Tier, 0, len(route.KeyTiers)+1)
	seen := make(map[Tier]bool, len(route.KeyTiers)+1)
	addTier := func(tier Tier) {
		if tier != TierZen && tier != TierGo || seen[tier] {
			return
		}
		seen[tier] = true
		tiers = append(tiers, tier)
	}
	addTier(route.Tier)
	for _, tier := range route.KeyTiers {
		addTier(tier)
	}
	if len(tiers) == 0 {
		return nil, nil, errors.New("no usable upstream tier")
	}
	bodies := make(map[Tier][]byte, len(tiers))
	limits := make(map[Tier]map[string]int, len(tiers))
	for _, tier := range tiers {
		proto := route.ProtocolFor(tier)
		baseURL := rt.cfg.Upstream.Zen
		if tier == TierGo {
			baseURL = rt.cfg.Upstream.Go
		}
		upstreamPayload, err := protocol.PrepareRequest(from, proto, input, baseURL)
		if err != nil {
			if tier != route.Tier {
				continue
			}
			return nil, nil, fmt.Errorf("prepare %s upstream request: %w", tier, err)
		}
		// Only this endpoint requires a minimum Responses budget; never change
		// public protocol conversion or larger caller-provided limits.
		if proto == protocol.Responses {
			var n float64
			switch v := upstreamPayload["max_output_tokens"].(type) {
			case int:
				n = float64(v)
			case float64:
				n = v
			case json.Number:
				n, _ = v.Float64()
			}
			if n > 0 && n < 16 && math.Trunc(n) == n {
				upstreamPayload["max_output_tokens"] = 16
			}
		}
		if effort := rt.cfg.ForcedEffort(jsonutil.StringAt(upstreamPayload, "model")); effort != "" {
			protocol.ForcedEffort(proto, upstreamPayload, effort)
		}
		encoded, err := json.Marshal(upstreamPayload)
		if err != nil {
			return nil, nil, errors.New("request contains unsupported JSON values")
		}
		bodies[tier] = encoded
		for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
			var n float64
			switch value := upstreamPayload[key].(type) {
			case float64:
				n = value
			case int:
				n = float64(value)
			case json.Number:
				n, _ = value.Float64()
			}
			if n > 0 && n <= 1<<30 && n == float64(int(n)) {
				if limits[tier] == nil {
					limits[tier] = map[string]int{}
				}
				limits[tier][key] = int(n)
			}
		}
	}
	return bodies, limits, nil
}

// copyErrorResponse relays an upstream error to the client and returns the
// extracted message for the usage report.
func copyErrorResponse(w http.ResponseWriter, proto protocol.Protocol, resp *http.Response, requestID string) string {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	message := http.StatusText(resp.StatusCode)
	var value map[string]any
	if json.Unmarshal(body, &value) == nil {
		message = jsonutil.FirstString(jsonutil.StringAt(value, "error", "message"), jsonutil.StringAt(value, "message"), message)
	}
	protocol.WriteError(w, proto, resp.StatusCode, message, "upstream_error", requestID)
	return message
}

func usageReport(usage protocol.Usage, known bool) provider.UsageReport {
	status := "ok"
	if usage.FinishReason == "length" || usage.FinishReason == "content_filter" {
		status = "incomplete"
	}
	var cached *int
	if usage.CachedKnown {
		n := usage.Cached
		cached = &n
	}
	return provider.UsageReport{
		FinishReason:        usage.FinishReason,
		TokensKnown:         &known,
		ReasoningTokens:     observedCount(usage.Reasoning, usage.ReasoningKnown),
		CacheCreationTokens: observedCount(usage.CacheCreation, usage.CacheCreationKnown),
		CachedTokens:        cached,
		InputTokens:         usage.Input,
		OutputTokens:        usage.Output,
		Status:              status,
	}
}

func observedCount(n int, known bool) *int {
	if !known {
		return nil
	}
	return &n
}
