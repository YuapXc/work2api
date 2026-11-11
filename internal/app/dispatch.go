package app

import (
	"context"
	"errors"
	"net/http"
	"time"

	"work2api/internal/core/protocol"
	"work2api/internal/core/provider"
	"work2api/internal/requestdiag"
	"work2api/internal/store"
	"work2api/internal/streamwatch"
)

// dispatchRuntime executes the registered default or namespaced provider.
func (s *Server) dispatchRuntime(w http.ResponseWriter, r *http.Request, proto provider.Protocol, payload map[string]any, principal *Principal) bool {
	return s.dispatchRuntimeTo(w, r, proto, payload, principal)
}

// dispatchRuntimeTo is dispatchRuntime with an explicit writer (the model test
// endpoint injects an httptest.Recorder to capture the response).
func (s *Server) dispatchRuntimeTo(w http.ResponseWriter, r *http.Request, proto provider.Protocol, payload map[string]any, principal *Principal) bool {
	if principal.RouteModel != "" {
		return s.dispatchModelRoute(w, r, proto, payload, principal)
	}
	rawModel := strOr(payload["model"], "")
	requestedLimits := requestdiag.OutputLimits(payload)
	if rawModel == "" {
		return false
	}
	resolved := s.o.resolveModel(rawModel)
	rt, ok := s.o.runtimes.Resolve(resolved)
	if !ok {
		return false
	}
	portal, eligible := rt.(provider.PortalProvider)
	if principal.UserID > 0 && (!eligible || !portal.SupportsPortal()) {
		writeAPIErr(w, errBody(403, "该渠道尚未开放用户调用", "model_not_allowed"))
		return true
	}
	// Hand the runtime the alias-resolved, still-namespaced id; the runtime
	// strips its own prefix before talking to its upstream.
	payload["model"] = resolved
	var customNames map[string]bool
	var namespaceNames map[string]protocol.NamespaceName
	if proto == provider.ProtocolResponses {
		var err error
		payload, customNames, namespaceNames, err = protocol.PrepareClientTools(payload)
		if err != nil {
			writeAPIErr(w, errBody(400, err.Error(), "invalid_request_error"))
			return true
		}
	}
	t0 := time.Now()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	call := provider.CurrentInvocation(ctx)
	call.Provider = rt.Name()
	if history, _ := r.Context().Value(responseHistoryContextKey{}).(*responseHistoryReplay); history != nil {
		call.Dispatched = history.dispatched
		call.PrepareBody = history.prepareBody
	}
	call.Caller = principal.SessionCaller()
	call.UsageObserver = principal.quota
	call.LogCapture = &provider.LogCapturePolicy{Enabled: principal.UserID == 0, Limit: s.o.cfg.UsageContentMaxBytes}
	ctx = s.o.invocationContext(provider.WithInvocation(ctx, call))
	writer := &cancelWriter{ResponseWriter: w, cancel: cancel}
	var output http.ResponseWriter = writer
	if _, ok := w.(http.Flusher); ok {
		output = &flushCancelWriter{writer}
	}
	var customWriter *protocol.CustomToolWriter
	if len(customNames) > 0 || len(namespaceNames) > 0 {
		customWriter = protocol.NewClientToolWriter(output, customNames, namespaceNames, 4*int(s.o.cfg.MaxResponseBytes))
		output = customWriter
	}
	request := provider.ServeRequest{
		Caller:   call.Caller,
		Protocol: proto,
		Payload:  payload,
		Writer:   output,
		AppName:  principal.AppName,
		Headers:  r.Header,
	}
	// Channel normalization must not mutate the body captured by authorization
	// refresh callbacks (in particular, stripping a model namespace).
	request.Payload = make(map[string]any, len(payload))
	for key, value := range payload {
		request.Payload[key] = value
	}
	var report provider.UsageReport
	if native, ok := rt.(provider.NativeSessionObserver); !ok || !native.OwnsSessionObservations() {
		finish := s.o.observations.Begin(rt.Name(), request)
		defer func() { finish(report) }()
	}
	report, err := rt.Serve(ctx, request)
	if customWriter != nil {
		if finishErr := customWriter.Finish(); finishErr != nil && err == nil {
			err = finishErr
			writer.Header().Del("Content-Length")
		}
	}
	if err != nil {
		if report.Status != "incomplete" {
			report.Status = "error"
		}
		report.Error = err.Error()
		if !writer.committed && r.Context().Err() == nil {
			var apiErr *provider.APIError
			if errors.As(err, &apiErr) {
				writeAPIErr(writer, apiErr)
			} else {
				writeAPIErr(writer, errBody(502, "供应商未完成请求，请检查渠道配置后重试", "upstream_error"))
			}
		}
	}
	if result, ok := r.Context().Value(routeAttemptResultKey{}).(*routeAttemptResult); ok {
		result.ok = err == nil && (report.Status == "" || report.Status == "ok")
	}
	if !report.Recorded {
		report.RequestedLimits = requestedLimits
		s.o.logRuntimeUsageDiagnostic(report, string(proto), resolved, t0, principal.AppName, principal.UserID, principal.AppID, requestdiag.UsageDiagnostics(r.Context()), r.Context())
	}
	if !report.Recorded {
		streamwatch.Outcome(r.Context(), report.Status)
	}
	return true
}

// logRuntimeUsage records a usage row for a runtime-served request. Unlike the
// workbuddy path it does not touch the workbuddy pool (runtimes own their own
// accounts), so it writes directly to the usage log with the report's fields.
func (o *Orchestrator) logRuntimeUsage(rep provider.UsageReport, protocol, model string, t0 time.Time, appName string, userID int64, appIDs ...int64) {
	var appID int64
	if len(appIDs) > 0 {
		appID = appIDs[0]
	}
	o.logRuntimeUsageDiagnostic(rep, protocol, model, t0, appName, userID, appID, nil)
}
func (o *Orchestrator) logRuntimeUsageDiagnostic(rep provider.UsageReport, protocol, model string, t0 time.Time, appName string, userID, appID int64, diagnostics *store.UsageDiagnostics, contexts ...context.Context) {
	var ctx context.Context
	if len(contexts) > 0 {
		ctx = contexts[0]
	}
	effort := rep.Effort
	if effort == "" {
		effort = "default"
	}
	status := rep.Status
	if status == "" {
		status = "ok"
	}
	// Enrich a copy: context diagnostics may be shared with routing attempts.
	if diagnostics == nil {
		diagnostics = &store.UsageDiagnostics{}
	} else {
		value := *diagnostics
		diagnostics = &value
	}
	if rep.FinishReason != "" {
		diagnostics.FinishReason = rep.FinishReason
	}
	if rep.RequestedLimits != nil {
		diagnostics.RequestedLimits = rep.RequestedLimits
	}
	if rep.EffectiveLimits != nil {
		diagnostics.EffectiveLimits = rep.EffectiveLimits
	}
	if ctx != nil {
		if timing := streamwatch.TimingFrom(ctx); timing != nil {
			diagnostics.Started = diagnostics.Started || timing.Snapshot().Attempts > 0
		}
	}
	if userID > 0 {
		rep.Input = ""
		rep.Output = ""
		rep.Reasoning = ""
	}
	o.recordUsage(ctx, store.UsageParams{
		Model:               model,
		Diagnostics:         diagnostics,
		Protocol:            protocol,
		AccountUID:          rep.AccountUID,
		InputTokens:         rep.InputTokens,
		TokensKnown:         rep.TokensKnown,
		OutputTokens:        rep.OutputTokens,
		CachedTokens:        rep.CachedTokens,
		ReasoningTokens:     rep.ReasoningTokens,
		CacheCreationTokens: rep.CacheCreationTokens,
		LatencyMs:           float64(time.Since(t0).Milliseconds()),
		Status:              status,
		Error:               rep.Error,
		InputContent:        clipContent(rep.Input, o.cfg.UsageContentMaxBytes),
		OutputContent:       clipContent(rep.Output, o.cfg.UsageContentMaxBytes),
		ReasoningContent:    clipContent(rep.Reasoning, o.cfg.UsageContentMaxBytes),
		Credits:             rep.Credits,
		AppName:             appName,
		UserID:              userID,
		AppID:               appID,
		ReasoningEffort:     effort,
	})
}

// runtimeModels returns the merged namespaced catalog of every ready runtime,
// each entry shaped like the workbuddy models.Registry entries the WebUI and
// /v1/models already consume.
func (o *Orchestrator) runtimeModels(ctx context.Context) []map[string]any {
	return runtimeModelsFrom(ctx, o.runtimes.Runtimes(), nil)
}

func runtimeModelsFrom(ctx context.Context, runtimes []provider.Runtime, include func(string) bool) []map[string]any {
	var out []map[string]any
	for _, rt := range runtimes {
		if !rt.Ready() && rt.Prefix() != "" {
			continue
		}
		for _, m := range rt.Models(ctx) {
			if include != nil {
				id := m.ID
				if extraID, ok := m.Extra["id"]; ok {
					id = str2(extraID)
				}
				if !include(id) {
					continue
				}
			}
			item := map[string]any{
				"id":         m.ID,
				"name":       m.Name,
				"object":     "model",
				"owned_by":   rt.Name(),
				"provider":   rt.Name(),
				"vision":     m.Vision,
				"modalities": m.Modalities,
			}
			if m.MaxOutput > 0 {
				item["max_output_tokens"] = m.MaxOutput
			}
			if m.Context > 0 {
				item["context_window"] = m.Context
				item["context_length"] = m.Context
			}
			for k, v := range m.Extra {
				if k == "account_uids" {
					continue
				}
				item[k] = v
			}
			out = append(out, item)
		}
	}
	return out
}

func (o *Orchestrator) refreshRuntimeModels(ctx context.Context) []string {
	return modelRefreshWarnings(o.refreshModelCatalogs(ctx, ""))
}

// A downstream write failure cancels every provider's upstream context.
type cancelWriter struct {
	http.ResponseWriter
	cancel    context.CancelFunc
	committed bool
}

func (w *cancelWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *cancelWriter) WriteHeader(status int) {
	if status >= 200 {
		w.committed = true
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *cancelWriter) Write(p []byte) (int, error) {
	w.committed = true
	_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(30 * time.Second))
	n, err := w.ResponseWriter.Write(p)
	if err != nil {
		w.cancel()
	}
	return n, err
}

type flushCancelWriter struct{ *cancelWriter }

func (w *flushCancelWriter) Flush() { _ = w.FlushError() }

func (w *flushCancelWriter) FlushError() error {
	w.committed = true
	controller := http.NewResponseController(w.ResponseWriter)
	_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	err := controller.Flush()
	if err != nil {
		w.cancel()
	}
	return err
}

// The legacy model-test endpoint uses the same registry and execution boundary.
func (s *Server) dispatchWorkBuddy(w http.ResponseWriter, r *http.Request, proto provider.Protocol, payload map[string]any, p *Principal) {
	if !s.dispatchRuntimeTo(w, r, proto, payload, p) {
		writeAPIErr(w, errBody(503, "模型渠道未就绪", "model_unavailable"))
	}
}
