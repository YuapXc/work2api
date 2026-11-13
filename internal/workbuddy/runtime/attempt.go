package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"work2api/internal/core/provider"
	"work2api/internal/streamwatch"
	"work2api/internal/toolschema"
	"work2api/internal/workbuddy/credentials"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/ratelimit"
	"work2api/internal/workbuddy/reasoning"
	"work2api/internal/workbuddy/siterouting"
	"work2api/internal/workbuddy/upstream"
)

type recoveryBudgetKey struct{}
type recoveryBudget struct{ remaining int }

func recoveryAvailable(ctx context.Context, slots ...int) bool {
	minimum := 1
	if len(slots) > 0 {
		minimum = slots[0]
	}
	b, _ := ctx.Value(recoveryBudgetKey{}).(*recoveryBudget)
	return b == nil || b.remaining >= minimum
}

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
	if mgr.AuthorizationInvalid() {
		return nil, &upstream.UpstreamError{StatusCode: 401, Raw: []byte(`{"error":{"message":"账号凭据失效，请重新授权","type":"auth_error"}}`)}
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
	shapeKey := acc.Profile + "\x00" + model
	r.toolTailMu.RLock()
	knownShape := r.toolTailModels[shapeKey]
	r.toolTailMu.RUnlock()
	shaped, learnShape := false, false
	if knownShape && nativeMessagesEndpoint(acc.Profile, model) == "" {
		if next, ok := replyAfterToolResults(body); ok {
			body = next
			shaped = true
		}
	}
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
	if tools := toolschema.WorkBuddyToolsForModel(body["tools"], model); reflectToolsChanged(body["tools"], tools) {
		copyBody := make(map[string]any, len(body))
		for k, v := range body {
			copyBody[k] = v
		}
		copyBody["tools"] = tools
		body = copyBody
	}
	url := acc.Endpoint() + "/v2/chat/completions"
	nativeURL := nativeMessagesEndpoint(acc.Profile, model)
	if nativeURL != "" {
		base, _ := siterouting.EndpointForProfile(acc.Profile)
		nativeURL = acc.Endpoint() + strings.TrimPrefix(nativeURL, base)
	}
	var nativeBody map[string]any
	var thinkingOwner string
	if nativeURL != "" {
		if err := upstream.ValidateChoiceCount(body); err != nil {
			return false, err
		}
		nativeBody, err = nativeMessagesBody(ctx, body)
		if err != nil {
			return false, err
		}
		thinkingOwner = fmt.Sprintf("%s\x00%s\x00%s\x00%p", acc.Profile, acc.UID, model, r.Manager(acc.UID))
		nativeBody = r.withoutRefusedThinking(thinkingOwner, nativeBody)
		if body["max_tokens"] == nil && body["max_completion_tokens"] == nil && r.Catalog != nil {
			if limit := r.Catalog.MaxOutputTokens(model); limit > 0 {
				nativeBody["max_tokens"] = limit
			}
		}
	}
	started := false
	var responseBytes int64
	var preamble []string
	preambleBytes := 0
	wrapped := func(line string) error {
		if nativeBody != nil && ctx.Value(messagesRequestKey{}) == nil && ctx.Value(responsesRequestKey{}) == nil {
			chunk := upstream.DecodeFrame(ctx, line)
			if _, exists := chunk[upstream.NativeEvent]; exists {
				return nil // Native-only metadata never crosses the Chat/Responses contract.
			}
		}
		responseBytes += int64(len(line))
		if limit := streamwatch.ResponseLimit(ctx); limit > 0 && responseBytes > limit {
			return streamwatch.ErrResponseTooLarge
		}
		if !started {
			chunk := upstream.DecodeFrame(ctx, line)
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			parsed := chunk != nil
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
	var prepared *upstream.PreparedStreamBody
	authorizationRetried := false
	signatureRetried := false
	emptyTextRetried := false
	imageStage, maxTransmissions := 0, 3
	if nativeBody != nil {
		maxTransmissions = 5
	}
	for retry := 0; retry < maxTransmissions; retry++ {
		applyUpstreamConversation(ctx, acc, headers, model)
		if retry > 0 {
			streamwatch.StartAttempt(ctx)
		}
		if err := provider.CheckDispatch(ctx, ref, model); err != nil {
			return started, err
		}
		if opts.OnAttempt != nil {
			opts.OnAttempt(ref)
		}
		if b, _ := ctx.Value(recoveryBudgetKey{}).(*recoveryBudget); b != nil {
			if b.remaining <= 0 {
				return started, chatAuthorizationError("本次上游恢复已结束，请稍后重试")
			}
			b.remaining--
		}
		normalizer := &toolFinishNormalizer{}
		yield := func(line string) error {
			for _, normalized := range normalizer.lines(ctx, line) {
				if e := wrapped(normalized); e != nil {
					return e
				}
			}
			return nil
		}
		if nativeBody != nil {
			if beta, _ := ctx.Value(messagesBetaKey{}).(string); beta != "" {
				headers["anthropic-beta"] = beta
			}
			nativeClient, ok := client.(interface {
				StreamMessages(context.Context, map[string]string, map[string]any, string, upstream.LineFunc) error
			})
			if !ok {
				return started, errors.New("upstream transport does not support native Messages")
			}
			err = nativeClient.StreamMessages(ctx, headers, nativeBody, nativeURL, wrapped)
		} else if reusable, ok := client.(*upstream.Client); ok {
			if prepared == nil {
				prepared, err = upstream.PrepareStreamBody(body)
				if err != nil {
					return started, err
				}
			}
			err = reusable.StreamPrepared(ctx, headers, prepared, url, yield)
		} else {
			err = client.StreamUpstream(ctx, headers, body, url, yield)
		}
		var rejected *upstream.UpstreamError
		if errors.As(err, &rejected) {
			rejected = rejected.Normalized()
			err = rejected
		}
		if nativeBody == nil && !started && ctx.Err() == nil && rejected != nil && rejected.StatusCode == 400 && BusinessCode(string(rejected.Raw), "11151") && !emptyTextRetried && recoveryAvailable(ctx) {
			if next, changed := reasoning.GuardEmptyTextParts(body, true); changed {
				emptyTextRetried = true
				maxTransmissions++
				streamwatch.AttemptResult(ctx, rejected.StatusCode)
				body, prepared = next, nil
				headers, err = r.headers(ctx, acc, opts.LocalNetworkFailure)
				if err != nil {
					return started, err
				}
				preamble, preambleBytes, responseBytes = nil, 0, 0
				continue
			}
		}
		if nativeBody != nil && !started && ctx.Err() == nil && rejected != nil && rejected.StatusCode == 400 && BusinessCode(string(rejected.Raw), "11135") && imageStage < 2 && recoveryAvailable(ctx) {
			next, fixed, removed := nativeBody, 0, 0
			if imageStage == 0 && recoveryAvailable(ctx, 2) {
				next, fixed, removed = recoverNativeImages(nativeBody, rejected.Raw, false)
				imageStage = 1
			}
			if fixed+removed == 0 {
				next, fixed, removed = recoverNativeImages(nativeBody, rejected.Raw, true)
				imageStage = 2
			}
			if fixed+removed > 0 {
				nativeBody = next
				preamble, preambleBytes, responseBytes = nil, 0, 0
				continue
			}
		}
		if nativeBody != nil && !started && ctx.Err() == nil && rejected != nil && !signatureRetried && recoveryAvailable(ctx) {
			if next, ok := r.recoverBoundThinking(thinkingOwner, nativeBody, rejected); ok {
				nativeBody, signatureRetried = next, true
				preamble, preambleBytes, responseBytes = nil, 0, 0
				continue
			}
		}
		if err == nil && learnShape {
			r.toolTailMu.Lock()
			if r.toolTailModels == nil {
				r.toolTailModels = map[string]bool{}
			}
			if len(r.toolTailModels) >= 256 {
				for key := range r.toolTailModels {
					delete(r.toolTailModels, key)
					break
				}
			}
			r.toolTailModels[shapeKey] = true
			r.toolTailMu.Unlock()
		}
		if nativeBody == nil && !started && ctx.Err() == nil && rejected != nil && rejected.StatusCode == 400 && BusinessCode(string(rejected.Raw), "11153") && !shaped && recoveryAvailable(ctx) {
			if next, ok := replyAfterToolResults(body); ok {
				streamwatch.AttemptResult(ctx, rejected.StatusCode)
				body, prepared = next, nil
				shaped, learnShape = true, true
				headers, err = r.headers(ctx, acc, opts.LocalNetworkFailure)
				if err != nil {
					return started, err
				}
				preamble = nil
				preambleBytes = 0
				responseBytes = 0
				continue
			}
		}
		if started || ctx.Err() != nil || !errors.As(err, &rejected) || rejected.StatusCode != 401 {
			return started, err
		}
		if authorizationRetried {
			return started, chatAuthorizationError("凭据续期后仍未通过调用鉴权，请重新授权或核实账号权限")
		}

		if !recoveryAvailable(ctx) {
			return started, chatAuthorizationError("上游鉴权未通过，本次恢复已结束，请核实账号授权")
		}
		authorizationRetried = true
		streamwatch.AttemptResult(ctx, 401)
		mgr := r.Manager(acc.UID)
		if mgr == nil {
			return started, err
		}
		if refreshErr := mgr.RecoverChatAuthorization(ctx, headers["Authorization"]); refreshErr != nil {
			if ctx.Err() != nil {
				return started, ctx.Err()
			}
			if errors.Is(refreshErr, credentials.ErrLoginRequired) {
				return started, chatAuthorizationError("账号登录态失效，请重新授权")
			}
			return started, chatAuthorizationError("凭据暂时续期失败，请稍后重试")
		}
		headers, err = r.headers(ctx, acc, opts.LocalNetworkFailure)
		if err != nil {
			return started, err
		}
		preamble = nil
		preambleBytes = 0
		responseBytes = 0
	}
	return started, err
}

func replyAfterToolResults(body map[string]any) (map[string]any, bool) {
	messages, _ := body["messages"].([]any)
	i := len(messages) - 1
	for i >= 0 {
		message, _ := messages[i].(map[string]any)
		if message["role"] != "user" {
			break
		}
		i--
	}
	if i < 0 || i == len(messages)-1 {
		return body, false
	}
	message, _ := messages[i].(map[string]any)
	if message["role"] != "tool" {
		return body, false
	}
	next := make([]any, 0, len(messages)+1)
	next = append(next, messages[:i+1]...)
	next = append(next, map[string]any{"role": "assistant", "content": ""})
	next = append(next, messages[i+1:]...)
	out := maps.Clone(body)
	out["messages"] = next
	return out, true
}

func reflectToolsChanged(a, b any) bool {
	x, ok := a.([]any)
	y, ok2 := b.([]any)
	return ok && ok2 && reflect.ValueOf(x).Pointer() != reflect.ValueOf(y).Pointer()
}

func chatAuthorizationError(message string) *upstream.UpstreamError {
	raw, _ := json.Marshal(map[string]any{"error": map[string]any{"message": message, "type": "auth_error"}})
	return &upstream.UpstreamError{StatusCode: 401, Raw: raw}
}
