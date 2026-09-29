// Package projection —— /v1/responses 请求的"最小语义闭包"投影（消息压缩）。
//
// 移植自 buddy-proxy protocols/responses_projection.py（2026-09-29 版）。
// Codex CLI 会把大量运行时提示、完整工具 schema、长历史、超大工具输出一并塞进
// /v1/responses 请求。本包在保持 OpenAI Chat 兼容的前提下，把发往上游的 body
// 投影成更小的等价上下文：
//
//   - 固定短 system 摘要替换 Codex/Claude Code harness
//   - 保留最新用户意图（锚点）与最近一段真实 assistant/tool 链路
//   - 更早历史压缩成规则摘要（User asked… / Assistant called tools… / Tool X returned…）
//   - 超长 tool output / tool arguments / 自由文本压缩成可继续推理的摘要
//   - 工具 schema 投影：只保留语义白名单键
//
// 双档门控：检测到 agentic CLI 特征（特定工具名或 harness 标记）才走激进重组，
// 否则保守模式只做逐条截断、不重组历史。标记字符串（"... [omitted N lines] ..."等）
// 是设计特性：人类可读、可直接展示，客户端无需特殊解析。
package projection

const (
	historyPrefix             = "Earlier conversation summary (condensed):"
	baseSystemPrompt          = "You are a coding assistant serving an OpenAI-compatible CLI. Be precise, concise, safe, and action-oriented. Use available tools when needed, follow repository instructions and durable user context, and continue from the preserved recent context. If earlier history was condensed, rely on the preserved recent messages and rerun tools when exact old details are required."
	maxSystemGuidanceChars    = 1200
	maxUserChars              = 3200
	maxAssistantChars         = 1800
	maxToolOutputChars        = 1600
	maxToolArgsChars          = 900
	maxHistorySummaryChars    = 2200
	maxHistoryItems           = 10
	maxTailMessages           = 8
	maxTailChars              = 7000
	schemaMaxDepth            = 6
	jsonShrinkMaxDepth        = 4
	jsonShrinkMaxKeys         = 12
	jsonShrinkMaxList         = 6
	oneOfMaxItems             = 6
	historyLineUserChars      = 220
	historyLineAssistantChars = 180
	historyLineReplyChars     = 160
	historyLineToolChars      = 220
	historyLineSystemChars    = 180
	inlineSummaryChars        = 220
)

// agenticToolNames Codex CLI 等 agentic 客户端的特征工具名：命中即走激进模式。
var agenticToolNames = map[string]bool{
	"exec_command": true, "write_stdin": true, "update_plan": true,
	"request_user_input": true, "view_image": true, "get_goal": true,
	"create_goal": true, "update_goal": true, "apply_patch": true,
	"tool_search_tool": true,
}

var harnessUserMarkers = []string{
	"# AGENTS.md instructions", "<environment_context>", "<permissions instructions>",
	"<collaboration_mode>", "<skills_instructions>", "<system-reminder>", "# claudeMd",
}

var harnessSystemMarkers = []string{
	"You are a coding agent running in the Codex CLI", "Within this context, Codex refers to",
	"# AGENTS.md spec", "<permissions instructions>", "<collaboration_mode>",
	"<skills_instructions>", "The following deferred tools are now available via ToolSearch.",
	"### Available skills", "## request_user_input availability", "You are Claude Code",
}

// schemaKeepKeys 工具 schema 投影白名单：丢弃 description 等（上游为省 token 的取舍）。
var schemaKeepKeys = map[string]bool{
	"type": true, "properties": true, "required": true, "items": true,
	"enum": true, "oneOf": true, "anyOf": true, "allOf": true,
	"additionalProperties": true, "format": true,
	"minimum": true, "maximum": true, "minItems": true, "maxItems": true,
	"minLength": true, "maxLength": true, "nullable": true,
}

// Stats 记录一次投影的统计（供日志/诊断）。
type Stats struct {
	Mode                  string `json:"mode"` // aggressive / conservative
	Aggressive            bool   `json:"aggressive"`
	DroppedHarness        int    `json:"dropped_harness_messages"`
	PreservedGuidance     int    `json:"preserved_guidance_messages"`
	SummarizedHistory     int    `json:"summarized_history_messages"`
	AnchorPreserved       bool   `json:"anchor_user_preserved"`
	TailMessages          int    `json:"tail_messages"`
	OriginalMessages      int    `json:"original_messages"`
	ProjectedMessages     int    `json:"projected_messages"`
	OriginalMessageChars  int    `json:"original_message_chars"`
	ProjectedMessageChars int    `json:"projected_message_chars"`
	OriginalTools         int    `json:"original_tools"`
	ProjectedTools        int    `json:"projected_tools"`
	OriginalToolChars     int    `json:"original_tool_chars"`
	ProjectedToolChars    int    `json:"projected_tool_chars"`
}

// Body 把 Responses 转出来的 Chat body 投影成最小上下文，返回新 body 与统计。
// 输入是 map[string]any 形态（与 orchestrator.enhanceBody 的 body 同构）。
func Body(body map[string]any) (map[string]any, Stats) {
	projected := cloneMap(body)
	rawMsgs, _ := body["messages"].([]any)
	rawTools, _ := body["tools"].([]any)
	messages := cloneMsgs(rawMsgs)
	tools := cloneMsgs(rawTools)

	projectedTools, toolStats := projectTools(tools)
	projected["tools"] = projectedTools

	aggressive := looksLikeAgenticCLI(messages, tools)
	if !aggressive {
		conservative := projectMessagesConservative(messages)
		projected["messages"] = toAny(conservative)
		return projected, Stats{
			Mode: "conservative", OriginalMessages: len(messages),
			ProjectedMessages:     len(conservative),
			OriginalMessageChars:  messagesSize(messages),
			ProjectedMessageChars: messagesSize(conservative),
			OriginalTools:         toolStats.OriginalTools, ProjectedTools: toolStats.ProjectedTools,
			OriginalToolChars: toolStats.OriginalToolChars, ProjectedToolChars: toolStats.ProjectedToolChars,
		}
	}

	toolNameByCallID := buildToolCallNameMap(messages)
	var guidance []string
	conversation := []map[string]any{}
	dropped := 0

	for _, msg := range messages {
		role, _ := msg["role"].(string)
		text := contentToText(msg["content"])

		if role == "system" {
			if looksLikeHarnessSystem(text) {
				dropped++
				continue
			}
			if g := truncateText(text, maxSystemGuidanceChars); g != "" {
				guidance = append(guidance, g)
			}
			continue
		}
		if role == "user" && looksLikeHarnessUser(text) {
			dropped++
			continue
		}
		if pm := projectConversationMessage(msg, false); pm != nil {
			conversation = append(conversation, pm)
		}
	}
	if len(conversation) == 0 {
		conv := projectMessagesConservative(messages)
		conversation = conv
	}

	tailStart := chooseTailStart(conversation)
	tailStart = expandTailForToolContext(conversation, tailStart)
	latestUserIdx := latestUserIndex(conversation)

	var anchorUser map[string]any
	if latestUserIdx != nil && *latestUserIdx < tailStart {
		anchorUser = cloneMap(conversation[*latestUserIdx])
	}

	var omitted []map[string]any
	for idx, msg := range conversation {
		if idx >= tailStart {
			break
		}
		if latestUserIdx != nil && idx == *latestUserIdx && anchorUser != nil {
			continue
		}
		omitted = append(omitted, msg)
	}

	finalMessages := []map[string]any{{"role": "system", "content": baseSystemPrompt}}
	if gm := mergeGuidanceMessages(guidance); gm != "" {
		finalMessages = append(finalMessages, map[string]any{"role": "system", "content": gm})
	}
	if hs := buildHistorySummary(omitted, toolNameByCallID); hs != "" {
		finalMessages = append(finalMessages, map[string]any{"role": "system", "content": hs})
	}
	if anchorUser != nil {
		finalMessages = append(finalMessages, anchorUser)
	}
	for _, m := range conversation[tailStart:] {
		finalMessages = append(finalMessages, m)
	}
	projected["messages"] = toAny(finalMessages)

	return projected, Stats{
		Mode: "aggressive", Aggressive: true,
		DroppedHarness: dropped, PreservedGuidance: len(guidance),
		SummarizedHistory: len(omitted),
		AnchorPreserved:   anchorUser != nil,
		TailMessages:      len(conversation) - tailStart,
		OriginalMessages:  len(messages), ProjectedMessages: len(finalMessages),
		OriginalMessageChars: messagesSize(messages), ProjectedMessageChars: messagesSize(finalMessages),
		OriginalTools: toolStats.OriginalTools, ProjectedTools: toolStats.ProjectedTools,
		OriginalToolChars: toolStats.OriginalToolChars, ProjectedToolChars: toolStats.ProjectedToolChars,
	}
}
