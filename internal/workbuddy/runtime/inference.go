package runtime

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
	"work2api/internal/core/provider"
	"work2api/internal/requestdiag"
	"work2api/internal/streamwatch"

	"work2api/internal/store"
	"work2api/internal/tokenusage"
	"work2api/internal/workbuddy/pool"
)

type LogArgs struct {
	Ctx             context.Context
	Protocol, Model string
	Acc             *pool.Account
	T0              time.Time
	Status, ErrStr  string
	Usage           map[string]any
	Cooldown        float64
	Input, Output   string
	Reasoning       string
	AppName, Effort string
	UserID          int64
	AppID           int64
	Quota           UsageObserver
	UpdatePool      bool
}

func (o *Runtime) LogUsage(a LogArgs) {
	if a.Ctx != nil {
		streamwatch.Outcome(a.Ctx, a.Status)
	}
	completedTransport := a.Status == "ok" || a.Status == "incomplete"
	if a.Quota != nil && a.Usage != nil {
		a.Quota.Observe(a.Usage)
	}
	if a.UserID > 0 {
		a.Input = ""
		a.Output = ""
		a.Reasoning = ""
	}
	if a.Acc != nil && a.UpdatePool {
		if completedTransport {
			o.Pool.OnSuccess(a.Acc.UID)
		} else {
			cd := a.Cooldown
			if cd == 0 {
				cd = CooldownSoft
			}
			o.Pool.OnFailure(a.Acc.UID, cd)
		}
	}
	if provider.CurrentInvocation(a.Ctx).Diagnostic {
		return
	}
	inTok, outTok := UsageTokens(a.Usage)
	known := UsageCountsKnown(a.Usage)
	var credits *float64
	if a.Usage != nil {
		if c, ok := a.Usage["credit"].(float64); ok {
			credits = &c
		}
	}
	if completedTransport || credits != nil {
		SessionCredits(a.Ctx, credits)
	}
	uid := ""
	if a.Acc != nil {
		uid = a.Acc.UID
	}
	effort := a.Effort
	if effort == "" {
		effort = "default"
	}
	diagnostics := requestdiag.UsageDiagnostics(a.Ctx)
	profile := ""
	if a.Acc != nil {
		profile = a.Acc.Profile
	}
	if reuse := SessionCacheObservation(a.Ctx, uid, a.Model, profile, inTok, UsageCachedTokens(a.Usage), completedTransport && known); reuse != nil {
		if diagnostics == nil {
			diagnostics = &store.UsageDiagnostics{}
		}
		diagnostics.CacheReuse = reuse
	}
	params := store.UsageParams{
		Diagnostics:         diagnostics,
		Model:               a.Model,
		Protocol:            a.Protocol,
		AccountUID:          uid,
		InputTokens:         inTok,
		TokensKnown:         &known,
		OutputTokens:        outTok,
		CachedTokens:        UsageCachedTokens(a.Usage),
		ReasoningTokens:     tokenusage.Reasoning(a.Usage),
		CacheCreationTokens: tokenusage.Created(a.Usage),
		LatencyMs:           float64(time.Since(a.T0).Milliseconds()),
		Status:              a.Status,
		Error:               a.ErrStr,
		InputContent:        ClipContent(a.Input, o.cfg.UsageContentMaxBytes),
		OutputContent:       ClipContent(a.Output, o.cfg.UsageContentMaxBytes),
		ReasoningContent:    ClipContent(a.Reasoning, o.cfg.UsageContentMaxBytes),
		Credits:             credits,
		AppName:             a.AppName,
		UserID:              a.UserID,
		AppID:               a.AppID,
		ReasoningEffort:     effort,
	}
	if o.RecordUsage != nil {
		o.RecordUsage(a.Ctx, params)
	} else {
		_ = o.db.LogUsage(params)
	}
}

// ClipContent 把落库的 content 截到 max 字节（0=不截），主要挡 base64 图片这类
// 大 payload。按 UTF-8 边界回退，避免把多字节字符切成乱码，并留可见截断标记。
func ClipContent(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…[已截断，原长 " + strconv.Itoa(len(s)) + " 字节]"
}

func UsageTokens(u map[string]any) (int, int) {
	return tokenusage.Totals(u)
}

func UsageCountsKnown(u map[string]any) bool {
	valid := func(keys ...string) bool {
		for _, key := range keys {
			switch n := u[key].(type) {
			case int:
				if n >= 0 {
					return true
				}
			case float64:
				if n >= 0 && n < 1e12 && n == float64(int64(n)) {
					return true
				}
			}
		}
		return false
	}
	return valid("prompt_tokens", "input_tokens") && valid("completion_tokens", "output_tokens")
}

// Missing fields remain unknown; raw numeric anomalies are kept for diagnosis.
func UsageCachedTokens(u map[string]any) *int {
	return tokenusage.Cached(u)
}

func FirstInt(m map[string]any, keys ...string) int {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch x := v.(type) {
			case float64:
				return int(x)
			case int:
				return x
			}
		}
	}
	return 0
}

func UsageReasoningEffort(body map[string]any) string {
	if e, ok := body["reasoning_effort"].(string); ok && strings.TrimSpace(e) != "" {
		return strings.ToLower(strings.TrimSpace(e))
	}
	if th, ok := body["thinking"].(map[string]any); ok {
		if th["type"] == "disabled" {
			return "off"
		}
	}
	return "default"
}

// mergeUsageInto merges an SSE chunk's usage into acc (returns merged).
func MergeUsageFromLine(line string, acc map[string]any) map[string]any {
	if !strings.HasPrefix(line, "data:") {
		return acc
	}
	data := strings.TrimSpace(line[5:])
	if data == "[DONE]" || data == "" {
		return acc
	}
	var chunk map[string]any
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return acc
	}
	return MergeUsageChunk(chunk, acc)
}

func MergeUsageChunk(chunk map[string]any, acc map[string]any) map[string]any {
	if u, ok := chunk["usage"].(map[string]any); ok && u != nil {
		return tokenusage.Merge(acc, u)
	}
	return acc
}

// DeltaParts extracts content + reasoning increments (with tool_call recompose).
func DeltaParts(line string) (string, string, string) {
	if !strings.HasPrefix(line, "data:") {
		return "", "", ""
	}
	data := strings.TrimSpace(line[5:])
	if data == "[DONE]" || data == "" {
		return "", "", ""
	}
	var chunk map[string]any
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return "", "", ""
	}
	return DeltaPartsChunk(chunk)
}

func DeltaPartsChunk(chunk map[string]any) (string, string, string) {
	var content, reasoning strings.Builder
	var finish string
	choices, _ := chunk["choices"].([]any)
	for _, ch := range choices {
		choice, _ := ch.(map[string]any)
		if f, ok := choice["finish_reason"].(string); ok && f != "" {
			finish = f
		}
		delta, _ := choice["delta"].(map[string]any)
		if delta == nil {
			continue
		}
		if s, ok := delta["content"].(string); ok {
			content.WriteString(s)
		}
		if s, ok := delta["reasoning_content"].(string); ok {
			reasoning.WriteString(s)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, t := range tcs {
				tc, _ := t.(map[string]any)
				fn, _ := tc["function"].(map[string]any)
				if fn == nil {
					continue
				}
				if name, ok := fn["name"].(string); ok && name != "" {
					content.WriteString("<tool_call:" + name + " ")
				}
				if args, ok := fn["arguments"].(string); ok && args != "" {
					content.WriteString(args)
				}
				if _, hasID := tc["id"]; hasID {
					if _, hasArgs := fn["arguments"]; !hasArgs {
						content.WriteString(">")
					}
				}
			}
		}
	}
	return content.String(), reasoning.String(), finish
}

// SanitizeChatSSE strips empty delta fields from a passthrough Chat SSE line.
// Returns "" if the whole line should be dropped.
func SanitizeChatSSE(line string) string { return SanitizeChatSSEWithRole(line, nil) }

func SanitizeChatSSEWithRole(line string, sentRole *bool) string {
	if !strings.HasPrefix(line, "data:") {
		return line
	}
	raw := strings.TrimSpace(line[5:])
	if raw == "[DONE]" || raw == "" {
		return line
	}
	var chunk map[string]any
	if json.Unmarshal([]byte(raw), &chunk) != nil {
		return line
	}
	return SanitizeChatChunkWithRole(line, chunk, sentRole)
}

func SanitizeChatChunkWithRole(line string, chunk map[string]any, sentRole *bool) string {
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) == 0 {
		return line
	}
	changed, filtered := strings.IndexByte(line, '\n') >= 0 || strings.IndexByte(line, '\r') >= 0, false
	var kept []any
	for i, ch := range choices {
		choice, ok := ch.(map[string]any)
		if !ok {
			if filtered {
				kept = append(kept, ch)
			}
			continue
		}
		if finish, ok := choice["finish_reason"].(string); ok && finish == "" {
			choice["finish_reason"] = nil
			changed = true
		}
		if delta, ok := choice["delta"].(map[string]any); ok {
			if sentRole != nil {
				if role, ok := delta["role"].(string); ok && role != "" {
					if *sentRole {
						delete(delta, "role")
						changed = true
					} else {
						*sentRole = true
					}
				}
			}
			if fn, ok := delta["function_call"].(map[string]any); ok && len(fn) <= 2 && IsEmptyVal(fn["name"]) && IsEmptyVal(fn["arguments"]) {
				delete(delta, "function_call")
				changed = true
			}
			for _, k := range []string{"content", "reasoning_content", "refusal", "function_call", "tool_calls", "reasoning"} {
				if v, has := delta[k]; has && IsEmptyVal(v) {
					delete(delta, k)
					changed = true
				}
			}
			if calls, ok := delta["tool_calls"].([]any); ok {
				for _, raw := range calls {
					call, _ := raw.(map[string]any)
					fn, _ := call["function"].(map[string]any)
					if name, ok := fn["name"].(string); ok && name == "" {
						delete(fn, "name")
						changed = true
					}
				}
			}
			if len(delta) == 0 {
				if choice["finish_reason"] != nil && choice["finish_reason"] != "" {
					delete(choice, "delta")
					changed = true
				} else {
					if !filtered {
						kept = append([]any{}, choices[:i]...)
						filtered = true
					}
					changed = true
					continue
				}
			}
		}
		if filtered {
			kept = append(kept, ch)
		}
	}
	if filtered {
		if len(kept) == 0 && chunk["usage"] == nil {
			return ""
		}
		if kept == nil {
			kept = []any{}
		}
		chunk["choices"] = kept
	}
	if !changed {
		return line
	}
	b, err := json.Marshal(chunk)
	if err != nil {
		return line
	}
	return "data: " + string(b)
}

func IsEmptyVal(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	case bool:
		return !x
	}
	return false
}

func (o *Runtime) ExtractInputText(body map[string]any) string {
	out := provider.NewLogCaptureWithPolicy(provider.LogCapturePolicy{Enabled: true, Limit: o.cfg.UsageContentMaxBytes})
	msgs, _ := body["messages"].([]any)
	written := false
	for _, item := range msgs {
		if out.Full() {
			break
		}
		message, _ := item.(map[string]any)
		role, _ := message["role"].(string)
		if role == "" {
			role = "user"
		}
		started := false
		appendText := func(text string) {
			if text == "" {
				return
			}
			if !started {
				if written {
					out.WriteString("\n")
				}
				out.WriteString(role)
				out.WriteString(": ")
				started = true
				written = true
			} else {
				out.WriteString(" ")
			}
			out.WriteString(text)
		}
		switch content := message["content"].(type) {
		case string:
			appendText(content)
		case []any:
			for _, item := range content {
				if out.Full() {
					break
				}
				part, _ := item.(map[string]any)
				switch part["type"] {
				case "text":
					text, _ := part["text"].(string)
					appendText(text)
				case "image_url":
					if image, ok := part["image_url"].(map[string]any); ok {
						appendText("[图片: image_url|")
						out.WriteString(ToStr(image["url"]))
						out.WriteString("]")
					}
				}
			}
		}
	}
	return out.String()
}

type UsageObserver interface{ Observe(map[string]any) }
