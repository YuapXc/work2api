package bridge

import "testing"

// imageContentParts：三种入站图片格式都归一成上游 image_url 形式
func TestImageContentParts(t *testing.T) {
	// OpenAI: image_url {url}
	parts, has := imageContentParts([]interface{}{
		map[string]interface{}{"type": "text", "text": "看这张图"},
		map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://x/y.png"}},
	})
	if !has || len(parts) != 2 {
		t.Fatalf("OpenAI image_url 应被识别, has=%v parts=%v", has, parts)
	}
	// Anthropic base64 → data: URL
	parts, has = imageContentParts([]interface{}{
		map[string]interface{}{"type": "image", "source": map[string]interface{}{
			"type": "base64", "media_type": "image/jpeg", "data": "QUJD"}},
	})
	if !has || len(parts) != 1 {
		t.Fatalf("Anthropic base64 应转 data URL, has=%v parts=%v", has, parts)
	}
	img := parts[0].(map[string]interface{})["image_url"].(map[string]interface{})
	if img["url"] != "data:image/jpeg;base64,QUJD" {
		t.Fatalf("data URL 不对: %v", img["url"])
	}
	// Anthropic url source
	parts, has = imageContentParts([]interface{}{
		map[string]interface{}{"type": "image", "source": map[string]interface{}{"type": "url", "url": "https://x/y.png"}},
	})
	if !has {
		t.Fatal("Anthropic url source 应被识别")
	}
	// Responses input_image（字符串形式）
	_, has = imageContentParts([]interface{}{
		map[string]interface{}{"type": "input_image", "image_url": "https://x/y.png"},
	})
	if !has {
		t.Fatal("input_image 字符串形式应被识别")
	}
	// 无图 → hasImage=false
	_, has = imageContentParts([]interface{}{map[string]interface{}{"type": "text", "text": "纯文本"}})
	if has {
		t.Fatal("纯文本不应报 hasImage")
	}
	// 缺 url 的图片块被跳过（不发无 url 的 image_url 上游）
	parts, has = imageContentParts([]interface{}{
		map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{}},
		map[string]interface{}{"type": "text", "text": "t"},
	})
	if has || len(parts) != 1 {
		t.Fatalf("缺 url 的图片块应被跳过, has=%v parts=%v", has, parts)
	}
}

// ConvertIncomingMessage：user 消息含图时 content 用数组且不带 contents
func TestConvertIncomingUserMessageWithImage(t *testing.T) {
	in := map[string]interface{}{
		"role": "user",
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "图里是什么"},
			map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://x/y.png"}},
		},
	}
	out := ConvertIncomingMessage(in, false)
	if out == nil {
		t.Fatal("应转换成功")
	}
	if _, hasContents := out["contents"]; hasContents {
		t.Fatal("含图 user 消息不得再带 contents（上游只读 contents，图片会丢）")
	}
	arr, ok := out["content"].([]interface{})
	if !ok || len(arr) != 2 {
		t.Fatalf("content 应为含图数组, got %v", out["content"])
	}
	if arr[1].(map[string]interface{})["type"] != "image_url" {
		t.Fatalf("第二块应为 image_url, got %v", arr[1])
	}
	// 纯文本 user 消息行为不变（contents 形式）
	plain := map[string]interface{}{"role": "user", "content": "hi"}
	out2 := ConvertIncomingMessage(plain, false)
	if _, hasContents := out2["contents"]; !hasContents {
		t.Fatalf("纯文本 user 应保留 contents 形式, got %v", out2)
	}
}

// extractModels：上游 model/list 条目的 is_vl 应被读出
func TestExtractModelsReadsIsVL(t *testing.T) {
	raw := []interface{}{
		map[string]interface{}{
			"key": "qwen38flash", "display_name": "Qwen3.8 Flash",
			"enable": true, "is_vl": true, "is_reasoning": false,
			"max_input_tokens": 131072, "price_factor": 0.1,
		},
		map[string]interface{}{
			"key": "dmodel", "enable": true, "is_vl": false,
		},
	}
	models := extractModels(raw)
	if len(models) != 2 {
		t.Fatalf("应解析出 2 个模型, got %d", len(models))
	}
	if !models[0].IsVL {
		t.Fatal("is_vl=true 的条目应读出 IsVL=true")
	}
	if models[1].IsVL {
		t.Fatal("is_vl=false 的条目 IsVL 应为 false")
	}
}

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
