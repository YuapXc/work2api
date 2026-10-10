package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"work2api/internal/core/provider"
	"work2api/internal/streamwatch"
	"work2api/internal/workbuddy/credentials"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/ratelimit"
	"work2api/internal/workbuddy/siterouting"
	"work2api/internal/workbuddy/upstream"
)

// AttemptOptions supplies observations, never response rewriting or authority.
// The core Invocation owns dispatch checks and request-lease coordination.
type AttemptOptions struct {
	Ratelimit           bool
	Interval            float64
	LocalNetworkFailure func(error) bool
	Observe             func(string)
	OnAttempt           func(provider.AccountRef)
}

func (r *Runtime) headers(ctx context.Context, acc *pool.Account, localNetwork func(error) bool) (map[string]string, error) {
	mgr := r.Manager(acc.UID)
	if mgr == nil {
		return nil, &provider.DispatchError{Status: 503, Message: "账号凭据不可用", Type: "auth_error"}
	}
	h, err := mgr.GetChatHeadersContext(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, credentials.ErrLoginRequired) {
			return nil, &upstream.UpstreamError{StatusCode: 401, Raw: []byte(`{"error":{"message":"账号凭据失效，请重新授权","type":"auth_error"}}`)}
		}
		if localNetwork != nil && localNetwork(err) {
			return nil, err
		}
		return nil, &upstream.UpstreamError{StatusCode: 503, Raw: []byte(`{"error":{"message":"账号凭据刷新暂时失败，请稍后重试","type":"upstream_error"}}`)}
	}
	return h, nil
}

func (r *Runtime) Stream(ctx context.Context, acc *pool.Account, body map[string]any, sink func(string) error, opts AttemptOptions) (bool, error) {
	if opts.Ratelimit && !r.Limiter(acc.UID, opts.Interval).Try() {
		if err := provider.Wait(ctx, r.Limiter(acc.UID, opts.Interval).Wait); err != nil {
			if errors.Is(err, ratelimit.ErrQueueFull) {
				return false, &provider.DispatchError{Status: 429, Message: "账号等待队列已满，请稍后重试", Type: "rate_limit_error"}
			}
			return false, err
		}
	}
	ref := provider.AccountRef{Provider: "workbuddy", LocalID: acc.UID}
	model, _ := body["model"].(string)
	if err := provider.CheckDispatch(ctx, ref, model); err != nil {
		return false, err
	}
	headers, err := r.headers(ctx, acc, opts.LocalNetworkFailure)
	if err != nil {
		return false, err
	}
	if err := provider.CheckDispatch(ctx, ref, model); err != nil {
		return false, err
	}
	url, _ := siterouting.ChatURLForProfile(acc.Profile)
	started := false
	var responseBytes int64
	var preamble []string
	preambleBytes := 0
	wrapped := func(line string) error {
		responseBytes += int64(len(line))
		if responseBytes > streamwatch.ResponseLimit(ctx) {
			return streamwatch.ErrResponseTooLarge
		}
		if !started {
			var chunk map[string]any
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			parsed := json.Unmarshal([]byte(data), &chunk) == nil
			terminal := data == "[DONE]"
			if choices, ok := chunk["choices"].([]any); ok {
				for _, value := range choices {
					choice, _ := value.(map[string]any)
					if finish, _ := choice["finish_reason"].(string); finish != "" {
						terminal = true
					}
				}
			}
			if parsed && !terminal && !upstream.HasOutput(chunk) {
				preambleBytes += len(line)
				if preambleBytes > 64*1024 {
					return streamwatch.ErrResponseTooLarge
				}
				preamble = append(preamble, line)
				return nil
			}
			started = true // From here, a writer error also prohibits replay.
			for _, prefix := range preamble {
				if opts.Observe != nil {
					opts.Observe(prefix)
				}
				if err := sink(prefix); err != nil {
					return err
				}
			}
			preamble = nil
		}
		if opts.Observe != nil {
			opts.Observe(line)
		}
		return sink(line)
	}
	client := r.UpstreamClient
	if client == nil {
		client = upstream.Shared()
	}
	if opts.OnAttempt != nil {
		opts.OnAttempt(ref)
	}
	err = client.StreamUpstream(ctx, headers, body, url, wrapped)
	return started, err
}
