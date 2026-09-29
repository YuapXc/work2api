package projection

import (
	"encoding/json"
	"strings"
)

// ---- 识别 ----

func looksLikeHarnessUser(text string) bool { return anyContains(text, harnessUserMarkers) }
func looksLikeHarnessSystem(text string) bool {
	return anyContains(text, harnessSystemMarkers)
}

func anyContains(text string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// looksLikeAgenticCLI 是否 agentic CLI 请求：特征工具名命中，或历史里带
// harness 标记。决定保守/激进双档。
func looksLikeAgenticCLI(messages []map[string]any, tools []map[string]any) bool {
	for _, t := range tools {
		if name := toolName(t); agenticToolNames[name] {
			return true
		}
	}
	for _, msg := range messages {
		text := contentToText(msg["content"])
		if looksLikeHarnessUser(text) || looksLikeHarnessSystem(text) {
			return true
		}
	}
	return false
}

// ---- 消息投影 ----

func projectMessagesConservative(messages []map[string]any) []map[string]any {
	out := make([]map[string]any, 0, len(messages))
	for _, msg := range messages {
		if pm := projectConversationMessage(msg, true); pm != nil {
			out = append(out, pm)
		}
	}
	return out
}

func projectConversationMessage(msg map[string]any, conservative bool) map[string]any {
	if msg == nil {
		return nil
	}
	role, _ := msg["role"].(string)
	out := cloneMap(msg)

	switch role {
	case "system":
		out["content"] = truncateText(contentToText(msg["content"]), maxSystemGuidanceChars)
		return out
	case "user":
		out["content"] = truncateText(contentToText(msg["content"]), maxUserChars)
		return out
	case "assistant":
		out["content"] = summarizeFreeText(contentToText(msg["content"]), maxAssistantChars)
		var projectedCalls []any
		if calls, ok := msg["tool_calls"].([]any); ok {
			for _, raw := range calls {
				call, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if pc := projectToolCall(call); pc != nil {
					projectedCalls = append(projectedCalls, pc)
				}
			}
		}
		if len(projectedCalls) > 0 {
			out["tool_calls"] = projectedCalls
		} else if _, has := out["tool_calls"]; has {
			delete(out, "tool_calls")
		}
		return out
	case "tool":
		out["content"] = summarizeToolOutput(contentToText(msg["content"]))
		return out
	}

	if conservative {
		out["content"] = truncateText(contentToText(msg["content"]), maxAssistantChars)
		return out
	}
	return nil
}

func projectToolCall(toolCall map[string]any) map[string]any {
	if toolCall == nil {
		return nil
	}
	fn, _ := toolCall["function"].(map[string]any)
	name, _ := fn["name"].(string)
	args := anyToString(fn["arguments"])
	return map[string]any{
		"id":   toolCall["id"],
		"type": orDefault(toolCall["type"], "function"),
		"function": map[string]any{
			"name":      name,
			"arguments": summarizeToolArguments(name, args),
		},
	}
}

// summarizeToolArguments 超长工具参数压缩：apply_patch 整体省略；合法 JSON 走
// shrinkJSONValue；非法 JSON 硬截断。
func summarizeToolArguments(name string, arguments string) string {
	if len(arguments) <= maxToolArgsChars {
		return arguments
	}
	if name == "apply_patch" {
		return `{"summary": "Large apply_patch payload omitted; a patch was prepared or applied in a previous step."}`
	}
	var parsed any
	if err := json.Unmarshal([]byte(arguments), &parsed); err != nil {
		b, _ := json.Marshal(map[string]any{"summary": truncateText(arguments, 320)})
		return string(b)
	}
	b, err := json.Marshal(shrinkJSONValue(parsed, 0, ""))
	if err != nil {
		b, _ = json.Marshal(map[string]any{"summary": truncateText(arguments, 320)})
	}
	return string(b)
}

// shrinkJSONValue JSON 收缩：深度>4 → "<omitted>"；dict>12 键记 _omitted_keys；
// list>6 项截前 6；长字符串按键名白名单给更大限额。
func shrinkJSONValue(value any, depth int, key string) any {
	if depth >= jsonShrinkMaxDepth {
		return "<omitted>"
	}
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		idx := 0
		for k, item := range v {
			if idx >= jsonShrinkMaxKeys {
				out["_omitted_keys"] = len(v) - idx
				break
			}
			out[k] = shrinkJSONValue(item, depth+1, k)
			idx++
		}
		return out
	case []any:
		if len(v) > jsonShrinkMaxList {
			v = v[:jsonShrinkMaxList]
		}
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = shrinkJSONValue(item, depth+1, key)
		}
		return out
	case string:
		limit := 120
		switch key {
		case "cmd", "chars", "patch", "content", "text", "question":
			limit = 240
		}
		return truncateText(v, limit)
	}
	return value
}

// ---- 工具 schema 投影 ----

type toolsStats struct {
	OriginalTools     int
	ProjectedTools    int
	OriginalToolChars int
	ProjectedToolChars int
}

func projectTools(tools []map[string]any) ([]map[string]any, toolsStats) {
	projected := []map[string]any{}
	for _, tool := range tools {
		if tool["type"] != "function" {
			continue
		}
		fn, _ := tool["function"].(map[string]any)
		if fn == nil {
			fn = tool // 上游兼容：tool 本身就是 function 形态
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		projectedFn := map[string]any{"name": name}
		if params, ok := fn["parameters"]; ok {
			projectedFn["parameters"] = projectSchema(params, 0)
		}
		if strict, ok := fn["strict"]; ok {
			projectedFn["strict"] = strict
		}
		projected = append(projected, map[string]any{"type": "function", "function": projectedFn})
	}
	return projected, toolsStats{
		OriginalTools: len(tools), ProjectedTools: len(projected),
		OriginalToolChars: toolsSize(tools), ProjectedToolChars: toolsSize(projected),
	}
}

func projectSchema(schema any, depth int) any {
	if depth >= schemaMaxDepth {
		return map[string]any{"type": "object"}
	}
	switch v := schema.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, value := range v {
			if !schemaKeepKeys[key] {
				continue
			}
			switch key {
			case "properties":
				props, ok := value.(map[string]any)
				if !ok {
					continue
				}
				projectedProps := make(map[string]any, len(props))
				for prop, ps := range props {
					projectedProps[prop] = projectSchema(ps, depth+1)
				}
				out["properties"] = projectedProps
			case "items":
				out["items"] = projectSchema(value, depth+1)
			case "oneOf", "anyOf", "allOf":
				items, ok := value.([]any)
				if !ok {
					continue
				}
				if len(items) > oneOfMaxItems {
					items = items[:oneOfMaxItems]
				}
				projected := make([]any, len(items))
				for i, item := range items {
					projected[i] = projectSchema(item, depth+1)
				}
				out[key] = projected
			case "additionalProperties":
				if ap, ok := value.(map[string]any); ok {
					out["additionalProperties"] = projectSchema(ap, depth+1)
				} else {
					out[key] = value
				}
			default:
				out[key] = value
			}
		}
		if len(out) == 0 {
			return map[string]any{"type": "object"}
		}
		return out
	case []any:
		if len(v) > oneOfMaxItems {
			v = v[:oneOfMaxItems]
		}
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = projectSchema(item, depth+1)
		}
		return out
	}
	return schema
}

// ---- 尾部选择与历史摘要 ----

// chooseTailStart 从尾部向前选最多 maxTailMessages 条 / maxTailChars 字符。
func chooseTailStart(messages []map[string]any) int {
	if len(messages) == 0 {
		return 0
	}
	start := len(messages) - 1
	totalChars := 0
	kept := 0
	for idx := len(messages) - 1; idx >= 0; idx-- {
		cost := messageCost(messages[idx])
		if kept > 0 && (kept >= maxTailMessages || totalChars+cost > maxTailChars) {
			break
		}
		start = idx
		totalChars += cost
		kept++
	}
	return start
}

// expandTailForToolContext 把尾部工具回复（role=tool）配对的 assistant
// tool_calls 消息扩进 tail，防止截断打断 tool_call_id 配对链。
func expandTailForToolContext(messages []map[string]any, start int) int {
	if start <= 0 || len(messages) == 0 {
		return start
	}
	needed := map[string]bool{}
	for _, msg := range messages[start:] {
		if msg["role"] == "tool" {
			if id, _ := msg["tool_call_id"].(string); id != "" {
				needed[id] = true
			}
		}
	}
	if len(needed) == 0 {
		return start
	}
	expanded := start
	for idx := start - 1; idx >= 0; idx-- {
		msg := messages[idx]
		if msg["role"] != "assistant" {
			continue
		}
		callIDs := map[string]bool{}
		if calls, ok := msg["tool_calls"].([]any); ok {
			for _, raw := range calls {
				call, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if id, _ := call["id"].(string); id != "" {
					callIDs[id] = true
				}
			}
		}
		hit := false
		for id := range callIDs {
			if needed[id] {
				hit = true
				delete(needed, id)
			}
		}
		if hit {
			expanded = idx
			if len(needed) == 0 {
				break
			}
		}
	}
	return expanded
}

func latestUserIndex(messages []map[string]any) *int {
	for idx := len(messages) - 1; idx >= 0; idx-- {
		if messages[idx]["role"] == "user" {
			i := idx
			return &i
		}
	}
	return nil
}

func buildHistorySummary(messages []map[string]any, toolNameByCallID map[string]string) string {
	var lines []string
	totalChars := 0
	summarized := 0
	for _, msg := range messages {
		line := historyLine(msg, toolNameByCallID)
		if line == "" {
			continue
		}
		if summarized >= maxHistoryItems || totalChars+len(line) > maxHistorySummaryChars {
			break
		}
		lines = append(lines, "- "+line)
		totalChars += len(line)
		summarized++
	}
	if remaining := len(messages) - summarized; remaining > 0 {
		lines = append(lines, "- "+itoa(remaining)+" earlier messages or tool results were further condensed.")
	}
	if len(lines) == 0 {
		return ""
	}
	return historyPrefix + "\n" + strings.Join(lines, "\n")
}

func historyLine(msg map[string]any, toolNameByCallID map[string]string) string {
	role, _ := msg["role"].(string)
	text := contentToText(msg["content"])
	switch role {
	case "user":
		return "User asked: " + truncateText(text, historyLineUserChars)
	case "assistant":
		var toolNames []string
		if calls, ok := msg["tool_calls"].([]any); ok {
			for _, raw := range calls {
				call, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				fn, _ := call["function"].(map[string]any)
				if n, _ := fn["name"].(string); n != "" {
					toolNames = append(toolNames, n)
				}
			}
		}
		if len(toolNames) > 4 {
			toolNames = toolNames[:4]
		}
		switch {
		case text != "" && len(toolNames) > 0:
			return "Assistant replied: " + truncateText(text, historyLineReplyChars) +
				" Then called tools: " + strings.Join(toolNames, ", ") + "."
		case len(toolNames) > 0:
			return "Assistant called tools: " + strings.Join(toolNames, ", ") + "."
		case text != "":
			return "Assistant replied: " + truncateText(text, historyLineAssistantChars)
		}
		return ""
	case "tool":
		name := toolNameByCallID[anyToString(msg["tool_call_id"])]
		if name == "" {
			name = "tool"
		}
		return "Tool " + name + " returned: " + toolOutputInlineSummary(text)
	case "system":
		return "System guidance: " + truncateText(text, historyLineSystemChars)
	}
	return ""
}

func buildToolCallNameMap(messages []map[string]any) map[string]string {
	mapping := map[string]string{}
	for _, msg := range messages {
		if msg["role"] != "assistant" {
			continue
		}
		calls, ok := msg["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			id, _ := call["id"].(string)
			fn, _ := call["function"].(map[string]any)
			name, _ := fn["name"].(string)
			if id != "" && name != "" {
				mapping[id] = name
			}
		}
	}
	return mapping
}

func mergeGuidanceMessages(messages []string) string {
	var merged []string
	total := 0
	for _, message := range messages {
		if len(merged) >= 2 {
			break
		}
		text := strings.TrimSpace(message)
		if text == "" {
			continue
		}
		if total+len(text) > maxSystemGuidanceChars {
			text = truncateText(text, maxSystemGuidanceChars-total)
		}
		merged = append(merged, text)
		total += len(text)
		if total >= maxSystemGuidanceChars {
			break
		}
	}
	if len(merged) == 0 {
		return ""
	}
	if len(merged) == 1 {
		return merged[0]
	}
	return "Additional instructions:\n" + strings.Join(merged, "\n\n")
}

// ---- 工具输出/文本压缩 ----

// summarizeToolOutput 工具输出压缩：保留前 10 行 + 后 6 行 + Process exited 行，
// 剔除 Output:/Chunk ID:/Wall time: 等包装行，中间插入省略标记。
func summarizeToolOutput(text string) string {
	if text == "" {
		return ""
	}
	if len(text) <= maxToolOutputChars && strings.Count(text, "\n") <= 24 {
		return text
	}
	lines := strings.Split(text, "\n")
	exitLine := ""
	for _, line := range lines {
		if s := strings.TrimSpace(line); strings.Contains(s, "Process exited with code") {
			exitLine = s
			break
		}
	}
	var useful []string
	sawOutput := false
	for _, line := range lines {
		stripped := strings.TrimRight(line, " \t\r")
		switch {
		case stripped == "Output:":
			sawOutput = true
			continue
		case strings.HasPrefix(stripped, "Chunk ID:"),
			strings.HasPrefix(stripped, "Wall time:"),
			strings.HasPrefix(stripped, "Original token count:"),
			strings.HasPrefix(stripped, "Process exited with code"):
			continue
		}
		_ = sawOutput
		useful = append(useful, stripped)
	}

	head := useful
	if len(head) > 10 {
		head = head[:10]
	}
	var tail []string
	if len(useful) > 16 {
		tail = useful[len(useful)-6:]
	}
	omitted := len(useful) - len(head) - len(tail)
	if omitted < 0 {
		omitted = 0
	}

	var parts []string
	if exitLine != "" {
		parts = append(parts, exitLine)
	}
	if len(head) > 0 {
		parts = append(parts, "Key output:")
		parts = append(parts, head...)
	}
	if omitted > 0 {
		parts = append(parts, "... [omitted "+itoa(omitted)+" lines] ...")
	}
	if len(tail) > 0 {
		parts = append(parts, "Recent tail:")
		parts = append(parts, tail...)
	}
	summary := strings.TrimSpace(strings.Join(parts, "\n"))
	if summary == "" {
		summary = text
	}
	return truncateText(summary, maxToolOutputChars)
}

func toolOutputInlineSummary(text string) string {
	summarized := strings.ReplaceAll(summarizeToolOutput(text), "\n", " | ")
	return truncateText(summarized, inlineSummaryChars)
}

// summarizeFreeText 自由文本压缩：留头 limit/2 + 尾 limit/3，中间插入省略标记。
func summarizeFreeText(text string, limit int) string {
	if text == "" || len(text) <= limit {
		return text
	}
	head := strings.TrimRight(text[:limit/2], " \t\r\n")
	tail := strings.TrimLeft(text[len(text)-limit/3:], " \t\r\n")
	omitted := len(text) - len(head) - len(tail)
	return head + "\n... [" + itoa(omitted) + " chars omitted] ...\n" + tail
}

// truncateText 硬截断：留前 limit-24 字符，末尾追加 " ... [truncated N chars]"。
func truncateText(text string, limit int) string {
	if text == "" || len(text) <= limit {
		return text
	}
	keep := limit - 24
	if keep < 0 {
		keep = 0
	}
	// 按 rune 对齐截断点，避免切断多字节字符
	cut := keep
	for cut > 0 && (text[cut]&0xC0) == 0x80 {
		cut--
	}
	return strings.TrimRight(text[:cut], " \t\r\n") +
		" ... [truncated " + itoa(len(text)-cut) + " chars]"
}

// ---- 度量 ----

func contentToText(content any) string {
	switch v := content.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, block := range v {
			switch bb := block.(type) {
			case map[string]any:
				if t, ok := bb["text"].(string); ok {
					b.WriteString(t)
				} else if o, ok := bb["output"].(string); ok {
					b.WriteString(o)
				}
			case string:
				b.WriteString(bb)
			}
		}
		return b.String()
	}
	return anyToString(content)
}

func messageCost(msg map[string]any) int {
	cost := len(contentToText(msg["content"]))
	if calls, ok := msg["tool_calls"].([]any); ok {
		for _, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			fn, _ := call["function"].(map[string]any)
			if fn == nil {
				continue
			}
			name, _ := fn["name"].(string)
			args, _ := fn["arguments"].(string)
			cost += len(name) + len(args)
		}
	}
	return cost
}

func messagesSize(messages []map[string]any) int {
	total := 0
	for _, msg := range messages {
		total += messageCost(msg)
		if role, _ := msg["role"].(string); role != "" {
			total += len(role)
		}
	}
	return total
}

func toolName(tool map[string]any) string {
	fn, _ := tool["function"].(map[string]any)
	if fn == nil {
		fn = tool
	}
	name, _ := fn["name"].(string)
	return name
}

func toolsSize(tools []map[string]any) int {
	b, err := json.Marshal(toAny(tools))
	if err != nil {
		return 0
	}
	return len(b)
}

// ---- 小工具 ----

func anyToString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case nil:
		return ""
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

func orDefault(v any, def string) string {
	if s, ok := v.(string); ok && s != "" {
		return s
	}
	return def
}

func itoa(n int) string {
	return fmtInt(n)
}

func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
