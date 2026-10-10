package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// ---- 每模型一键测试 ----
//
// 设计要点（与用户确认的口径）：
//   - 只做单模型单次测试，没有批量/全模型入口——避免对上游形成批量探测流量。
//   - 测试走与用户请求完全相同的推理链路，验证的是真实可用性，消耗真实积分。
//   - 后端频控：同一时刻只允许一个测试在跑，且两次测试间隔 ≥5s，防手抖连点。
//   - 请求 max_tokens=64，prompt 限 200 字符，减少单次测试用量；上游不保证严格遵守输出预算。

var (
	testMu       sync.Mutex
	testLastDone time.Time
	testRunning  bool
)

const (
	testMinInterval = 5 * time.Second
	testMaxTokens   = 64
	testMaxPrompt   = 200
	testTimeout     = 90 * time.Second
)

func (s *Server) adminModelTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, errBody(400, "bad json", "invalid_request_error").Body)
		return
	}
	model := strings.TrimSpace(body.Model)
	if model == "" {
		writeJSON(w, 400, errBody(400, "缺少 model", "invalid_request_error").Body)
		return
	}
	prompt := strings.TrimSpace(body.Prompt)
	if prompt == "" {
		prompt = "hi"
	}
	if chars := []rune(prompt); len(chars) > testMaxPrompt {
		prompt = string(chars[:testMaxPrompt])
	}

	// 频控：单飞 + 最小间隔
	testMu.Lock()
	if testRunning {
		testMu.Unlock()
		writeJSON(w, 429, errBody(429, "已有测试在进行，请稍候", "rate_limit_error").Body)
		return
	}
	if since := time.Since(testLastDone); since < testMinInterval {
		wait := int((testMinInterval - since).Seconds()) + 1
		testMu.Unlock()
		writeJSON(w, 429, errBody(429, fmt.Sprintf("测试过于频繁，请 %d 秒后再试", wait), "rate_limit_error").Body)
		return
	}
	testRunning = true
	testMu.Unlock()
	defer func() {
		testMu.Lock()
		testRunning = false
		testLastDone = time.Now()
		testMu.Unlock()
	}()

	chatBody := map[string]any{
		"model":      model,
		"messages":   []any{map[string]any{"role": "user", "content": prompt}},
		"stream":     false,
		"max_tokens": testMaxTokens,
	}
	raw, _ := json.Marshal(chatBody)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(raw)))
	ctx, cancel := context.WithTimeout(r.Context(), testTimeout)
	defer cancel()
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	// 内部发起：以内部 principal 走完整推理链路（与用户请求同路径），
	// 不经过 API key 鉴权。会话键留空，不粘住任何账号。
	principal := &Principal{AppName: "model-test"}
	if aerr := s.prepareModel(principal, chatBody); aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	req, release, admitted := s.admitModel(w, req, principal)
	if !admitted {
		return
	}
	defer release()
	rec := httptest.NewRecorder()

	t0 := time.Now()
	if !s.dispatchRuntimeTo(rec, req, "chat", chatBody, principal) {
		// default workbuddy path
		s.runChatPath(rec, req, chatBody, principal)
	}
	latency := time.Since(t0).Milliseconds()

	var payload map[string]any
	parseOK := json.Unmarshal(rec.Body.Bytes(), &payload) == nil

	resp := map[string]any{
		"ok":         false,
		"model":      model,
		"latency_ms": latency,
	}
	if ctx.Err() != nil {
		resp["status"] = http.StatusGatewayTimeout
		resp["error"] = "模型测试已超时或被取消"
		writeJSON(w, 200, resp)
		return
	}
	if rec.Code >= 400 {
		msg := ""
		if parseOK {
			if e, ok := payload["error"].(map[string]any); ok {
				msg, _ = e["message"].(string)
			}
		}
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", rec.Code)
		}
		resp["status"] = rec.Code
		resp["error"] = msg
		writeJSON(w, 200, resp)
		return
	}
	if parseOK && payload["error"] != nil {
		if e, ok := payload["error"].(map[string]any); ok {
			resp["error"], _ = e["message"].(string)
		} else {
			resp["error"] = fmt.Sprintf("%v", payload["error"])
		}
		writeJSON(w, 200, resp)
		return
	}

	choices, validChoices := payload["choices"].([]any)
	if !parseOK || !validChoices || len(choices) == 0 {
		resp["error"] = "上游未返回有效的模型响应"
		writeJSON(w, 200, resp)
		return
	}
	content := ""
	finish := ""
	var usage map[string]any
	if parseOK {
		if choices, ok := payload["choices"].([]any); ok && len(choices) > 0 {
			if c, ok := choices[0].(map[string]any); ok {
				finish, _ = c["finish_reason"].(string)
				if m, ok := c["message"].(map[string]any); ok {
					if ct, ok := m["content"].(string); ok {
						content = ct
					}
				}
			}
		}
		if u, ok := payload["usage"].(map[string]any); ok {
			usage = u
		}
	}
	content = strings.TrimSpace(content)
	if content == "" {
		content = "(空回复)"
	}
	if chars := []rune(content); len(chars) > 200 {
		content = string(chars[:200])
	}
	resp["ok"] = true
	resp["content"] = content
	resp["finish_reason"] = finish
	resp["usage"] = usage
	writeJSON(w, 200, resp)
}
