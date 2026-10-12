package runtime

import (
	"regexp"
	"strings"
)

// 出站指纹脱敏：把「客户端固定模板」的改写从开头那几条 system 消息扩展到整条
// 对话。思路参考 linguo2625469/workbuddy2api-panel 的 internal/upstream/sanitize.go
// （该仓库另有一套「整体替换 system prompt」的做法，会丢客户端指令，本项目不采用）。
//
// 为什么需要：WorkBuddy 对客户端指纹做逐字匹配拦截（400 code=11-128），一字之差
// 即可放行。这些模板不止出现在 system 里——assistant 会回显、reasoning 会带上、
// 工具参数与工具结果里也会出现，而原兼容层只看开头，历史里的同一批模板原样出站。
//
// 启用时机：只在最深的兼容档（level 2，即固定身份改写也被拒之后）才改写历史，
// 没有拒绝证据时不动用户原话。

// channelFingerprintFeatures 特征预检：全部不中即原样返回（热路径零分配）。
var channelFingerprintFeatures = []string{
	"You are Claude Code",
	"You are a coding agent running in the Codex CLI",
	"Main branch (",
	"github.com/anthropics/",
	"cc_",
}

// 改写规则统一按短语匹配，不要求行首：同一批模板出现在工具参数里的转义 JSON
// （真实换行变成 \n）时，只按行锚定的规则会漏掉。
//
// channelIdentityBannerRe 匹配到 "official CLI" 为止再补回 " tool"，破坏上游的
// 逐字匹配，语义与可读性不变。不带结尾标点，CLI 版（句号收尾）与桌面版（逗号接
// Claude Agent SDK）一并覆盖。
var (
	channelIdentityBannerRe = regexp.MustCompile(`(You are Claude Code, Anthropic's official CLI)( for Claude)`)
	channelCodexIntroRe     = regexp.MustCompile(`(You are a coding agent running in the Codex CLI)(, a terminal-based coding assistant\.)`)
	channelBranchRuleRe     = regexp.MustCompile(`Main branch \(you will usually use this for PRs\)`)
	channelFeedbackRuleRe   = regexp.MustCompile(`(To give feedback, users should report the issue at https://github\.com/anthropics/)claude-code(/issues)`)
)

// 剥离层：billing header 键值段整段删除；兜底层再对残留的裸键名做最小缩写
// （header→hdr）——两种形态都要破坏，大小写不敏感。
var (
	channelHeaderKVRe   = regexp.MustCompile(`(?i)x-anthropic-billing-header:[^;\n]*;?[ \t\r\n]*`)
	channelHeaderBareRe = regexp.MustCompile(`(?i)x-anthropic-billing-header`)
	channelCCKVRe       = regexp.MustCompile(`(?i)\bcc_[a-z0-9_]+=[^;\n]*;?[ \t\r\n]*`)
)

// channelErrorCodeRe 上游反探测串：业务错误码自身出现在请求体里（裸数字或带连
// 字符）时归一成带连字符的形态。
//
// 面板作者的实测是「出现即整单 400」；本机 2026-10-12 在 user / assistant /
// tool / reasoning 四种位置复测未复现，因此不放进常态路径，只留在被拒后的这一档：
// 请求本来就在失败，改写代价可控。用户讨论该错误码时会被改写，属已知取舍。
var channelErrorCodeRe = regexp.MustCompile(`\b11-?128\b`)

// sanitizeChannelText 单段文本脱敏：预检不中即原样返回（零分配）。
func sanitizeChannelText(text string) string {
	if !hasChannelFingerprint(text) {
		return text
	}
	text = channelIdentityBannerRe.ReplaceAllString(text, "$1 tool$2")
	text = channelCodexIntroRe.ReplaceAllString(text, "$1 tool$2")
	text = channelBranchRuleRe.ReplaceAllString(text, "Default branch (you will usually use this for PRs)")
	text = channelFeedbackRuleRe.ReplaceAllString(text, "${1}cl4ude-c0de${2}")
	text = channelErrorCodeRe.ReplaceAllString(text, "11-128")
	if channelHeaderKVRe.MatchString(text) {
		text = channelHeaderKVRe.ReplaceAllString(text, "")
	}
	if strings.Contains(text, "cc_") {
		text = channelCCKVRe.ReplaceAllString(text, "")
	}
	return channelHeaderBareRe.ReplaceAllString(text, "x-anthropic-billing-hdr")
}

func hasChannelFingerprint(text string) bool {
	if channelHeaderBareRe.MatchString(text) || channelErrorCodeRe.MatchString(text) {
		return true
	}
	for _, feature := range channelFingerprintFeatures {
		if strings.Contains(text, feature) {
			return true
		}
	}
	return false
}

// sanitizeChannelContent 兼容字符串与多模态数组：只动 text 段，图片等原样保留。
func sanitizeChannelContent(value any) (any, bool) {
	switch content := value.(type) {
	case string:
		sanitized := sanitizeChannelText(content)
		return sanitized, sanitized != content
	case []any:
		var blocks []any
		for i, item := range content {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			text, ok := block["text"].(string)
			if !ok {
				continue
			}
			sanitized := sanitizeChannelText(text)
			if sanitized == text {
				continue
			}
			if blocks == nil {
				blocks = append([]any(nil), content...)
			}
			copied := copyChannelMap(block)
			copied["text"] = sanitized
			blocks[i] = copied
		}
		if blocks == nil {
			return value, false
		}
		return blocks, true
	}
	return value, false
}

// sanitizeChannelToolCalls 处理 assistant.tool_calls[].function.arguments。
// arguments 是字符串化的 JSON，按文本改写即可；content 为 null 的工具调用轮同样
// 会走到这里（旧做法在 content 缺失时跳过整条消息，工具参数成了盲区）。
func sanitizeChannelToolCalls(value any) (any, bool) {
	calls, ok := value.([]any)
	if !ok {
		return value, false
	}
	var out []any
	for i, item := range calls {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		function, ok := call["function"].(map[string]any)
		if !ok {
			continue
		}
		args, ok := function["arguments"].(string)
		if !ok {
			continue
		}
		sanitized := sanitizeChannelText(args)
		if sanitized == args {
			continue
		}
		if out == nil {
			out = append([]any(nil), calls...)
		}
		copiedCall := copyChannelMap(call)
		copiedFunction := copyChannelMap(function)
		copiedFunction["arguments"] = sanitized
		copiedCall["function"] = copiedFunction
		out[i] = copiedCall
	}
	if out == nil {
		return value, false
	}
	return out, true
}

func copyChannelMap(source map[string]any) map[string]any {
	copied := make(map[string]any, len(source))
	for key, value := range source {
		copied[key] = value
	}
	return copied
}

// sanitizeChannelMessages 逐条脱敏：content、reasoning_content、reasoning 与
// tool_calls 各自独立判断。只复制真正改动的消息，未命中时返回原切片。
func sanitizeChannelMessages(messages []any) ([]any, bool) {
	var out []any
	for i, item := range messages {
		message, ok := item.(map[string]any)
		if !ok {
			continue
		}
		updated, changed := sanitizeChannelMessage(message)
		if !changed {
			continue
		}
		if out == nil {
			out = append([]any(nil), messages...)
		}
		out[i] = updated
	}
	if out == nil {
		return messages, false
	}
	return out, true
}

func sanitizeChannelMessage(message map[string]any) (map[string]any, bool) {
	var updated map[string]any
	ensure := func() map[string]any {
		if updated == nil {
			updated = copyChannelMap(message)
		}
		return updated
	}
	if content, ok := message["content"]; ok {
		if sanitized, changed := sanitizeChannelContent(content); changed {
			ensure()["content"] = sanitized
		}
	}
	for _, key := range []string{"reasoning_content", "reasoning"} {
		text, ok := message[key].(string)
		if !ok {
			continue
		}
		if sanitized := sanitizeChannelText(text); sanitized != text {
			ensure()[key] = sanitized
		}
	}
	if calls, ok := message["tool_calls"]; ok {
		if sanitized, changed := sanitizeChannelToolCalls(calls); changed {
			ensure()["tool_calls"] = sanitized
		}
	}
	if updated == nil {
		return message, false
	}
	return updated, true
}
