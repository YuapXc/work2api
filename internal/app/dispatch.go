package app

import (
	"context"
	"net/http"
	"time"

	"work2api/internal/core/provider"
	"work2api/internal/store"
)

// dispatchRuntime routes a request whose (alias-resolved) model belongs to a
// non-default provider Runtime (qoder/*, opencode/*). It writes the client
// response itself and logs a usage row, then returns true. Returns false when
// the model is not namespaced to any runtime, leaving the default (workbuddy)
// path to handle it.
func (s *Server) dispatchRuntime(w http.ResponseWriter, r *http.Request, proto provider.Protocol, payload map[string]any, principal *Principal) bool {
	rawModel := strOr(payload["model"], "")
	if rawModel == "" {
		return false
	}
	resolved := s.o.resolveModel(rawModel)
	rt, ok := provider.RuntimeForModel(resolved)
	if !ok {
		return false
	}
	// Hand the runtime the alias-resolved, still-namespaced id; the runtime
	// strips its own prefix before talking to its upstream.
	payload["model"] = resolved
	t0 := time.Now()
	report, _ := rt.Serve(r.Context(), provider.ServeRequest{
		Protocol: proto,
		Payload:  payload,
		Writer:   w,
		AppName:  principal.AppName,
	})
	s.o.logRuntimeUsage(report, string(proto), resolved, t0, principal.AppName)
	return true
}

// logRuntimeUsage records a usage row for a runtime-served request. Unlike the
// workbuddy path it does not touch the workbuddy pool (runtimes own their own
// accounts), so it writes directly to the usage log with the report's fields.
func (o *Orchestrator) logRuntimeUsage(rep provider.UsageReport, protocol, model string, t0 time.Time, appName string) {
	effort := rep.Effort
	if effort == "" {
		effort = "default"
	}
	status := rep.Status
	if status == "" {
		status = "ok"
	}
	_ = o.db.LogUsage(store.UsageParams{
		Model:            model,
		Protocol:         protocol,
		AccountUID:       rep.AccountUID,
		InputTokens:      rep.InputTokens,
		OutputTokens:     rep.OutputTokens,
		LatencyMs:        float64(time.Since(t0).Milliseconds()),
		Status:           status,
		Error:            rep.Error,
		InputContent:     rep.Input,
		OutputContent:    rep.Output,
		ReasoningContent: rep.Reasoning,
		Credits:          rep.Credits,
		AppName:          appName,
		ReasoningEffort:  effort,
	})
}

// runtimeModels returns the merged namespaced catalog of every ready runtime,
// each entry shaped like the workbuddy models.Registry entries the WebUI and
// /v1/models already consume.
func (o *Orchestrator) runtimeModels(ctx context.Context) []map[string]any {
	var out []map[string]any
	for _, rt := range provider.Runtimes() {
		if !rt.Ready() {
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
