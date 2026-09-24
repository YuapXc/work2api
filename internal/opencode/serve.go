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
	"time"

	"work2api/internal/core/protocol"
	"work2api/internal/core/provider"
	"work2api/internal/jsonutil"
)

func externalProtocol(p provider.Protocol) protocol.Protocol {
	switch p {
	case provider.ProtocolAnthropic:
		return protocol.Anthropic
	case provider.ProtocolResponses:
		return protocol.Responses
	default:
		return protocol.Chat
	}
}

// Serve handles one inference request end-to-end, writing the client response
// to req.Writer and returning a usage report for the caller to log.
func (rt *Runtime) Serve(ctx context.Context, req provider.ServeRequest) (provider.UsageReport, error) {
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

	stream := jsonutil.BoolAt(payload, "stream")
	ids := deriveRequestIDs(payload)

	// System One decision payloads share no shape with the message bridge, so
	// they are relayed verbatim to every tier.
	if route.Protocol == protocol.SystemOne {
		return rt.serveSystemOne(ctx, w, external, payload, route, ids)
	}

	bodies, err := rt.prepareRouteBodies(external, route, payload)
	if err != nil {
		protocol.WriteError(w, external, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
		return provider.UsageReport{Status: "error", Error: err.Error()}, nil
	}

	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(rt.cfg.Retry.TimeoutSeconds)*time.Second)
	defer cancel()
	resp, upstreamRoute, err := rt.doUpstream(requestCtx, route, bodies, ids)
	if err != nil {
		rt.logger.Warn("all upstream attempts failed", "component", "opencode.upstream", "request_id", ids.Request, "model", model, "error", err)
		if errors.Is(err, context.DeadlineExceeded) {
			protocol.WriteError(w, external, http.StatusGatewayTimeout, "upstream request timed out", "upstream_timeout", ids.Request)
			return provider.UsageReport{Status: "error", Error: "upstream timeout"}, nil
		}
		protocol.WriteError(w, external, http.StatusBadGateway, "all upstream attempts failed", "upstream_error", ids.Request)
		return provider.UsageReport{Status: "error", Error: "upstream error"}, nil
	}
	defer resp.Body.Close()

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
		var usage protocol.Usage
		if external == upstreamRoute.Protocol {
			usage, _, err = protocol.ForwardStream(ctx, w, resp.Body, upstreamRoute.Protocol, model)
		} else {
			usage, _, err = protocol.TranscodeStream(ctx, w, resp.Body, upstreamRoute.Protocol, external, model)
		}
		report := usageReport(usage)
		if err != nil && !protocol.ClientCanceled(ctx, err) {
			rt.logger.Warn("downstream stream ended with an error", "component", "opencode.stream", "request_id", ids.Request, "model", model, "error", err)
			report.Status = "error"
			report.Error = err.Error()
		}
		return report, nil
	}

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		protocol.WriteError(w, external, http.StatusBadGateway, "failed to read upstream response", "upstream_error", ids.Request)
		return provider.UsageReport{Status: "error", Error: "read upstream failed"}, nil
	}
	// Free models (and the anonymous lane) are force-streamed upstream even when
	// the client asked for a single JSON document; collapse the SSE back.
	if upstreamRoute.Anonymous || rt.catalog.IsFreeModel(upstreamRoute.ID) {
		collapsed, err := protocol.CollapseStream(bytes.NewReader(responseBody), upstreamRoute.Protocol, model)
		if err != nil {
			rt.logger.Warn("stream collapse failed", "component", "opencode.conversion", "request_id", ids.Request, "model", model, "error", err)
			protocol.WriteError(w, external, http.StatusBadGateway, "unsupported upstream response", "upstream_error", ids.Request)
			return provider.UsageReport{Status: "error", Error: "collapse failed"}, nil
		}
		responseBody = collapsed
	}
	usage, _ := protocol.ResponseUsage(upstreamRoute.Protocol, responseBody)
	if external != upstreamRoute.Protocol {
		responseBody, err = protocol.ConvertResponse(upstreamRoute.Protocol, external, responseBody)
		if err != nil {
			rt.logger.Warn("response protocol conversion failed", "component", "opencode.conversion", "request_id", ids.Request, "model", model, "error", err)
			protocol.WriteError(w, external, http.StatusBadGateway, "unsupported upstream response", "upstream_error", ids.Request)
			return provider.UsageReport{Status: "error", Error: "conversion failed"}, nil
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(responseBody)
	return usageReport(usage), nil
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
		if errors.Is(err, context.DeadlineExceeded) {
			protocol.WriteError(w, protocol.SystemOne, http.StatusGatewayTimeout, "upstream request timed out", "upstream_timeout", ids.Request)
			return provider.UsageReport{Status: "error", Error: "upstream timeout"}, nil
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
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return provider.UsageReport{Status: "ok"}, nil
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		protocol.WriteError(w, protocol.SystemOne, http.StatusBadGateway, "failed to read upstream response", "upstream_error", ids.Request)
		return provider.UsageReport{Status: "error", Error: "read upstream failed"}, nil
	}
	usage, _ := protocol.ResponseUsage(upstreamRoute.Protocol, responseBody)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(responseBody)
	return usageReport(usage), nil
}

func (rt *Runtime) prepareRouteBodies(from protocol.Protocol, route Route, input map[string]any) (map[Tier][]byte, error) {
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
		return nil, errors.New("no usable upstream tier")
	}
	bodies := make(map[Tier][]byte, len(tiers))
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
			return nil, fmt.Errorf("prepare %s upstream request: %w", tier, err)
		}
		if effort := rt.cfg.ForcedEffort(jsonutil.StringAt(upstreamPayload, "model")); effort != "" {
			protocol.ForcedEffort(proto, upstreamPayload, effort)
		}
		encoded, err := json.Marshal(upstreamPayload)
		if err != nil {
			return nil, errors.New("request contains unsupported JSON values")
		}
		bodies[tier] = encoded
	}
	return bodies, nil
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

func usageReport(usage protocol.Usage) provider.UsageReport {
	return provider.UsageReport{
		InputTokens:  usage.Input,
		OutputTokens: usage.Output,
		Status:       "ok",
	}
}
