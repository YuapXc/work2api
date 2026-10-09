package upstream

import (
	"encoding/json"
	"strings"
)

// ValidateChoiceCount reflects the single-response contract of our three adapters.
// n counts alternative answers to one request, not concurrent agent requests.
func ValidateChoiceCount(body map[string]any) error {
	if n, exists := body["n"]; exists {
		if count, ok := toInt(n); !ok || count != 1 {
			return invalidStream("WorkBuddy 仅支持 n=1；并行 Agent 请分别发送独立请求")
		}
	}
	return nil
}

type streamedTool struct {
	id, name string
	args     strings.Builder
}

// completionState validates before terminal events reach protocol adapters.
// Normal argument fragments remain untouched; only the complete JSON is checked.
type completionState struct {
	seen, content bool
	finish        string
	tools         map[int]*streamedTool
}

func (s *completionState) observe(chunk map[string]any) error {
	if chunk == nil {
		return invalidStream("上游响应不是 JSON 对象")
	}
	if chunk["error"] != nil || !codeOK(chunk["code"]) {
		raw, _ := json.Marshal(chunk)
		return &UpstreamError{StatusCode: 502, Raw: raw}
	}
	if usage := chunk["usage"]; usage != nil {
		if _, ok := usage.(map[string]any); !ok {
			return invalidStream("上游 usage 格式无效")
		}
	}
	choices, ok := chunk["choices"].([]any)
	if !ok || len(choices) > 1 || (len(choices) == 0 && chunk["usage"] == nil) {
		return invalidStream("上游 choices 格式不符合单回复协议")
	}
	if len(choices) == 0 {
		return nil // A final usage-only chunk is valid, including after finish.
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return invalidStream("上游 choice 格式无效")
	}
	if index, exists := choice["index"]; exists {
		if i, ok := toInt(index); !ok || i != 0 {
			return invalidStream("上游回复 index 无效")
		}
	}
	delta, ok := choice["delta"].(map[string]any)
	if !ok {
		return invalidStream("上游 delta 格式无效")
	}
	finish := ""
	if v := choice["finish_reason"]; v != nil {
		var ok bool
		finish, ok = v.(string)
		if !ok {
			return invalidStream("上游结束原因格式无效")
		}
	}
	if s.finish != "" {
		if (finish != "" && finish != s.finish) || HasOutput(chunk) {
			return invalidStream("上游在回复结束后继续发送内容")
		}
		return nil
	}
	s.seen = true
	for _, key := range []string{"content", "reasoning_content", "refusal"} {
		if v := delta[key]; v != nil {
			text, ok := v.(string)
			if !ok {
				return invalidStream("上游内容增量格式无效")
			}
			s.content = s.content || text != ""
		}
	}
	if v := delta["tool_calls"]; v != nil {
		calls, ok := v.([]any)
		if !ok {
			return invalidStream("上游工具调用格式无效")
		}
		for _, v := range calls {
			call, ok := v.(map[string]any)
			if !ok {
				return invalidStream("上游工具增量格式无效")
			}
			index, ok := toInt(call["index"])
			if !ok || index < 0 || index >= 1024 {
				return invalidStream("上游工具 index 无效")
			}
			if s.tools == nil {
				s.tools = map[int]*streamedTool{}
			}
			t := s.tools[index]
			if t == nil {
				t = &streamedTool{}
				s.tools[index] = t
			}
			if v := call["type"]; v != nil && v != "" && v != "function" {
				return invalidStream("上游工具类型不受支持")
			}
			if err := stableToolField(call["id"], &t.id); err != nil {
				return err
			}
			if v := call["function"]; v != nil {
				fn, ok := v.(map[string]any)
				if !ok {
					return invalidStream("上游 function 格式无效")
				}
				if err := stableToolField(fn["name"], &t.name); err != nil {
					return err
				}
				if v := fn["arguments"]; v != nil {
					args, ok := v.(string)
					if !ok {
						return invalidStream("上游工具参数增量不是字符串")
					}
					t.args.WriteString(args)
				}
			}
		}
	}
	s.finish = finish
	if finish != "" {
		return s.complete(false)
	}
	return nil
}

func stableToolField(v any, dst *string) error {
	if v == nil {
		return nil
	}
	text, ok := v.(string)
	if !ok || (*dst != "" && text != "" && *dst != text) {
		return invalidStream("上游工具标识发生变化或格式无效")
	}
	if text != "" {
		*dst = text
	}
	return nil
}

func (s *completionState) complete(allowToolFinish bool) error {
	if !s.seen {
		return invalidStream("上游没有返回模型回复")
	}
	if s.finish == "length" || s.finish == "content_filter" {
		return nil // Truncation/filtering is not a complete executable tool call.
	}
	if s.finish == "" && (!allowToolFinish || len(s.tools) == 0) {
		return invalidStream("上游流缺少结束原因，结果可能不完整")
	}
	if s.finish == "tool_calls" && len(s.tools) == 0 {
		return invalidStream("上游声明工具结束但未提供工具调用")
	}
	if len(s.tools) > 0 {
		if s.finish != "" && s.finish != "tool_calls" {
			return invalidStream("上游工具调用与结束原因不一致")
		}
		ids := map[string]bool{}
		for _, t := range s.tools {
			var args map[string]any
			if t.id == "" || t.name == "" || ids[t.id] || json.Unmarshal([]byte(t.args.String()), &args) != nil || args == nil {
				return invalidStream("上游工具调用不完整或参数不是有效 JSON 对象")
			}
			ids[t.id] = true
		}
	} else if !s.content {
		return invalidStream("上游没有正文、思考或工具调用")
	}
	return nil
}

// HasOutput distinguishes response-bearing deltas from role/usage preambles.
// Unknown extensions are conservatively output, so they are never replayed.
func HasOutput(chunk map[string]any) bool {
	choices, _ := chunk["choices"].([]any)
	for _, v := range choices {
		choice, _ := v.(map[string]any)
		delta, _ := choice["delta"].(map[string]any)
		for key, value := range delta {
			if key == "role" || empty(value) {
				continue
			}
			if key == "function_call" {
				if f, ok := value.(map[string]any); ok && empty(f["name"]) && empty(f["arguments"]) && len(f) <= 2 {
					continue
				}
			}
			return true
		}
	}
	return false
}
