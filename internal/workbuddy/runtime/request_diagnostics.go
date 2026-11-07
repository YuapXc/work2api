package runtime

import (
	"context"
	"errors"
	"net"

	"work2api/internal/core/provider"
	"work2api/internal/streamwatch"
	"work2api/internal/workbuddy/upstream"
)

func DiagnosticErrorKind(ctx context.Context, err error) string {
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
	if LocalNetworkFailure(err) {
		return "local_network"
	}
	if ue, ok := err.(*upstream.UpstreamError); ok {
		return string(ClassifyUpstream(ue.StatusCode, ue.Raw, ue.Header))
	}
	if _, ok := err.(*provider.APIError); ok {
		return "gateway"
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "timeout"
	}
	return "transport"
}
