package app

import (
	"net/http"
	"time"

	"work2api/internal/core/provider"
	"work2api/internal/workbuddy/adapters"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/upstream"
)

// sseWriter sets SSE headers and returns a flush-writer.
func sseWriter(w http.ResponseWriter) (func(string), bool) {
	fl, ok := w.(http.Flusher)
	if !ok {
		return nil, false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl.Flush()
	return func(s string) {
		_, _ = w.Write([]byte(s))
		fl.Flush()
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
	// Namespaced models (qoder/*, opencode/*) are served by their own provider
	// runtime, which owns conversion + upstream; the workbuddy path below only
	// handles the default (un-namespaced) provider.
	if s.dispatchRuntime(w, r, provider.ProtocolChat, payload, principal) {
		return
	}
	clientStream := boolVal(payload["stream"])
	body := s.o.enhanceBody(upstream.BuildUpstreamBody(payload))
	model := strOr(body["model"], "auto")
	// 会话键从原始 payload 提取（BuildUpstreamBody 已剥掉 prompt_cache_key/metadata）
	sessionKey := extractSessionKey(payload)
	acc, aerr := s.o.pickAccount(model, sessionKey)
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
		var out, reason string
		writeChunk, ok := sseWriter(w)
		if !ok {
			writeJSON(w, 500, errBody(500, "streaming unsupported", "server_error").body)
			return
		}
		sink := func(line string) error {
			clean := sanitizeChatSSE(line)
			if clean != "" {
				writeChunk(clean + "\n\n")
			}
			usage = mergeUsageFromLine(line, usage)
			c, rr := deltaParts(line)
			out += c
			reason += rr
			return nil
		}
		served, err := o.openUpstream(r.Context(), acc, body, model, sessionKey, sink, func(fa *pool.Account, e *upstream.UpstreamError) {
			o.logUsage(logArgs{protocol: "chat", model: model, acc: fa, t0: t0, status: "error", errStr: upstreamErrorText(e.StatusCode, e.Raw), input: input, appName: principal.AppName, updatePool: false})
		})
		if err != nil {
			if ue, ok := err.(*upstream.UpstreamError); ok {
				o.logUsage(logArgs{protocol: "chat", model: model, acc: served, t0: t0, status: "error", errStr: upstreamErrorText(ue.StatusCode, ue.Raw), input: input, appName: principal.AppName, effort: effort, updatePool: false})
				writeChunk("data: " + jsonError(ue.StatusCode, string(ue.Raw)) + "\n\n")
			} else {
				o.logUsage(logArgs{protocol: "chat", model: model, acc: served, t0: t0, status: "error", errStr: err.Error(), input: input, appName: principal.AppName, effort: effort, updatePool: false})
				writeChunk("data: " + jsonError(502, err.Error()) + "\n\n")
			}
			writeChunk("data: [DONE]\n\n")
			return
		}
		o.logUsage(logArgs{protocol: "chat", model: model, acc: served, t0: t0, status: "ok", usage: usage, input: input, output: out, reasoning: reason, appName: principal.AppName, effort: effort, updatePool: true})
		return
	}

	var lines []string
	sink := func(line string) error { lines = append(lines, line); return nil }
	served, err := o.openUpstream(r.Context(), acc, body, model, sessionKey, sink, nil)
	if err != nil {
		st, detail := errToHTTP(err)
		if ue, ok := err.(*upstream.UpstreamError); ok {
			o.logUsage(logArgs{protocol: "chat", model: model, acc: served, t0: t0, status: "error", errStr: upstreamErrorText(ue.StatusCode, ue.Raw), input: input, appName: principal.AppName, effort: effort, updatePool: false})
		} else {
			o.logUsage(logArgs{protocol: "chat", model: model, acc: served, t0: t0, status: "error", errStr: err.Error(), input: input, appName: principal.AppName, effort: effort, updatePool: false})
		}
		writeJSON(w, st, detail)
		return
	}
	collected, cerr := upstream.CollectStream(model, func(yield upstream.LineFunc) error {
		for _, l := range lines {
			if e := yield(l); e != nil {
				return e
			}
		}
		return nil
	})
	if cerr != nil {
		writeJSON(w, 502, errBody(502, cerr.Error(), "upstream_error").body)
		return
	}
	out, reason, usage := collectSummary(collected)
	o.logUsage(logArgs{protocol: "chat", model: model, acc: served, t0: t0, status: "ok", usage: usage, input: input, output: out, reasoning: reason, appName: principal.AppName, effort: effort, updatePool: true})
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
	FeedLine(string) string
	Finish() string
	GetNonstreamResponse() map[string]any
	ToolsSummary() string
	TextContent() string
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
	chatBody = o.enhanceBody(chatBody)
	model := strOr(chatBody["model"], "auto")
	sessionKey := extractSessionKey(payload)
	acc, aerr := o.pickAccount(model, sessionKey)
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
	clientStream := true
	if v, ok := payload["stream"]; ok {
		clientStream = boolVal(v)
	}
	_, canStream := w.(http.Flusher)
	streamMode := clientStream && canStream
	// Only open the SSE header stream when actually streaming; otherwise the
	// non-stream branches below respond with writeJSON and must own the header.
	var writeChunk func(string)
	if streamMode {
		writeChunk, _ = sseWriter(w)
	}
	var reason string
	sink := func(line string) error {
		evt := conv.FeedLine(line)
		_, rr := deltaParts(line)
		reason += rr
		if evt != "" && streamMode {
			writeChunk(evt)
		}
		return nil
	}
	served, err := o.openUpstream(r.Context(), acc, chatBody, model, sessionKey, sink, func(fa *pool.Account, e *upstream.UpstreamError) {
		o.logUsage(logArgs{protocol: protocol, model: model, acc: fa, t0: t0, status: "error", errStr: upstreamErrorText(e.StatusCode, e.Raw), input: input, appName: principal.AppName, updatePool: false})
	})
	if err != nil {
		st, detail := errToHTTP(err)
		errStr := err.Error()
		if ue, ok := err.(*upstream.UpstreamError); ok {
			errStr = upstreamErrorText(ue.StatusCode, ue.Raw)
		}
		// 账号池处罚已在 openUpstream 统一处理，这里只记日志（updatePool:false）。
		o.logUsage(logArgs{protocol: protocol, model: model, acc: served, t0: t0, status: "error", errStr: errStr, input: input, appName: principal.AppName, effort: effort, updatePool: false})
		if streamMode {
			if protocol == "anthropic" {
				writeChunk(errAnthropic(st, errStr))
			} else {
				writeChunk("data: " + jsonError(st, errStr) + "\n\n")
			}
			return
		}
		writeJSON(w, st, detail)
		return
	}
	finish := conv.Finish()
	out := conv.TextContent() + conv.ToolsSummary()
	o.logUsage(logArgs{protocol: protocol, model: model, acc: served, t0: t0, status: "ok", usage: conv.Usage(), input: input, output: out, reasoning: reason, appName: principal.AppName, effort: effort, updatePool: true})
	if streamMode {
		writeChunk(finish)
		return
	}
	writeJSON(w, 200, conv.GetNonstreamResponse())
}

func errToHTTP(err error) (int, map[string]any) {
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
