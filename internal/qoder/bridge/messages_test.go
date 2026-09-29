package bridge

import "testing"

// 带 tool_calls 的 assistant content=null → content=""（上游拒 null，实测 "" 即 200）
func TestNormalizeOutboundNullContentWithToolCalls(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "hi"},
		map[string]interface{}{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []interface{}{
				map[string]interface{}{"id": "c1", "type": "function",
					"function": map[string]interface{}{"name": "ls", "arguments": "{}"}},
			},
		},
		map[string]interface{}{"role": "tool", "tool_call_id": "c1", "content": "ok"},
	}
	out := normalizeOutboundMessages(msgs)
	a := out[1].(map[string]interface{})
	if a["content"] != "" {
		t.Fatalf("带 tool_calls 的 null content 应补成空串，got %v", a["content"])
	}
	// 普通 assistant 的 content:null 合法，不应凭空造空回复
	plain := []interface{}{map[string]interface{}{"role": "assistant", "content": nil}}
	out2 := normalizeOutboundMessages(plain)
	if out2[0].(map[string]interface{})["content"] != nil {
		t.Fatal("不带 tool_calls 的 null content 不应被改")
	}
}

// developer→system 时摘 tool_calls（tool_calls 只能挂 assistant，否则其后的
// tool 没有 assistant 可配对）；摘除后 content 修正不应再命中
func TestNormalizeOutboundDeveloperRole(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{
			"role":    "developer",
			"content": nil,
			"tool_calls": []interface{}{
				map[string]interface{}{"id": "c1"},
			},
		},
	}
	out := normalizeOutboundMessages(msgs)
	m := out[0].(map[string]interface{})
	if m["role"] != "system" {
		t.Fatalf("developer 应转 system，got %v", m["role"])
	}
	if _, has := m["tool_calls"]; has {
		t.Fatal("developer 的 tool_calls 应被摘掉")
	}
}

func TestNormalizeOutboundPassthrough(t *testing.T) {
	msgs := []interface{}{
		map[string]interface{}{"role": "user", "content": "hi"},
		"not-a-map",
	}
	out := normalizeOutboundMessages(msgs)
	if out[1] != "not-a-map" {
		t.Fatal("非 map 条目应原样保留")
	}
}

// describeUpstreamError：真因在 details（JSON 字符串）里，顶层 message 只有
// "Error in upstream response"
func TestDescribeUpstreamErrorDigsDetails(t *testing.T) {
	frame := map[string]interface{}{
		"code":    "115",
		"message": "Error in upstream response",
		"details": `{"error":{"message":"Messages with role 'tool' must be a response to a preceding message with 'tool_calls'"}}`,
	}
	got := describeUpstreamError(frame)
	if !contains(got, "tool_calls") {
		t.Fatalf("details.error.message 应被挖出，got %q", got)
	}
	if !contains(got, "Error in upstream response") {
		t.Fatalf("顶层 message 应保留，got %q", got)
	}
	// details 非法 JSON：原文保留
	frame2 := map[string]interface{}{"code": "1", "message": "m", "details": "plain text detail"}
	if got := describeUpstreamError(frame2); !contains(got, "plain text detail") {
		t.Fatalf("非 JSON details 应原文保留，got %q", got)
	}
	// 无任何信息
	if got := describeUpstreamError(map[string]interface{}{}); got == "" {
		t.Fatal("空帧应返回兜底文案而非空串")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
