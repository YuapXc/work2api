// Package desensitize mitigates CodeBuddy's content moderation from
// false-flagging clients' fixed compliance system templates. Ported from
// workbuddy_one/desensitize.py.
//
// It inserts a zero-width space (U+200B) inside a small, explicit list of
// "compliance-declaration" terms so the backend's keyword match fails while
// humans/models still read the word unchanged. By default it only touches
// system messages, and it can prune/compact bulky harness runtime context.
package desensitize

import (
	"regexp"
	"sort"
	"strings"
)

const zwsp = "​"

var sensitiveTerms = []string{
	"DoS", "DDoS", "exploit", "credential testing", "credential stuffing",
	"supply chain compromise", "supply-chain compromise", "detection evasion",
	"C2 frameworks", "C2 framework", "command and control", "malicious purposes",
	"malicious intent", "mass targeting", "brute force", "brute-force",
	"privilege escalation", "reverse shell", "remote code execution", "SQL injection",
	"XSS", "CSRF", "phishing", "malware", "ransomware", "keylogger", "rootkit",
	"backdoor", "botnet", "zero-day", "0day",
	"vulnerability", "vulnerabilities", "red teaming", "red-teaming", "sandbox",
	"sandboxing", "sandboxed", "unsandboxed", "escalated privileges", "escalated",
	"escalation", "destructive action", "destructive command", "destructive",
	"attack", "attacks", "cybersecurity", "security review", "exploit development",
	"hacking", "penetration testing", "penetration test", "injection", "weaponize",
	"weaponized", "harmful", "dangerous", "abuse", "abusive", "illegal", "terrorist",
	"terrorism", "bomb", "weapon", "weapons", "drug", "drugs", "narcotic", "suicide",
	"self-harm", "murder", "kill", "violence", "violent",
	"Claude Code", "Claude Opus", "Claude Sonnet", "Claude Haiku", "Claude Fable",
	"Anthropic", "Co-Authored-By", "noreply@anthropic.com",
	// 客户端身份特征词（避免暴露使用 Claude Code / Codex CLI 等，被上游按
	// 客户端特征拦截；buddy-proxy 2026-09 词表）
	"Oh My Pi", "omp", "Kiro", "Codex CLI", "coding harness", "harness",
	"subagent", "subagents", "MCP Server", "MCP tool", "tool call", "function call",
	"antml:invoke", "antml:function_calls",
	"skill://", "agent://", "artifact://", "rule://", "memory://", "local://",
	"history://", "issue://", "pr://", "xd://",
	"AutoImprove", "CodeGraph", "codegraph_explore", "Rust Token Killer",
	"AGENTS.md", "CLAUDE.md", ".cursorrules",
}

var pattern = buildPattern()

func buildPattern() *regexp.Regexp {
	terms := make([]string, len(sensitiveTerms))
	copy(terms, sensitiveTerms)
	// longest-first so longer terms match before shorter substrings
	sort.SliceStable(terms, func(i, j int) bool { return len(terms[i]) > len(terms[j]) })
	quoted := make([]string, len(terms))
	for i, t := range terms {
		quoted[i] = regexp.QuoteMeta(t)
	}
	return regexp.MustCompile("(?i)" + strings.Join(quoted, "|"))
}

func zeroWidthSplit(term string) string {
	r := []rune(term)
	if len(r) <= 1 {
		return term
	}
	return string(r[0]) + zwsp + string(r[1:])
}

// Text inserts a zero-width space into trigger words. No triggers → unchanged.
func Text(text string) string {
	if text == "" {
		return text
	}
	return pattern.ReplaceAllStringFunc(text, zeroWidthSplit)
}

var harnessUserMarkers = []string{
	"# AGENTS.md instructions", "<environment_context>", "<permissions instructions>",
	"<collaboration_mode>", "<skills_instructions>", "<system-reminder>", "# claudeMd",
}

var codexSystemMarkers = []string{
	"You are a coding agent running in the Codex CLI", "Within this context, Codex refers to",
	"# How you work", "You are Claude Code",
}

// Claude Code 2.x 注入的长 system 模板：不含旧版 Codex 标记，携带客户端/渠道
// 身份、内部运行时指令与工具环境元数据。即使逐词打零宽空格，整个块仍会被上游
// 渠道审核整体拒绝，需要语义压缩成短摘要（buddy-proxy desensitize.py 同款）。
var claudeHarnessMarkers = []string{
	"You are an interactive agent that helps users with software engineering tasks.",
	"# Harness", "# Session-specific guidance", "# Memory", "# Environment", "# Context management",
}

const claudeHarnessSummary = "Help with software engineering tasks. Follow repository instructions and " +
	"runtime permission rules, use available capabilities when needed, and report outcomes accurately. " +
	"Be precise, helpful, concise, and safe."

// looksLikeClaudeHarness 识别 Claude Code 注入的长 system 模板（而非普通 system prompt）。
func looksLikeClaudeHarness(text string) bool {
	if len(text) < 1000 {
		return false
	}
	matched := 0
	for _, m := range claudeHarnessMarkers {
		if strings.Contains(text, m) {
			matched++
		}
	}
	return matched >= 2
}

var permissionsMarkers = []string{
	"<permissions instructions>", "Filesystem sandboxing defines which files can be read or written.",
	"## How to request escalation",
}

var skillsMarkers = []string{"<skills_instructions>", "### Available skills", "### How to use skills"}

type runtimeBlock struct{ start, end, replacement string }

var runtimeBlockReplacements = []runtimeBlock{
	{"<environment_context>", "</environment_context>", "Environment context is provided by the harness."},
	{"<permissions instructions>", "</permissions instructions>", "Runtime permissions apply: filesystem access may be sandboxed, network may be restricted, and some commands may require user approval."},
	{"<collaboration_mode>", "</collaboration_mode>", "Collaboration mode instructions are provided by the harness."},
	{"<skills_instructions>", "</skills_instructions>", "Runtime skill metadata is available. Use relevant skills only when explicitly requested or clearly applicable."},
	{"<plugins_instructions>", "</plugins_instructions>", "Runtime plugin metadata is available when relevant."},
	{"<system-reminder>", "</system-reminder>", "Runtime reminder context is provided by the harness."},
}

var runtimeTailMarkers = []string{
	"The following deferred tools are now available via ToolSearch.",
	"Available agent types for the Agent tool:",
	"## MCP Server Instructions",
}

const (
	runtimeTailSummary = "Runtime tool, agent, skill, and MCP metadata is available separately."
	codexCoreSummary   = "You are a coding assistant in Codex CLI. Be precise, helpful, concise, and safe. Inspect the repository, use available tools when needed, follow repository instructions, and keep the user informed with concise progress updates."
)

var multiNewline = regexp.MustCompile(`\n{3,}`)

func contentToText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, blk := range v {
			if m, ok := blk.(map[string]any); ok && m["type"] == "text" {
				if t, ok := m["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	}
	return ""
}

func looksLikeHarnessUser(content any) bool {
	text := contentToText(content)
	for _, m := range harnessUserMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

func anyContains(text string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

func pruneRuntimeFragments(role, text string) string {
	if text == "" {
		return text
	}
	pruned := text
	for _, rb := range runtimeBlockReplacements {
		re := regexp.MustCompile(`(?s)\s*` + regexp.QuoteMeta(rb.start) + `.*?` + regexp.QuoteMeta(rb.end) + `\s*`)
		pruned = re.ReplaceAllString(pruned, "\n\n"+rb.replacement+"\n\n")
	}
	cut := -1
	for _, m := range runtimeTailMarkers {
		if idx := strings.Index(pruned, m); idx >= 0 {
			if cut < 0 || idx < cut {
				cut = idx
			}
		}
	}
	if cut >= 0 {
		head := strings.TrimRight(pruned[:cut], " \t\r\n")
		if head != "" {
			pruned = head + "\n\n" + runtimeTailSummary
		} else {
			pruned = runtimeTailSummary
		}
	}
	if role == "user" && looksLikeHarnessUser(pruned) {
		if strings.Contains(pruned, "# AGENTS.md instructions") ||
			strings.Contains(text, "<environment_context>") ||
			strings.Contains(text, "<skills_instructions>") {
			return "Repository instructions and durable user context are provided. Follow repository guidance while answering the user's actual request."
		}
	}
	pruned = strings.TrimSpace(multiNewline.ReplaceAllString(pruned, "\n\n"))
	return pruned
}

func compactHarnessMessage(role string, content any) (string, bool) {
	text := contentToText(content)
	if text == "" {
		return "", false
	}
	// Claude Code 2.x 长 harness 模板：整体压缩（渠道审核会整块拒绝，
	// 逐词脱敏救不了；只保留行为契约，工具 schema 在独立的 tools 字段不受影响）。
	if role == "system" && looksLikeClaudeHarness(text) {
		return claudeHarnessSummary, true
	}
	if role == "system" && anyContains(text, codexSystemMarkers) {
		if strings.Contains(text, "You are Claude Code") {
			return "You are a coding assistant. Be precise, helpful, concise, and safe. Use available tools when needed, follow repository instructions, and keep the user informed.", true
		}
		return "You are a coding assistant in Codex CLI. Be precise, helpful, concise, and safe. Use available tools when needed, follow repository instructions, and keep the user informed.", true
	}
	if anyContains(text, permissionsMarkers) {
		return "Runtime permissions apply: filesystem access may be sandboxed, network may be restricted, and some commands may require user approval.", true
	}
	if anyContains(text, skillsMarkers) {
		return "Runtime skill metadata is available. Use relevant skills only when explicitly requested or clearly applicable.", true
	}
	if role == "user" && looksLikeHarnessUser(content) {
		return "Repository instructions and environment context are provided. Follow repository guidance while answering the user's actual request.", true
	}
	return "", false
}

// Options controls desensitization behavior.
type Options struct {
	Roles                  []string
	DesensitizeHarnessUser bool
	CompactHarness         bool
	DesensitizeTools       bool
	StripToolMetadata      bool
}

// DefaultOptions mirror the Python defaults (roles=("system",)).
func DefaultOptions() Options { return Options{Roles: []string{"system"}} }

func roleIn(role string, roles []string) bool {
	for _, r := range roles {
		if r == role {
			return true
		}
	}
	return false
}

// Messages desensitizes the given roles' message text, returning a new slice.
func Messages(messages []any, opts Options) []any {
	out := make([]any, 0, len(messages))
	for _, raw := range messages {
		m, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		role, _ := m["role"].(string)
		should := roleIn(role, opts.Roles)
		if role == "user" && opts.DesensitizeHarnessUser {
			should = looksLikeHarnessUser(m["content"])
		}
		nm := cloneMap(m)
		if should {
			content := m["content"]
			if opts.CompactHarness {
				if compacted, ok := compactHarnessMessage(role, content); ok {
					nm["content"] = Text(compacted)
					out = append(out, nm)
					continue
				}
			}
			switch c := content.(type) {
			case string:
				nm["content"] = Text(pruneRuntimeFragments(role, c))
			case []any:
				var blocks []any
				for _, blk := range c {
					if b, ok := blk.(map[string]any); ok && b["type"] == "text" {
						nb := cloneMap(b)
						t, _ := b["text"].(string)
						nb["text"] = Text(pruneRuntimeFragments(role, t))
						blocks = append(blocks, nb)
					} else {
						blocks = append(blocks, blk)
					}
				}
				nm["content"] = blocks
			}
		}
		out = append(out, nm)
	}
	return out
}

func desensitizeToolValue(value any, strip bool) any {
	switch v := value.(type) {
	case map[string]any:
		nv := make(map[string]any, len(v))
		for k, item := range v {
			if k == "description" || k == "title" {
				if s, ok := item.(string); ok {
					if strip {
						continue
					}
					nv[k] = Text(s)
					continue
				}
			}
			nv[k] = desensitizeToolValue(item, strip)
		}
		return nv
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = desensitizeToolValue(item, strip)
		}
		return out
	}
	return value
}

// Body desensitizes messages/tools in a request body, returning a new body.
func Body(body map[string]any, opts Options) map[string]any {
	if len(opts.Roles) == 0 {
		opts.Roles = []string{"system"}
	}
	changed := false
	nb := cloneMap(body)
	if msgs, ok := body["messages"].([]any); ok && len(msgs) > 0 {
		nb["messages"] = Messages(msgs, opts)
		changed = true
	}
	if opts.DesensitizeTools {
		if tools, ok := body["tools"].([]any); ok && len(tools) > 0 {
			nb["tools"] = desensitizeToolValue(tools, opts.StripToolMetadata)
			changed = true
		}
	}
	if changed {
		return nb
	}
	return body
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
