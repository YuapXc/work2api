package provider

import (
	"context"
	"time"
)

// AccountCheck is an observation, never authority. Status valid applies only to
// Scope; a read-only check cannot prove model inference or remove an auth block.
type AccountCheck struct {
	Status    string `json:"status"`
	Scope     string `json:"scope"`
	Message   string `json:"message"`
	CheckedAt int64  `json:"checked_at"`
	ExpiresAt int64  `json:"expires_at"`
	LatencyMS int64  `json:"latency_ms"`
	Model     string `json:"model,omitempty"`
}
type AccountChecker interface {
	CheckAccountIDs() []string
	CheckAccountIdentity(string) string
	CheckAccount(context.Context, string, bool) AccountCheck
}

func Checked(status, scope, message string) AccountCheck {
	return AccountCheck{Status: status, Scope: scope, Message: message, CheckedAt: time.Now().Unix()}
}

type accountCheckAgeKey struct{}

func WithAccountCheckMaxAge(ctx context.Context, age time.Duration) context.Context {
	return context.WithValue(ctx, accountCheckAgeKey{}, age)
}
func AccountCheckMaxAge(ctx context.Context) time.Duration {
	if age, ok := ctx.Value(accountCheckAgeKey{}).(time.Duration); ok && age > 0 {
		return age
	}
	return 30 * time.Minute
}

// AccountIdentityInspector exposes only locally constructed, redacted diagnostics.
type AccountIdentityInspector interface {
	AccountIdentityPreview(string) (map[string]any, error)
}
