package app

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"strings"
	"work2api/internal/store"
	"work2api/internal/streamwatch"
	"work2api/internal/workbuddy/upstream"
)

type requestDiagnosticsKey struct{}
type requestDiagnostics struct{ store.UsageDiagnostics }

func diagnosticErrorKind(ctx context.Context, err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, streamwatch.ErrResponseTooLarge) {
		return "response_limit"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
		return "cancelled"
	}
	if isLocalNetworkFailure(err) {
		return "local_network"
	}
	if ue, ok := err.(*upstream.UpstreamError); ok {
		return string(classifyUpstream(ue.StatusCode, ue.Raw, ue.Header))
	}
	if _, ok := err.(*apiError); ok {
		return "gateway"
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "timeout"
	}
	return "transport"
}

func outputLimits(body map[string]any) map[string]int {
	out := map[string]int{}
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if n, err := strconv.Atoi(toStrLoose(body[key])); err == nil && n > 0 && n <= 1<<30 {
			out[key] = n
		}
	}
	return out
}

func diagnostic(ctx context.Context) *requestDiagnostics {
	if ctx == nil {
		return nil
	}
	d, _ := ctx.Value(requestDiagnosticsKey{}).(*requestDiagnostics)
	return d
}

func (d *requestDiagnostics) observe(line string) {
	if d == nil {
		return
	}
	d.Started = true
	var chunk map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &chunk) != nil {
		return
	}
	choices, _ := chunk["choices"].([]any)
	for _, item := range choices {
		choice, _ := item.(map[string]any)
		reason, _ := choice["finish_reason"].(string)
		switch reason {
		case "stop", "tool_calls", "function_call", "length", "content_filter", "aborted", "insufficient_system_resource":
			if d.FinishReason == "" || reason == "length" || reason == "content_filter" {
				d.FinishReason = reason
			}
		}
	}
}

func usageDiagnostics(ctx context.Context) *store.UsageDiagnostics {
	if d := diagnostic(ctx); d != nil {
		value := d.UsageDiagnostics
		return &value
	}
	return nil
}
