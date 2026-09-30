package bridge

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"work2api/internal/qoder/cosy"

	"work2api/internal/qoder/logger"
)

type Delta struct {
	Role            string
	Content         string
	Reasoning       string // 上游推理过程（reasoning_content）
	ToolCalls       []interface{}
	InputTokens     int
	OutputTokens    int
	HasInputTokens  bool
	HasOutputTokens bool
	Err             error // 上游返回业务错误时非 nil
}

func (d Delta) isEmpty() bool {
	return d.Role == "" && d.Content == "" && d.Reasoning == "" && d.ToolCalls == nil && d.InputTokens == 0 && d.OutputTokens == 0 && !d.HasInputTokens && !d.HasOutputTokens && d.Err == nil
}

// UpdateUsage applies cumulative snapshots, including explicit zero values,
// without resetting a field omitted by a later frame or summing snapshots.
func (d Delta) UpdateUsage(input, output *int) {
	if d.HasInputTokens || d.InputTokens > 0 {
		*input = d.InputTokens
	}
	if d.HasOutputTokens || d.OutputTokens > 0 {
		*output = d.OutputTokens
	}
}

func ExtractDelta(dataLine string) Delta {
	var wrapper map[string]interface{}
	if err := json.Unmarshal([]byte(dataLine), &wrapper); err != nil {
		logger.Debug("[delta] unmarshal wrapper failed: %v, raw=%s", err, dataLine)
		return Delta{}
	}
	// ---- 信封状态检查（第 3 步，hub iter_inner_sse 形态） ----
	// 上游帧：data:{"headers":{...},"body":"<内层 chunk>","statusCodeValue":200}
	// 关键形态：上游 HTTP200 建流后，把 provider 故障包在信封里投递
	// statusCodeValue=418/5xx（access log 记 200 而业务错的根因）。
	// 与内层业务 code（body 里的 {"code":"115"}）严格区分：信封错误带真实
	// HTTP 状态，可参与瞬时重试判定；重开闸门在 CallQoderWithOpts。
	if raw, ok := wrapper["statusCodeValue"]; ok {
		if status := toEnvelopeStatus(raw); status != 200 {
			ue := NewUpstreamError(status, envelopeDetail(wrapper))
			logger.Error("[delta] envelope statusCodeValue=%v -> %s", raw, ue.Message)
			return Delta{Err: ue}
		}
	}
	inner, _ := wrapper["body"].(string)
	if inner == "" {
		logger.Debug("[delta] no body field, keys=%v, raw=%s", mapKeys(wrapper), dataLine)
		return Delta{}
	}
	var innerJSON map[string]interface{}
	if err := json.Unmarshal([]byte(inner), &innerJSON); err != nil {
		logger.Debug("[delta] unmarshal inner failed: %v", err)
		return Delta{}
	}
	// 先捕获顶层 usage（上游可能在最后一个 chunk 中与 choices 一起返回），
	// 不提前 return：同帧若还带 choices 内容，合并进同一个 Delta，避免内容丢失
	var usageIn, usageOut int
	var hasIn, hasOut bool
	if usage, ok := innerJSON["usage"].(map[string]interface{}); ok {
		_, hasIn = usage["prompt_tokens"]
		_, hasOut = usage["completion_tokens"]
		usageIn = int(cosy.FloatVal(usage, "prompt_tokens"))
		usageOut = int(cosy.FloatVal(usage, "completion_tokens"))
	}
	choices, _ := innerJSON["choices"].([]interface{})
	for _, ch := range choices {
		chMap, _ := ch.(map[string]interface{})
		delta, _ := chMap["delta"].(map[string]interface{})
		if delta == nil {
			continue
		}
		role, _ := delta["role"].(string)
		content, _ := delta["content"].(string)
		reasoning, _ := delta["reasoning_content"].(string)
		var toolCalls []interface{}
		if tc, ok := delta["tool_calls"].([]interface{}); ok && len(tc) > 0 {
			toolCalls = tc
		}
		if role != "" || content != "" || reasoning != "" || toolCalls != nil {
			return Delta{Role: role, Content: content, Reasoning: reasoning, ToolCalls: toolCalls,
				InputTokens: usageIn, OutputTokens: usageOut, HasInputTokens: hasIn, HasOutputTokens: hasOut}
		}
	}
	// 上游业务错误：{"code":"115","message":"..."}
	// 流内业务错误（HTTP 200 帧内）：非内容审核不做瞬时重试指引，
	// 内容审核单独映射为 content_policy_rejected。
	// 注意：业务错误检查仍先于 usage-only 返回，错误帧不因带 usage 被误判为正常帧。
	if code, ok := innerJSON["code"].(string); ok && code != "" && code != "0" {
		err := NewStreamBusinessError(fmt.Sprintf("upstream error code=%s: %s", code, describeUpstreamError(innerJSON)))
		logger.Error("[delta] %v", err)
		return Delta{Err: err}
	}
	// choices 与业务 code 均未命中且 usage > 0：维持返回 usage-only Delta（行为同修复前）
	if hasIn || hasOut {
		return Delta{InputTokens: usageIn, OutputTokens: usageOut, HasInputTokens: hasIn, HasOutputTokens: hasOut}
	}
	logger.Debug("[delta] no valid choices found, inner=%s", inner)
	return Delta{}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// describeUpstreamError 把带内错误帧转成可读原因。
//
// 上游把真正的失败原因放在 details 里（JSON 字符串），顶层 message 只有一句
// 没用的 "Error in upstream response"。只取 message 会让「模型不存在」「参数
// 非法」「渠道校验拦截」全都退化成同一句话，线上只能靠猜——所以把
// details.error.message 一并挖出（buddy-proxy #45 同款）。
func describeUpstreamError(frame map[string]interface{}) string {
	parts := []string{}
	if m, _ := frame["message"].(string); m != "" {
		parts = append(parts, m)
	}
	detailMsg := ""
	switch d := frame["details"].(type) {
	case string:
		if s := strings.TrimSpace(d); s != "" {
			var parsed map[string]interface{}
			if err := json.Unmarshal([]byte(s), &parsed); err != nil {
				detailMsg = s
			} else if errObj, ok := parsed["error"].(map[string]interface{}); ok {
				detailMsg, _ = errObj["message"].(string)
			} else if m, ok := parsed["message"].(string); ok {
				detailMsg = m
			}
		}
	case map[string]interface{}:
		if errObj, ok := d["error"].(map[string]interface{}); ok {
			detailMsg, _ = errObj["message"].(string)
		}
	}
	if detailMsg != "" {
		parts = append(parts, detailMsg)
	}
	if len(parts) == 0 {
		return "上游返回未知错误"
	}
	return truncate(strings.Join(parts, " | "), 500)
}

func mapKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// toEnvelopeStatus 信封 statusCodeValue 可能是 int/float64/str，统一成 int
// （解析失败回 502，按瞬时故障处理）。
func toEnvelopeStatus(v interface{}) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, err := strconv.Atoi(x)
		if err != nil {
			return 502
		}
		return n
	}
	return 502
}

// envelopeDetail 提取信封错误详情：body 为字符串时直接用（错误详情在内层），
// 否则截断整帧 JSON 便于排查。
func envelopeDetail(wrapper map[string]interface{}) string {
	if body, ok := wrapper["body"].(string); ok && body != "" {
		return body
	}
	b, _ := json.Marshal(wrapper)
	return truncate(string(b), 400)
}
