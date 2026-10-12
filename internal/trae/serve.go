package trae

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"work2api/internal/core/protocol"
	"work2api/internal/core/provider"
	"work2api/internal/streamwatch"
	"work2api/internal/trae/auth"
	"work2api/internal/trae/upstream"
)

type eventReader struct {
	scanner *bufio.Scanner
	event   string
	data    []string
}

func newEvents(reader io.Reader) *eventReader {
	s := bufio.NewScanner(reader)
	s.Buffer(make([]byte, 4096), streamwatch.DefaultMaxLineBytes)
	return &eventReader{scanner: s}
}
func (r *eventReader) next() (*upstream.SOLOEvent, error) {
	for r.scanner.Scan() {
		line := r.scanner.Text()
		if line == "" {
			if len(r.data) > 0 {
				name, data := r.event, strings.Join(r.data, "\n")
				r.event = ""
				r.data = nil
				return upstream.ParseSOLOLine(name, data)
			}
			r.event = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			r.event = strings.TrimSpace(line[6:])
		} else if strings.HasPrefix(line, "data:") {
			r.data = append(r.data, strings.TrimSpace(line[5:]))
		}
	}
	if err := r.scanner.Err(); err != nil {
		return nil, err
	}
	if len(r.data) > 0 {
		name, data := r.event, strings.Join(r.data, "\n")
		r.event = ""
		r.data = nil
		return upstream.ParseSOLOLine(name, data)
	}
	return nil, io.ErrUnexpectedEOF
}
func eventError(e *upstream.SOLOEvent) error {
	return &upstream.SOLOStreamError{Code: e.ErrorCode, Msg: e.ErrorMessage}
}

func (r *Runtime) Serve(ctx context.Context, req provider.ServeRequest) (report provider.UsageReport, result error) {
	report.Status = "error"
	body, err := protocol.ConvertRequest(protocol.Protocol(req.Protocol), protocol.Chat, req.Payload)
	if err != nil {
		return report, provider.NewAPIError(400, "请求无法转换为 Trae 协议", "invalid_request_error")
	}
	model := strings.TrimPrefix(text(body["model"]), "trae/")
	if model == "" || model == "auto" {
		return report, provider.NewAPIError(400, "请选择具体 Trae 模型", "invalid_request_error")
	}
	if n, present := body["n"]; present && number(n) != 1 {
		return report, provider.NewAPIError(400, "Trae 当前仅支持 n=1", "invalid_request_error")
	}
	if body["max_tokens"] == nil && body["max_completion_tokens"] != nil {
		body["max_tokens"] = body["max_completion_tokens"]
	}
	if body["response_format"] != nil {
		return report, provider.NewAPIError(400, "该 Trae 路径尚未验证结构化输出，请移除 response_format", "unsupported_parameter")
	}
	toolNames := map[string]bool{}
	if raw := body["tools"]; raw != nil {
		tools, ok := raw.([]any)
		if !ok {
			return report, provider.NewAPIError(400, "tools 必须是工具定义数组", "invalid_request_error")
		}
		for _, item := range tools {
			tool, valid := item.(map[string]any)
			if !valid || tool["type"] != "function" {
				return report, provider.NewAPIError(400, "Trae 原生路径只支持调用方 function 工具", "unsupported_tool")
			}
			fn, ok := tool["function"].(map[string]any)
			if !ok || text(fn["name"]) == "" {
				return report, provider.NewAPIError(400, "工具缺少函数定义", "invalid_request_error")
			}
			toolNames[text(fn["name"])] = true
		}
	}
	if choice := body["tool_choice"]; choice != nil {
		valid := false
		switch value := choice.(type) {
		case string:
			valid = value == "auto" || value == "none" || value == "required" && len(toolNames) > 0
		case map[string]any:
			fn, _ := value["function"].(map[string]any)
			valid = value["type"] == "function" && toolNames[text(fn["name"])]
		}
		if !valid {
			return report, provider.NewAPIError(400, "tool_choice 必须为 auto、none、required 或已定义的 function", "invalid_request_error")
		}
	}
	body["model"] = model
	image := hasImageInput(body)
	encoded, err := json.Marshal(body)
	if err != nil {
		return report, err
	}
	explicit := provider.ExplicitSessionID(req.Headers, req.Payload)
	session := ""
	if explicit != "" {
		session = provider.ScopedSessionID(req.Caller, req.Headers, "trae", model, explicit)
	}
	tried := map[string]bool{}
	refreshed := map[*auth.Auth]bool{}
	var lastRejection error
	lastRetryDelay := time.Duration(0)
	returnRejection := func() (provider.UsageReport, error) {
		if lastRetryDelay > 0 {
			req.Writer.Header().Set("Retry-After", fmt.Sprint(int64((lastRetryDelay+time.Second-1)/time.Second)))
		}
		return report, lastRejection
	}
	r.refreshBlockedQuotas(ctx, req, model, image)
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
		selectFn := func(busy map[string]bool) (provider.AccountRef, error) {
			id, e := r.selectAccountFor(req, model, session, tried, busy, image)
			return provider.AccountRef{Provider: "trae", LocalID: id}, e
		}
		var ref provider.AccountRef
		if coordinator := provider.CurrentInvocation(ctx).Select; coordinator != nil {
			ref, err = coordinator(ctx, selectFn)
		} else {
			ref, err = selectFn(nil)
		}
		if err != nil {
			if ctx.Err() == nil && lastRejection != nil {
				return returnRejection()
			}
			return report, err
		}
		tried[ref.LocalID] = true
		report.AccountUID = ref.LocalID
		r.mu.Lock()
		a := r.accounts[ref.LocalID]
		var identityModel string
		var identity *auth.Auth
		if a != nil {
			identity, identityModel = a.modelIdentityFor(model, image)
		}
		r.mu.Unlock()
		if a == nil || identity == nil {
			continue
		}
		if err = r.ensure(ctx, identity); err != nil {
			if ctx.Err() == nil && attempt < 2 {
				r.cool(ref.LocalID, upstream.ErrSessionDead)
				continue
			}
			return report, err
		}
		if err = provider.CheckDispatch(ctx, ref, "trae/"+model); err != nil {
			return report, err
		}
		streamwatch.StartAttempt(ctx)
		// Inspect the prepared wire body, not inferred client defaults.
		var sent map[string]any
		if json.Unmarshal(upstream.PrepareBodyResolved(encoded, identity.Variant, identityModel), &sent) == nil {
			if n, ok := sent["max_tokens"].(float64); ok && n > 0 && n <= 1<<30 && n == float64(int(n)) {
				report.EffectiveLimits = map[string]int{"max_tokens": int(n)}
			}
		}
		stream, status, errorBody, headers, sendErr := r.client(ctx).ChatStreamWithHeaders(identity, encoded, identityModel)
		streamwatch.AttemptHeaders(ctx, status, headers)
		streamwatch.AttemptResult(ctx, status)
		if sendErr != nil {
			return report, errors.New("Trae 上游连接失败")
		}
		if stream == nil {
			kind := upstream.Classify(status, string(errorBody))
			if kind == upstream.ErrSessionDead && !refreshed[identity] && identity.RefreshTokenValue() != "" && attempt < 2 {
				refreshed[identity] = true
				if refreshErr := r.forceRefresh(ctx, ref.LocalID, identity); refreshErr == nil {
					if checkErr := provider.CheckDispatch(ctx, ref, "trae/"+model); checkErr != nil {
						return report, checkErr
					}
					attempt++ // the same-identity retry shares the total dispatch budget
					streamwatch.StartAttempt(ctx)
					stream, status, errorBody, headers, sendErr = r.client(ctx).ChatStreamWithHeaders(identity, encoded, identityModel)
					streamwatch.AttemptHeaders(ctx, status, headers)
					streamwatch.AttemptResult(ctx, status)
					if sendErr != nil {
						return report, errors.New("Trae 上游连接失败")
					}
					kind = upstream.Classify(status, string(errorBody))
				}
			}
			if stream == nil {
				lastRejection = rejectionError(kind, status, upstream.BusinessCode(errorBody))
				lastRetryDelay = 0
				if kind == upstream.ErrSoftRate {
					lastRetryDelay = retryAfter(headers.Get("Retry-After"), time.Now())
					if lastRetryDelay == 0 {
						lastRetryDelay = time.Minute
					}
				}
				r.coolWithDelay(ref.LocalID, kind, lastRetryDelay)
				if retryable(kind) && attempt < 2 {
					continue
				}
				return returnRejection()
			}
		}
		watch := streamwatch.NewWatch(stream, ctx, 0, 0)
		defer watch.Close()
		events := newEvents(watch.Reader(stream))
		var first *upstream.SOLOEvent
		var initialUsage map[string]any
		for {
			first, err = events.next()
			if err != nil {
				break
			}
			if first.Event == "error" {
				err = eventError(first)
				break
			}
			if first.Event == "token_usage" {
				initialUsage = first.Usage
			}
			if first.Event == "done" {
				err = errors.New("Trae 上游返回空回复")
				break
			}
			if first.Response != "" || first.Reasoning != "" || len(first.ToolCalls) > 0 && string(first.ToolCalls) != "null" && string(first.ToolCalls) != "[]" || first.Event == "extra_info" && text(first.Usage["reasoning_content"]) != "" {
				break
			}
		}
		if err != nil {
			stream.Close()
			var rejection *upstream.SOLOStreamError
			if errors.As(err, &rejection) {
				kind := rejection.Kind()
				lastRejection = rejectionError(kind, 0, rejection.Code)
				lastRetryDelay = 0
				if kind == upstream.ErrSoftRate {
					lastRetryDelay = time.Minute
				}
				r.cool(ref.LocalID, kind)
				if retryable(kind) && attempt < 2 {
					continue
				}
				return returnRejection()
			}
			return report, err
		}
		// Cancellation closes both sides and the HTTP body; no detached reader
		// survives the downstream consumer or service shutdown.
		pipeReader, pipeWriter := io.Pipe()
		readerDone := make(chan struct{})
		go func() {
			defer close(readerDone)
			defer stream.Close()
			pipeWriter.CloseWithError(normalizeStream(pipeWriter, events, first, "trae/"+model, initialUsage))
		}()
		stop := context.AfterFunc(ctx, func() { stream.Close(); pipeReader.CloseWithError(ctx.Err()) })
		var usage protocol.Usage
		var known bool
		streaming, _ := req.Payload["stream"].(bool)
		if streaming {
			req.Writer.Header().Set("Content-Type", "text/event-stream")
			req.Writer.Header().Set("Cache-Control", "no-cache")
			usage, known, err = protocol.TranscodeStream(ctx, req.Writer, pipeReader, protocol.Chat, protocol.Protocol(req.Protocol), "trae/"+model)
		} else {
			var reply []byte
			reply, usage, known, err = protocol.CollapseStreamWithUsage(pipeReader, protocol.Chat, "trae/"+model)
			if err == nil {
				reply, err = protocol.ConvertResponse(protocol.Chat, protocol.Protocol(req.Protocol), reply)
			}
			if err == nil {
				req.Writer.Header().Set("Content-Type", "application/json")
				_, err = req.Writer.Write(reply)
			}
		}
		stop()
		pipeReader.Close()
		stream.Close()
		<-readerDone
		report.InputTokens = usage.Input
		report.FinishReason = usage.FinishReason
		report.OutputTokens = usage.Output
		if usage.ReasoningKnown {
			n := usage.Reasoning
			report.ReasoningTokens = &n
		}
		if usage.CacheCreationKnown {
			n := usage.CacheCreation
			report.CacheCreationTokens = &n
		}
		report.TokensKnown = &known
		if usage.CachedKnown {
			cached := usage.Cached
			report.CachedTokens = &cached
		}
		if err != nil {
			report.Status = "incomplete"
			return report, err
		}
		report.Status = "ok"
		if report.FinishReason == "length" || report.FinishReason == "content_filter" {
			report.Status = "incomplete"
		}
		r.mu.Lock()
		a.Failures = 0
		a.LastUsed = nowSec()
		if session != "" {
			r.rememberAffinity(session, ref.LocalID)
		}
		r.mu.Unlock()
		return report, nil
	}
	if lastRejection != nil {
		return returnRejection()
	}
	return report, errors.New("Trae 可用账号已尝试完毕")
}
func (r *Runtime) cool(id string, kind upstream.ErrKind) {
	r.coolWithDelay(id, kind, 0)
}
func (r *Runtime) coolWithDelay(id string, kind upstream.ErrKind, delay time.Duration) {
	if !retryable(kind) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if a := r.accounts[id]; a != nil {
		a.Failures++
		duration := time.Minute
		if kind == upstream.ErrPlanLimit {
			now := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600))
			duration = time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location()).Sub(now)
		}
		if kind == upstream.ErrSoftRate && delay > 0 {
			duration = delay
		}
		a.Cooldown = time.Now().Add(duration)
		a.CooldownKind = kind
	}
}

func normalizeStream(w io.Writer, events *eventReader, first *upstream.SOLOEvent, model string, initialUsage map[string]any) error {
	// The bridge re-prefixes this id per downstream protocol. Emitting a bare
	// provider-scoped token keeps that rewrite honest; a "chatcmpl-" prefix
	// here used to survive into Responses ids as "resp_chatcmpl-trae-...".
	id := fmt.Sprintf("trae-%d", time.Now().UnixNano())
	emit := func(delta map[string]any, finish any, usage map[string]any) error {
		chunk := map[string]any{"id": id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		if usage != nil {
			chunk["usage"] = usage
		}
		raw, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "data: %s\n\n", raw)
		return err
	}
	var reasoning strings.Builder
	if initialUsage != nil {
		if err := emit(map[string]any{}, nil, initialUsage); err != nil {
			return err
		}
	}
	toolsSeen := false
	for e := first; e != nil; {
		switch e.Event {
		case "error":
			return rejectionError((&upstream.SOLOStreamError{Code: e.ErrorCode, Msg: e.ErrorMessage}).Kind(), 0, e.ErrorCode)
		case "output":
			delta := map[string]any{}
			if e.Response != "" {
				delta["content"] = e.Response
			}
			if e.Reasoning != "" {
				delta["reasoning_content"] = e.Reasoning
				reasoning.WriteString(e.Reasoning)
			}
			if len(e.ToolCalls) > 0 && string(e.ToolCalls) != "null" {
				var tools any
				if err := json.Unmarshal(e.ToolCalls, &tools); err != nil {
					return err
				}
				if single, ok := tools.(map[string]any); ok {
					tools = []any{single}
				}
				if list, ok := tools.([]any); ok {
					for index, v := range list {
						if tool, ok := v.(map[string]any); ok {
							if tool["index"] == nil {
								tool["index"] = index
							}
							if fn, ok := tool["function_call"].(map[string]any); ok {
								tool["function"] = fn
								delete(tool, "function_call")
							}
							if fn, ok := tool["function"].(map[string]any); ok {
								if arguments, ok := fn["arguments"].(map[string]any); ok {
									raw, _ := json.Marshal(arguments)
									fn["arguments"] = string(raw)
								}
							}
						}
					}
					delta["tool_calls"] = list
					toolsSeen = true
				}
			}
			if len(delta) > 0 {
				if err := emit(delta, nil, nil); err != nil {
					return err
				}
			}
		case "extra_info":
			// extra_info is an accumulated snapshot, not a second reasoning delta.
			if full, ok := e.Usage["reasoning_content"].(string); ok && strings.HasPrefix(full, reasoning.String()) {
				suffix := strings.TrimPrefix(full, reasoning.String())
				if suffix != "" {
					if err := emit(map[string]any{"reasoning_content": suffix}, nil, nil); err != nil {
						return err
					}
					reasoning.WriteString(suffix)
				}
			}
		case "token_usage":
			if err := emit(map[string]any{}, nil, e.Usage); err != nil {
				return err
			}
		case "done":
			finish := e.FinishReason
			if finish == "" {
				finish = "stop"
			}
			if toolsSeen && finish == "stop" {
				finish = "tool_calls"
			}
			if err := emit(map[string]any{}, finish, nil); err != nil {
				return err
			}
			_, err := io.WriteString(w, "data: [DONE]\n\n")
			return err
		}
		var err error
		e, err = events.next()
		if err != nil {
			return err
		}
	}
	return io.ErrUnexpectedEOF
}
