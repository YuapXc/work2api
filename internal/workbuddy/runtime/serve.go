package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"work2api/internal/streamwatch"

	"work2api/internal/core/protocol/chatstream"
	"work2api/internal/core/provider"
	"work2api/internal/workbuddy/adapters"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/projection"
	"work2api/internal/workbuddy/upstream"
)

func (o *Runtime) serveChat(w http.ResponseWriter, r *http.Request, payload map[string]any, principal *provider.Caller) {
	clientStream := BoolVal(payload["stream"])
	body := o.EnhanceBody(upstream.BuildUpstreamBody(payload))
	model := StrOr(body["model"], "auto")
	// 会话键从原始 payload 提取（BuildUpstreamBody 已剥掉 prompt_cache_key/metadata）
	sessionKey := RequestSessionKey(r, principal, model, payload)
	sessionCtx, finishSession := o.Sessions.Begin(r.Context(), sessionKey, model, principal, payload)
	r = r.WithContext(sessionCtx)
	defer finishSession()
	acc, aerr := o.pickForCaller(principal, model, sessionKey)
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	input := ""
	if principal.UserID == 0 {
		input = o.ExtractInputText(body)
	}
	effort := UsageReasoningEffort(body)
	t0 := time.Now()

	usage := map[string]any{}
	if clientStream {
		status := "ok"
		out := provider.NewLogCaptureWithPolicy(provider.LogCapturePolicy{Enabled: principal.UserID == 0, Limit: o.cfg.UsageContentMaxBytes})
		reason := provider.NewLogCaptureWithPolicy(provider.LogCapturePolicy{Enabled: principal.UserID == 0, Limit: o.cfg.UsageContentMaxBytes})
		var send func(string) error
		opened := false
		writeChunk := func(chunk string) error {
			if !opened {
				send, _ = streamwatch.SSEWriter(w)
				opened = true
			}
			return send(chunk)
		}
		if _, ok := w.(http.Flusher); !ok {
			writeJSON(w, 500, errBody(500, "streaming unsupported", "server_error").Body)
			return
		}
		sentRole := false
		sink := func(line string) error {
			chunk := upstream.DecodeFrame(r.Context(), line)
			// Observe the original chunk before sanitization drops empty fields.
			usage = MergeUsageChunk(chunk, usage)
			c, rr, finish := DeltaPartsChunk(chunk)
			clean := SanitizeChatChunkWithRole(line, chunk, &sentRole)
			if clean != "" {
				if err := writeChunk(clean + "\n\n"); err != nil {
					return err
				}
			}
			if finish == "length" || finish == "content_filter" {
				status = "incomplete"
			}
			out.WriteString(c)
			reason.WriteString(rr)
			return nil
		}
		served, err := o.OpenUpstreamScoped(r.Context(), acc, body, model, sessionKey, sink, func(fa *pool.Account, e *upstream.UpstreamError) {
			o.LogUsage(LogArgs{Ctx: r.Context(), Protocol: "chat", Model: model, Acc: fa, T0: t0, Status: "error", ErrStr: UpstreamErrorText(e.StatusCode, e.Raw), Input: input, AppName: principal.AppName, UserID: principal.UserID, AppID: principal.AppID, Quota: provider.CurrentInvocation(r.Context()).UsageObserver, UpdatePool: false})
		}, principal.AccountScope)
		if err != nil {
			if !opened {
				setRetryHints(w, err)
				st, detail := ErrToHTTP(err)
				if st == 429 && IsLocalOverload(detail) {
					w.Header().Set("Retry-After", "2")
				}
				o.LogUsage(LogArgs{Ctx: r.Context(), Protocol: "chat", Model: model, Acc: served, T0: t0, Status: "error", ErrStr: err.Error(), Input: input, AppName: principal.AppName, UserID: principal.UserID, AppID: principal.AppID, Quota: provider.CurrentInvocation(r.Context()).UsageObserver, Effort: effort, Usage: usage, UpdatePool: false})
				writeJSON(w, st, detail)
				return
			}
			if ue, ok := err.(*upstream.UpstreamError); ok {
				o.LogUsage(LogArgs{Ctx: r.Context(), Protocol: "chat", Model: model, Acc: served, T0: t0, Status: "error", ErrStr: UpstreamErrorText(ue.StatusCode, ue.Raw), Input: input, AppName: principal.AppName, UserID: principal.UserID, AppID: principal.AppID, Quota: provider.CurrentInvocation(r.Context()).UsageObserver, Effort: effort, Usage: usage, UpdatePool: false})
				writeChunk("data: " + JsonError(ue.StatusCode, string(ue.Raw)) + "\n\n")
			} else {
				o.LogUsage(LogArgs{Ctx: r.Context(), Protocol: "chat", Model: model, Acc: served, T0: t0, Status: "error", ErrStr: err.Error(), Input: input, AppName: principal.AppName, UserID: principal.UserID, AppID: principal.AppID, Quota: provider.CurrentInvocation(r.Context()).UsageObserver, Effort: effort, Usage: usage, UpdatePool: false})
				status, detail := ErrToHTTP(err)
				if body, ok := detail["error"].(map[string]any); ok {
					body["code"] = status
				}
				encoded, _ := json.Marshal(detail)
				writeChunk("data: " + string(encoded) + "\n\n")
			}
			writeChunk("data: [DONE]\n\n")
			return
		}
		o.LogUsage(LogArgs{Ctx: r.Context(), Protocol: "chat", Model: model, Acc: served, T0: t0, Status: status, Usage: usage, Input: input, Output: out.String(), Reasoning: reason.String(), AppName: principal.AppName, UserID: principal.UserID, AppID: principal.AppID, Quota: provider.CurrentInvocation(r.Context()).UsageObserver, Effort: effort, UpdatePool: true})
		return
	}

	var served *pool.Account
	collected, err := upstream.CollectStream(model, func(yield upstream.LineFunc) error {
		var callErr error
		served, callErr = o.OpenUpstreamScoped(r.Context(), acc, body, model, sessionKey, func(line string) error {
			if strings.TrimSpace(line) == "data: [DONE]" {
				return nil
			}
			return yield(line)
		}, func(fa *pool.Account, e *upstream.UpstreamError) {
			// 非流式路径的换号重试同样要留痕，否则连续 failover 无法排查。
			o.LogUsage(LogArgs{Ctx: r.Context(), Protocol: "chat", Model: model, Acc: fa, T0: t0, Status: "error", ErrStr: UpstreamErrorText(e.StatusCode, e.Raw), Input: input, AppName: principal.AppName, UserID: principal.UserID, AppID: principal.AppID, Quota: provider.CurrentInvocation(r.Context()).UsageObserver, UpdatePool: false})
		}, principal.AccountScope)
		return callErr
	})
	if err != nil {
		usage, _ = collected["usage"].(map[string]any)
		setRetryHints(w, err)
		st, detail := ErrToHTTP(err)
		if ue, ok := err.(*upstream.UpstreamError); ok {
			o.LogUsage(LogArgs{Ctx: r.Context(), Protocol: "chat", Model: model, Acc: served, T0: t0, Status: "error", ErrStr: UpstreamErrorText(ue.StatusCode, ue.Raw), Input: input, AppName: principal.AppName, UserID: principal.UserID, AppID: principal.AppID, Quota: provider.CurrentInvocation(r.Context()).UsageObserver, Effort: effort, Usage: usage, UpdatePool: false})
		} else {
			o.LogUsage(LogArgs{Ctx: r.Context(), Protocol: "chat", Model: model, Acc: served, T0: t0, Status: "error", ErrStr: err.Error(), Input: input, AppName: principal.AppName, UserID: principal.UserID, AppID: principal.AppID, Quota: provider.CurrentInvocation(r.Context()).UsageObserver, Effort: effort, Usage: usage, UpdatePool: false})
		}
		if st == 429 && IsLocalOverload(detail) {
			w.Header().Set("Retry-After", "2")
		}
		writeJSON(w, st, detail)
		return
	}
	out, reason, usage := CollectSummary(collected)
	status := "ok"
	choices, _ := collected["choices"].([]any)
	for _, ch := range choices {
		choice, _ := ch.(map[string]any)
		if choice["finish_reason"] == "length" || choice["finish_reason"] == "content_filter" {
			status = "incomplete"
		}
	}
	o.LogUsage(LogArgs{Ctx: r.Context(), Protocol: "chat", Model: model, Acc: served, T0: t0, Status: status, Usage: usage, Input: input, Output: out, Reasoning: reason, AppName: principal.AppName, UserID: principal.UserID, AppID: principal.AppID, Quota: provider.CurrentInvocation(r.Context()).UsageObserver, Effort: effort, UpdatePool: true})
	writeJSON(w, 200, collected)
}

type converter interface {
	CompletionStatus() string
	SetNonstream()
	FeedLine(string) string
	FeedChunk(map[string]any) string
	Finish() string
	GetNonstreamResponse() map[string]any
	ToolsSummaryLimited(int) string
	TextContent() string
	Reasoning() string
	Usage() map[string]any
}

func (o *Runtime) serveConverted(w http.ResponseWriter, r *http.Request, payload map[string]any, principal *provider.Caller, protocol string) {
	if protocol == "anthropic" {
		r = r.WithContext(context.WithValue(r.Context(), messagesRequestKey{}, payload))
		if beta := r.Header.Get("Anthropic-Beta"); beta != "" {
			r = r.WithContext(context.WithValue(r.Context(), messagesBetaKey{}, beta))
		}
	}
	var chatBody map[string]any
	var cvErr error
	if protocol == "anthropic" {
		chatBody, cvErr = adapters.AnthropicRequestToChat(payload)
	} else {
		chatBody, cvErr = adapters.ResponsesRequestToChat(payload)
	}
	if cvErr != nil {
		writeJSON(w, 400, errBody(400, "请求内容无效："+cvErr.Error(), "invalid_request_error").Body)
		return
	}
	// 消息压缩（--optimize-context 语义，OPTIMIZE_CONTEXT 开关，默认关）：只对
	// /v1/responses 生效（与 buddy-proxy 相同的适用面）——Codex CLI 的 agentic
	// 请求常带海量运行时提示/schema/长历史，投影后显著省 token 且降低触发上游
	// 内容审核的概率。挂在 enhanceBody 之前，投影结果再走统一的脱敏/参数增强。
	if protocol == "responses" && o.cfg.OptimizeContext {
		var stats projection.Stats
		chatBody, stats = projection.Body(chatBody)
		log.Printf("[projection] mode=%s msgs %d->%d chars %d->%d tools chars %d->%d",
			stats.Mode, stats.OriginalMessages, stats.ProjectedMessages,
			stats.OriginalMessageChars, stats.ProjectedMessageChars,
			stats.OriginalToolChars, stats.ProjectedToolChars)
	}
	chatBody = o.EnhanceBody(chatBody)
	model := StrOr(chatBody["model"], "auto")
	sessionKey := RequestSessionKey(r, principal, model, payload)
	sessionCtx, finishSession := o.Sessions.Begin(r.Context(), sessionKey, model, principal, payload)
	r = r.WithContext(sessionCtx)
	defer finishSession()
	acc, aerr := o.pickForCaller(principal, model, sessionKey)
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	input := ""
	if principal.UserID == 0 {
		input = o.ExtractInputText(chatBody)
	}
	effort := UsageReasoningEffort(chatBody)
	t0 := time.Now()

	var conv converter
	if protocol == "anthropic" {
		conv = chatstream.NewAnthropicStreamConverter(model)
	} else {
		conv = chatstream.NewResponsesStreamConverter(model)
	}
	clientStream := false
	if v, ok := payload["stream"]; ok {
		clientStream = BoolVal(v)
	}
	_, canStream := w.(http.Flusher)
	streamMode := clientStream && canStream
	if !streamMode {
		conv.SetNonstream()
	} else if messages, ok := conv.(*chatstream.AnthropicStreamConverter); ok {
		messages.SetStreamingLogPolicy(provider.LogCapturePolicy{Enabled: principal.UserID == 0, Limit: o.cfg.UsageContentMaxBytes})
	}
	// Only open the SSE header stream when actually streaming; otherwise the
	// non-stream branches below respond with writeJSON and must own the header.
	var send func(string) error
	opened := false
	writeChunk := func(chunk string) error {
		if !opened {
			send, _ = streamwatch.SSEWriter(w)
			opened = true
		}
		return send(chunk)
	}
	var nativeResult nativeMessageResult
	nativeResponse := false
	sink := func(line string) error {
		chunk := upstream.DecodeFrame(r.Context(), line)
		if event, ok := chunk[upstream.NativeEvent].(map[string]any); ok && protocol == "anthropic" {
			if !nativeResponse {
				if messages, ok := conv.(*chatstream.AnthropicStreamConverter); ok {
					messages.SetObserveOnly(provider.LogCapturePolicy{Enabled: principal.UserID == 0, Limit: o.cfg.UsageContentMaxBytes})
				}
			}
			nativeResponse = true
			if !streamMode {
				nativeResult.observe(event)
			} else {
				raw, err := json.Marshal(event)
				if err != nil {
					return err
				}
				if err := writeChunk("event: " + ToStr(event["type"]) + "\ndata: " + string(raw) + "\n\n"); err != nil {
					return err
				}
			}
		}
		evt := conv.FeedChunk(chunk)
		if nativeResponse {
			return nil
		}
		if evt != "" && streamMode {
			return writeChunk(evt)
		}
		return nil
	}
	served, err := o.OpenUpstreamScoped(r.Context(), acc, chatBody, model, sessionKey, sink, func(fa *pool.Account, e *upstream.UpstreamError) {
		o.LogUsage(LogArgs{Ctx: r.Context(), Protocol: protocol, Model: model, Acc: fa, T0: t0, Status: "error", ErrStr: UpstreamErrorText(e.StatusCode, e.Raw), Input: input, AppName: principal.AppName, UserID: principal.UserID, AppID: principal.AppID, Quota: provider.CurrentInvocation(r.Context()).UsageObserver, UpdatePool: false})
	}, principal.AccountScope)
	if err != nil {
		setRetryHints(w, err)
		st, detail := ErrToHTTP(err)
		errStr := err.Error()
		if ue, ok := err.(*upstream.UpstreamError); ok {
			errStr = UpstreamErrorText(ue.StatusCode, ue.Raw)
		}
		// 账号池处罚已在 openUpstream 统一处理，这里只记日志（UpdatePool:false）。
		o.LogUsage(LogArgs{Ctx: r.Context(), Protocol: protocol, Model: model, Acc: served, T0: t0, Status: "error", ErrStr: errStr, Input: input, AppName: principal.AppName, UserID: principal.UserID, AppID: principal.AppID, Quota: provider.CurrentInvocation(r.Context()).UsageObserver, Effort: effort, Usage: conv.Usage(), UpdatePool: false})
		if streamMode && opened {
			if protocol == "anthropic" {
				writeChunk(ErrAnthropic(st, errStr))
			} else if responses, ok := conv.(*chatstream.ResponsesStreamConverter); ok {
				writeChunk(responses.Fail(errStr, st))
			} else {
				writeChunk("data: " + JsonError(st, errStr) + "\n\n")
			}
			return
		}
		if st == 429 && IsLocalOverload(detail) {
			w.Header().Set("Retry-After", "2")
		}
		writeJSON(w, st, detail)
		return
	}
	var outLog strings.Builder
	if principal.UserID == 0 {
		AppendLogText(&outLog, conv.TextContent(), o.cfg.UsageContentMaxBytes)
		remaining := o.cfg.UsageContentMaxBytes - outLog.Len()
		if o.cfg.UsageContentMaxBytes <= 0 || remaining > 0 {
			AppendLogText(&outLog, conv.ToolsSummaryLimited(remaining), o.cfg.UsageContentMaxBytes)
		}
	}
	out := outLog.String()
	status := "ok"
	if conv.CompletionStatus() == "incomplete" {
		status = "incomplete"
	}
	var deliveryErr error
	defer func() {
		args := LogArgs{Ctx: r.Context(), Protocol: protocol, Model: model, Acc: served, T0: t0, Status: status, Usage: conv.Usage(), Input: input, Output: out, Reasoning: conv.Reasoning(), AppName: principal.AppName, UserID: principal.UserID, AppID: principal.AppID, Quota: provider.CurrentInvocation(r.Context()).UsageObserver, Effort: effort, UpdatePool: true}
		if deliveryErr != nil {
			args.Status = "error"
			args.ErrStr = "客户端响应发送失败：" + deliveryErr.Error()
			args.UpdatePool = false
		}
		o.LogUsage(args)
	}()
	if streamMode {
		if nativeResponse {
			return
		}
		if responses, ok := conv.(*chatstream.ResponsesStreamConverter); ok {
			// Each done/completed event retains its official full payload; only
			// the encoding/writing is incremental to bound transient copies.
			if !opened {
				if err := writeChunk(""); err != nil {
					deliveryErr = err
					return
				}
			}
			controller := http.NewResponseController(w)
			err := responses.FinishEvents(func(event map[string]any) error {
				_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
				if _, err := io.WriteString(w, "data: "); err != nil {
					return err
				}
				if err := json.NewEncoder(w).Encode(event); err != nil {
					return err
				}
				if _, err := io.WriteString(w, "\n"); err != nil {
					return err
				}
				return controller.Flush()
			})
			if err != nil {
				deliveryErr = err
				return
			}
		} else {
			deliveryErr = writeChunk(conv.Finish())
		}
		return
	}
	if nativeResponse {
		writeJSON(w, 200, nativeResult.message)
		return
	}
	writeJSON(w, 200, conv.GetNonstreamResponse())
}
func ErrToHTTP(err error) (int, map[string]any) {
	if errors.Is(err, streamwatch.ErrResponseTooLarge) {
		return 502, errBody(502, "上游响应超过 MAX_RESPONSE_BYTES 限制", "response_too_large").Body
	}
	if ae, ok := err.(*apiError); ok {
		return ae.Status, ae.Body
	}
	if ue, ok := err.(*upstream.UpstreamError); ok {
		return ue.StatusCode, SafeErr(ue.Raw, ue.StatusCode)
	}
	return 502, errBody(502, "upstream error: "+err.Error(), "upstream_error").Body
}

func CollectSummary(collected map[string]any) (out, reason string, usage map[string]any) {
	usage, _ = collected["usage"].(map[string]any)
	choices, _ := collected["choices"].([]any)
	if len(choices) > 0 {
		if ch, ok := choices[0].(map[string]any); ok {
			if msg, ok := ch["message"].(map[string]any); ok {
				out, _ = msg["content"].(string)
				reason, _ = msg["reasoning_content"].(string)
				if tcs, ok := msg["tool_calls"].([]any); ok {
					for _, t := range tcs {
						if tc, ok := t.(map[string]any); ok {
							if fn, ok := tc["function"].(map[string]any); ok {
								out += "<tool_call:" + StrOr(fn["name"], "?") + " " + StrOr(fn["arguments"], "") + ">"
							}
						}
					}
				}
			}
		}
	}
	return out, reason, usage
}

func BoolVal(v any) bool { b, _ := v.(bool); return b }

func StrOr(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

func AppendLogText(b *strings.Builder, text string, limit int) {
	if limit > 0 {
		left := limit - b.Len()
		if left <= 0 {
			return
		}
		if len(text) > left {
			text = text[:left]
		}
	}
	b.WriteString(text)
}

func IsLocalOverload(body map[string]any) bool {
	e, _ := body["error"].(map[string]any)
	return e["type"] == "local_overload"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"error":{"message":"response encoding failed","type":"server_error"}}`)
	}
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
func writeAPIErr(w http.ResponseWriter, e *apiError) { writeJSON(w, e.Status, e.Body) }
func (o *Runtime) pickForCaller(p *provider.Caller, model, key string) (*pool.Account, *apiError) {
	if p != nil && (p.UserID > 0 || p.TestAccount != "") {
		return o.PickAccountExcludingIn(model, key, nil, p.AccountScope)
	}
	return o.PickAccount(model, key)
}
func (o *Runtime) Serve(ctx context.Context, req provider.ServeRequest) (provider.UsageReport, error) {
	if model, _ := o.Catalog.CachedModel(strings.TrimPrefix(ToStr(req.Payload["model"]), "workbuddy/")); model != nil && model["model_type"] != nil {
		return provider.UsageReport{}, errBody(400, "该模型用于媒体生成，请使用图片或视频接口", "invalid_request_error")
	}
	ctx = upstream.WithParsedFrames(ctx)
	caller := req.Caller
	r := (&http.Request{Header: req.Headers}).WithContext(ctx)
	switch req.Protocol {
	case provider.ProtocolChat:
		o.serveChat(req.Writer, r, req.Payload, &caller)
	case provider.ProtocolAnthropic, provider.ProtocolResponses:
		o.serveConverted(req.Writer, r, req.Payload, &caller, string(req.Protocol))
	default:
		return provider.UsageReport{}, provider.ErrUnsupported
	}
	return provider.UsageReport{Recorded: true}, nil
}
