package requestdiag

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"work2api/internal/store"
)

type RequestDiagnosticsKey struct{}
type RequestDiagnostics struct{ store.UsageDiagnostics }

func OutputLimits(body map[string]any) map[string]int {
	out := map[string]int{}
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if n, err := strconv.Atoi(limitString(body[key])); err == nil && n > 0 && n <= 1<<30 {
			out[key] = n
		}
	}
	return out
}

func Diagnostic(ctx context.Context) *RequestDiagnostics {
	if ctx == nil {
		return nil
	}
	d, _ := ctx.Value(RequestDiagnosticsKey{}).(*RequestDiagnostics)
	return d
}

func (d *RequestDiagnostics) Observe(line string) {
	if d == nil {
		return
	}
	d.Started = true
	var chunk map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &chunk) != nil {
		return
	}
	d.ObserveChunk(chunk)
}

func (d *RequestDiagnostics) ObserveChunk(chunk map[string]any) {
	if d == nil {
		return
	}
	d.Started = true
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

func UsageDiagnostics(ctx context.Context) *store.UsageDiagnostics {
	if d := Diagnostic(ctx); d != nil {
		value := d.UsageDiagnostics
		return &value
	}
	return nil
}

func limitString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	}
	return ""
}
