package app

import (
	"context"
	"net/http"
	"time"

	"work2api/internal/core/provider"
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
	rawModel := strOr(payload["model"], "")
	if rawModel == "" {
		return false
	}
	resolved := s.o.resolveModel(rawModel)
	rt, ok := s.o.runtimes.Resolve(resolved)
	if !ok {
		rt = s.o.wb
		ok = rt != nil
	}
	if !ok {
		return false
	}
	portal, eligible := rt.(provider.PortalProvider)
	if principal.UserID > 0 && (!eligible || !portal.SupportsPortal()) {
		writeAPIErr(w, errBody(403, "当前共享服务仅支持 WorkBuddy 模型", "model_not_allowed"))
		return true
	}
	// Hand the runtime the alias-resolved, still-namespaced id; the runtime
	// strips its own prefix before talking to its upstream.
	payload["model"] = resolved
	t0 := time.Now()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	call := provider.CurrentInvocation(ctx)
	call.Provider = rt.Name()
	call.Caller = principal.SessionCaller()
	call.UsageObserver = principal.quota
	ctx = s.o.invocationContext(provider.WithInvocation(ctx, call))
	writer := &cancelWriter{ResponseWriter: w, cancel: cancel}
	var output http.ResponseWriter = writer
	if _, ok := w.(http.Flusher); ok {
		output = &flushCancelWriter{writer}
	}
	report, err := rt.Serve(ctx, provider.ServeRequest{
		Caller:   call.Caller,
		Protocol: proto,
		Payload:  payload,
		Writer:   output,
		AppName:  principal.AppName,
		Headers:  r.Header,
	})
	if err != nil {
		report.Status, report.Error = "error", err.Error()
		if !writer.committed && r.Context().Err() == nil {
			writeAPIErr(writer, errBody(502, "供应商未完成请求，请检查渠道配置后重试", "upstream_error"))
		}
	}
	if !report.Recorded {
		s.o.logRuntimeUsage(report, string(proto), resolved, t0, principal.AppName, principal.UserID, principal.AppID)
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
	effort := rep.Effort
	if effort == "" {
		effort = "default"
	}
	status := rep.Status
	if status == "" {
		status = "ok"
	}
	var appID int64
	if len(appIDs) > 0 {
		appID = appIDs[0]
	}
	if userID > 0 {
		rep.Input = ""
		rep.Output = ""
		rep.Reasoning = ""
	}
	_ = o.db.LogUsage(store.UsageParams{
		Model:            model,
		Protocol:         protocol,
		AccountUID:       rep.AccountUID,
		InputTokens:      rep.InputTokens,
		TokensKnown:      rep.TokensKnown,
		OutputTokens:     rep.OutputTokens,
		CachedTokens:     rep.CachedTokens,
		LatencyMs:        float64(time.Since(t0).Milliseconds()),
		Status:           status,
		Error:            rep.Error,
		InputContent:     clipContent(rep.Input, o.cfg.UsageContentMaxBytes),
		OutputContent:    clipContent(rep.Output, o.cfg.UsageContentMaxBytes),
		ReasoningContent: clipContent(rep.Reasoning, o.cfg.UsageContentMaxBytes),
		Credits:          rep.Credits,
		AppName:          appName,
		UserID:           userID,
		AppID:            appID,
		ReasoningEffort:  effort,
	})
}

// runtimeModels returns the merged namespaced catalog of every ready runtime,
// each entry shaped like the workbuddy models.Registry entries the WebUI and
// /v1/models already consume.
func (o *Orchestrator) runtimeModels(ctx context.Context) []map[string]any {
	var out []map[string]any
	for _, rt := range o.runtimes.Runtimes() {
		if !rt.Ready() && rt.Prefix() != "" {
			continue
		}
		for _, m := range rt.Models(ctx) {
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
			}
			for k, v := range m.Extra {
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

// benchTarget is one (provider, namespaced id, display name) the benchmarks
// resolver matches against AA.
type benchTarget struct{ provider, id, name string }

// benchTargets is the merged catalog used for AA lookups: workbuddy from the
// read-only model cache (no network on the request path) plus every ready
// runtime's namespaced models.
func (o *Orchestrator) benchTargets(ctx context.Context) []benchTarget {
	var out []benchTarget
	for _, m := range o.runtimeModels(ctx) {
		id := toStrLoose(m["id"])
		if id == "" {
			continue
		}
		out = append(out, benchTarget{toStrLoose(m["provider"]), id, toStrLoose(m["name"])})
	}
	return out
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

func (w *flushCancelWriter) Flush() {
	w.committed = true
	controller := http.NewResponseController(w.ResponseWriter)
	_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if err := controller.Flush(); err != nil {
		w.cancel()
	}
}

// The legacy model-test endpoint uses the same registry and execution boundary.
func (s *Server) dispatchWorkBuddy(w http.ResponseWriter, r *http.Request, proto provider.Protocol, payload map[string]any, p *Principal) {
	if !s.dispatchRuntimeTo(w, r, proto, payload, p) {
		writeAPIErr(w, errBody(503, "模型渠道未就绪", "model_unavailable"))
	}
}
