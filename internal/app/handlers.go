package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
	"work2api/internal/streamwatch"

	"work2api/internal/core/provider"
	"work2api/internal/workbuddy/adapters"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/projection"
	"work2api/internal/workbuddy/upstream"
)

// sseWriter sets SSE headers and returns a flush-writer.
func sseWriter(w http.ResponseWriter) (func(string) error, bool) {
	_, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	w.WriteHeader(http.StatusOK)
	return func(s string) error {
		controller := http.NewResponseController(w)
		_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := w.Write([]byte(s)); err != nil {
			return errors.Join(context.Canceled, err)
		}
		if err := controller.Flush(); err != nil {
			return errors.Join(context.Canceled, err)
		}
		return nil
	}, true
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	principal, aerr := s.auth(r)
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	payload, err := readJSON(r)
	if err != nil {
		writeJSON(w, 400, errBody(400, "bad json", "invalid_request_error").body)
		return
	}
	if _, ok := payload["messages"]; !ok {
		writeJSON(w, 400, errBody(400, "messages is required", "invalid_request_error").body)
		return
	}
	if aerr := s.prepareModel(principal, payload); aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), requestStreamKey{}, boolVal(payload["stream"])))
	// Observe authenticated, model-authorized WorkBuddy requests before queueing.
	// The later protocol path refreshes scope without counting this call twice.
	finishSession := func() {}
	if _, external := provider.RuntimeForModel(principal.EffectiveModel); !external {
		var sessionCtx context.Context
		sessionCtx, finishSession = s.o.sessions.begin(r.Context(), principalSessionKey(principal, principal.EffectiveModel, payload), principal.EffectiveModel, principal, payload)
		r = r.WithContext(sessionCtx)
	}
	defer finishSession()
	r, release, admitted := s.admitModel(w, r, principal)
	if !admitted {
		return
	}
	defer release()
	var refreshed bool
	r, refreshed = s.refreshPortalRequest(w, r, principal, payload)
	if !refreshed {
		return
	}
	if !s.reservePortalBudget(w, principal, payload) {
		return
	}
	defer principal.quota.settle()
	if s.dispatchRuntime(w, r, provider.ProtocolChat, payload, principal) {
		return
	}
	s.runChatPath(w, r, payload, principal)
}

// runChatPath is the default (workbuddy) chat path with an injectable writer,
// so the model test endpoint can capture the response with a recorder.
func (s *Server) runChatPath(w http.ResponseWriter, r *http.Request, payload map[string]any, principal *Principal) {
	clientStream := boolVal(payload["stream"])
	body := s.o.enhanceBody(upstream.BuildUpstreamBody(payload))
	model := strOr(body["model"], "auto")
	// 会话键从原始 payload 提取（BuildUpstreamBody 已剥掉 prompt_cache_key/metadata）
	sessionKey := principalSessionKey(principal, model, payload)
	sessionCtx, finishSession := s.o.sessions.begin(r.Context(), sessionKey, model, principal, payload)
	r = r.WithContext(sessionCtx)
	defer finishSession()
	acc, aerr := s.pickAccountFor(principal, model, sessionKey)
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	input := s.o.extractInputText(body)
	effort := usageReasoningEffort(body)
	t0 := time.Now()
	o := s.o

	if clientStream {
		usage := map[string]any{}
		status := "ok"
		var out, reason strings.Builder
		var send func(string) error
		opened := false
		writeChunk := func(chunk string) error {
			if !opened {
				send, _ = sseWriter(w)
				opened = true
			}
			return send(chunk)
		}
		if _, ok := w.(http.Flusher); !ok {
			writeJSON(w, 500, errBody(500, "streaming unsupported", "server_error").body)
			return
		}
		sink := func(line string) error {
			clean := sanitizeChatSSE(line)
			if clean != "" {
				if err := writeChunk(clean + "\n\n"); err != nil {
					return err
				}
			}
			usage = mergeUsageFromLine(line, usage)
			c, rr, finish := deltaParts(line)
			if finish == "length" || finish == "content_filter" {
				status = "incomplete"
			}
			appendLogText(&out, c, o.cfg.UsageContentMaxBytes)
			appendLogText(&reason, rr, o.cfg.UsageContentMaxBytes)
			return nil
		}
		served, err := o.openUpstreamScoped(r.Context(), acc, body, model, sessionKey, sink, func(fa *pool.Account, e *upstream.UpstreamError) {
			o.logUsage(logArgs{ctx: r.Context(), protocol: "chat", model: model, acc: fa, t0: t0, status: "error", errStr: upstreamErrorText(e.StatusCode, e.Raw), input: input, appName: principal.AppName, userID: principal.UserID, appID: principal.AppID, quota: principal.quota, updatePool: false})
		}, principal.AccountScope)
		if err != nil {
			if !opened {
				st, detail := errToHTTP(err)
				if st == 429 && isLocalOverload(detail) {
					w.Header().Set("Retry-After", "2")
				}
				o.logUsage(logArgs{ctx: r.Context(), protocol: "chat", model: model, acc: served, t0: t0, status: "error", errStr: err.Error(), input: input, appName: principal.AppName, userID: principal.UserID, appID: principal.AppID, quota: principal.quota, effort: effort, updatePool: false})
				writeJSON(w, st, detail)
				return
			}
			if ue, ok := err.(*upstream.UpstreamError); ok {
				o.logUsage(logArgs{ctx: r.Context(), protocol: "chat", model: model, acc: served, t0: t0, status: "error", errStr: upstreamErrorText(ue.StatusCode, ue.Raw), input: input, appName: principal.AppName, userID: principal.UserID, appID: principal.AppID, quota: principal.quota, effort: effort, updatePool: false})
				writeChunk("data: " + jsonError(ue.StatusCode, string(ue.Raw)) + "\n\n")
			} else {
				o.logUsage(logArgs{ctx: r.Context(), protocol: "chat", model: model, acc: served, t0: t0, status: "error", errStr: err.Error(), input: input, appName: principal.AppName, userID: principal.UserID, appID: principal.AppID, quota: principal.quota, effort: effort, updatePool: false})
				status, detail := errToHTTP(err)
				if body, ok := detail["error"].(map[string]any); ok {
					body["code"] = status
				}
				encoded, _ := json.Marshal(detail)
				writeChunk("data: " + string(encoded) + "\n\n")
			}
			writeChunk("data: [DONE]\n\n")
			return
		}
		o.logUsage(logArgs{ctx: r.Context(), protocol: "chat", model: model, acc: served, t0: t0, status: status, usage: usage, input: input, output: out.String(), reasoning: reason.String(), appName: principal.AppName, userID: principal.UserID, appID: principal.AppID, quota: principal.quota, effort: effort, updatePool: true})
		return
	}

	var served *pool.Account
	collected, err := upstream.CollectStream(model, func(yield upstream.LineFunc) error {
		var callErr error
		served, callErr = o.openUpstreamScoped(r.Context(), acc, body, model, sessionKey, func(line string) error {
			if strings.TrimSpace(line) == "data: [DONE]" {
				return nil
			}
			return yield(line)
		}, func(fa *pool.Account, e *upstream.UpstreamError) {
			// 非流式路径的换号重试同样要留痕，否则连续 failover 无法排查。
			o.logUsage(logArgs{ctx: r.Context(), protocol: "chat", model: model, acc: fa, t0: t0, status: "error", errStr: upstreamErrorText(e.StatusCode, e.Raw), input: input, appName: principal.AppName, userID: principal.UserID, appID: principal.AppID, quota: principal.quota, updatePool: false})
		}, principal.AccountScope)
		return callErr
	})
	if err != nil {
		st, detail := errToHTTP(err)
		if ue, ok := err.(*upstream.UpstreamError); ok {
			o.logUsage(logArgs{ctx: r.Context(), protocol: "chat", model: model, acc: served, t0: t0, status: "error", errStr: upstreamErrorText(ue.StatusCode, ue.Raw), input: input, appName: principal.AppName, userID: principal.UserID, appID: principal.AppID, quota: principal.quota, effort: effort, updatePool: false})
		} else {
			o.logUsage(logArgs{ctx: r.Context(), protocol: "chat", model: model, acc: served, t0: t0, status: "error", errStr: err.Error(), input: input, appName: principal.AppName, userID: principal.UserID, appID: principal.AppID, quota: principal.quota, effort: effort, updatePool: false})
		}
		if st == 429 && isLocalOverload(detail) {
			w.Header().Set("Retry-After", "2")
		}
		writeJSON(w, st, detail)
		return
	}
	out, reason, usage := collectSummary(collected)
	status := "ok"
	choices, _ := collected["choices"].([]any)
	for _, ch := range choices {
		choice, _ := ch.(map[string]any)
		if choice["finish_reason"] == "length" || choice["finish_reason"] == "content_filter" {
			status = "incomplete"
		}
	}
	o.logUsage(logArgs{ctx: r.Context(), protocol: "chat", model: model, acc: served, t0: t0, status: status, usage: usage, input: input, output: out, reasoning: reason, appName: principal.AppName, userID: principal.UserID, appID: principal.AppID, quota: principal.quota, effort: effort, updatePool: true})
	writeJSON(w, 200, collected)
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	s.handleConverted(w, r, "anthropic")
}

func (s *Server) handleResponses(w http.ResponseWriter, r *http.Request) {
	s.handleConverted(w, r, "responses")
}

// converter is the shared interface of the two stream converters.
type converter interface {
	CompletionStatus() string
	SetNonstream()
	FeedLine(string) string
	Finish() string
	GetNonstreamResponse() map[string]any
	ToolsSummaryLimited(int) string
	TextContent() string
	Reasoning() string
	Usage() map[string]any
}

var _ = adapters.NewAnthropicStreamConverter

func (s *Server) handleConverted(w http.ResponseWriter, r *http.Request, protocol string) {
	principal, aerr := s.auth(r)
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	payload, err := readJSON(r)
	if err != nil {
		writeJSON(w, 400, errBody(400, "bad json", "invalid_request_error").body)
		return
	}
	if aerr := s.prepareModel(principal, payload); aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	streamHint := false
	if value, ok := payload["stream"]; ok {
		streamHint = boolVal(value)
	}
	r = r.WithContext(context.WithValue(r.Context(), requestStreamKey{}, streamHint))
	// Observe authenticated, model-authorized WorkBuddy requests before queueing.
	// The later protocol path refreshes scope without counting this call twice.
	finishSession := func() {}
	if _, external := provider.RuntimeForModel(principal.EffectiveModel); !external {
		var sessionCtx context.Context
		sessionCtx, finishSession = s.o.sessions.begin(r.Context(), principalSessionKey(principal, principal.EffectiveModel, payload), principal.EffectiveModel, principal, payload)
		r = r.WithContext(sessionCtx)
	}
	defer finishSession()
	r, release, admitted := s.admitModel(w, r, principal)
	if !admitted {
		return
	}
	defer release()
	var refreshed bool
	r, refreshed = s.refreshPortalRequest(w, r, principal, payload)
	if !refreshed {
		return
	}
	if !s.reservePortalBudget(w, principal, payload) {
		return
	}
	defer principal.quota.settle()
	o := s.o
	// Route namespaced models to their provider runtime (it does its own
	// protocol conversion from the original anthropic/responses payload).
	proto := provider.ProtocolAnthropic
	if protocol == "responses" {
		proto = provider.ProtocolResponses
	}
	if s.dispatchRuntime(w, r, proto, payload, principal) {
		return
	}
	var chatBody map[string]any
	var cvErr error
	if protocol == "anthropic" {
		chatBody, cvErr = adapters.AnthropicRequestToChat(payload)
	} else {
		chatBody, cvErr = adapters.ResponsesRequestToChat(payload)
	}
	if cvErr != nil {
		writeJSON(w, 400, errBody(400, "请求内容无效："+cvErr.Error(), "invalid_request_error").body)
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
	chatBody = o.enhanceBody(chatBody)
	model := strOr(chatBody["model"], "auto")
	sessionKey := principalSessionKey(principal, model, payload)
	sessionCtx, finishSession := s.o.sessions.begin(r.Context(), sessionKey, model, principal, payload)
	r = r.WithContext(sessionCtx)
	defer finishSession()
	acc, aerr := s.pickAccountFor(principal, model, sessionKey)
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	input := o.extractInputText(chatBody)
	effort := usageReasoningEffort(chatBody)
	t0 := time.Now()

	var conv converter
	if protocol == "anthropic" {
		conv = adapters.NewAnthropicStreamConverter(model)
	} else {
		conv = adapters.NewResponsesStreamConverter(model)
	}
	clientStream := false
	if v, ok := payload["stream"]; ok {
		clientStream = boolVal(v)
	}
	_, canStream := w.(http.Flusher)
	streamMode := clientStream && canStream
	if !streamMode {
		conv.SetNonstream()
	}
	// Only open the SSE header stream when actually streaming; otherwise the
	// non-stream branches below respond with writeJSON and must own the header.
	var send func(string) error
	opened := false
	writeChunk := func(chunk string) error {
		if !opened {
			send, _ = sseWriter(w)
			opened = true
		}
		return send(chunk)
	}
	sink := func(line string) error {
		evt := conv.FeedLine(line)
		if evt != "" && streamMode {
			return writeChunk(evt)
		}
		return nil
	}
	served, err := o.openUpstreamScoped(r.Context(), acc, chatBody, model, sessionKey, sink, func(fa *pool.Account, e *upstream.UpstreamError) {
		o.logUsage(logArgs{ctx: r.Context(), protocol: protocol, model: model, acc: fa, t0: t0, status: "error", errStr: upstreamErrorText(e.StatusCode, e.Raw), input: input, appName: principal.AppName, userID: principal.UserID, appID: principal.AppID, quota: principal.quota, updatePool: false})
	}, principal.AccountScope)
	if err != nil {
		st, detail := errToHTTP(err)
		errStr := err.Error()
		if ue, ok := err.(*upstream.UpstreamError); ok {
			errStr = upstreamErrorText(ue.StatusCode, ue.Raw)
		}
		// 账号池处罚已在 openUpstream 统一处理，这里只记日志（updatePool:false）。
		o.logUsage(logArgs{ctx: r.Context(), protocol: protocol, model: model, acc: served, t0: t0, status: "error", errStr: errStr, input: input, appName: principal.AppName, userID: principal.UserID, appID: principal.AppID, quota: principal.quota, effort: effort, updatePool: false})
		if streamMode && opened {
			if protocol == "anthropic" {
				writeChunk(errAnthropic(st, errStr))
			} else if responses, ok := conv.(*adapters.ResponsesStreamConverter); ok {
				writeChunk(responses.Fail(errStr, st))
			} else {
				writeChunk("data: " + jsonError(st, errStr) + "\n\n")
			}
			return
		}
		if st == 429 && isLocalOverload(detail) {
			w.Header().Set("Retry-After", "2")
		}
		writeJSON(w, st, detail)
		return
	}
	var outLog strings.Builder
	appendLogText(&outLog, conv.TextContent(), o.cfg.UsageContentMaxBytes)
	remaining := o.cfg.UsageContentMaxBytes - outLog.Len()
	if o.cfg.UsageContentMaxBytes <= 0 || remaining > 0 {
		appendLogText(&outLog, conv.ToolsSummaryLimited(remaining), o.cfg.UsageContentMaxBytes)
	}
	out := outLog.String()
	status := "ok"
	if conv.CompletionStatus() == "incomplete" {
		status = "incomplete"
	}
	o.logUsage(logArgs{ctx: r.Context(), protocol: protocol, model: model, acc: served, t0: t0, status: status, usage: conv.Usage(), input: input, output: out, reasoning: conv.Reasoning(), appName: principal.AppName, userID: principal.UserID, appID: principal.AppID, quota: principal.quota, effort: effort, updatePool: true})
	if streamMode {
		if responses, ok := conv.(*adapters.ResponsesStreamConverter); ok {
			// Each done/completed event retains its official full payload; only
			// the encoding/writing is incremental to bound transient copies.
			if !opened {
				if err := writeChunk(""); err != nil {
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
				return
			}
		} else {
			_ = writeChunk(conv.Finish())
		}
		return
	}
	writeJSON(w, 200, conv.GetNonstreamResponse())
}

func errToHTTP(err error) (int, map[string]any) {
	if errors.Is(err, streamwatch.ErrResponseTooLarge) {
		return 502, errBody(502, "上游响应超过 MAX_RESPONSE_BYTES 限制", "response_too_large").body
	}
	if ae, ok := err.(*apiError); ok {
		return ae.status, ae.body
	}
	if ue, ok := err.(*upstream.UpstreamError); ok {
		return ue.StatusCode, safeErr(ue.Raw, ue.StatusCode)
	}
	return 502, errBody(502, "upstream error: "+err.Error(), "upstream_error").body
}

func collectSummary(collected map[string]any) (out, reason string, usage map[string]any) {
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
								out += "<tool_call:" + strOr(fn["name"], "?") + " " + strOr(fn["arguments"], "") + ">"
							}
						}
					}
				}
			}
		}
	}
	return out, reason, usage
}

func boolVal(v any) bool { b, _ := v.(bool); return b }

func strOr(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

func appendLogText(b *strings.Builder, text string, limit int) {
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

func isLocalOverload(body map[string]any) bool {
	e, _ := body["error"].(map[string]any)
	return e["type"] == "local_overload"
}
