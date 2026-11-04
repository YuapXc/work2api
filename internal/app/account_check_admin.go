package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"work2api/internal/core/provider"
)

func (s *Server) checkTarget(w http.ResponseWriter, r *http.Request) (provider.Runtime, provider.AccountChecker, string) {
	rt, ok := s.o.runtimes.ByName(r.PathValue("name"))
	if !ok {
		writeAPIErr(w, errBody(404, "渠道不存在", "not_found"))
		return nil, nil, ""
	}
	p, ok := rt.(provider.AccountChecker)
	if !ok {
		writeAPIErr(w, errBody(400, "该渠道不支持账号检查", "unsupported"))
		return nil, nil, ""
	}
	id := r.PathValue("id")
	exists := false
	for _, candidate := range p.CheckAccountIDs() {
		if candidate == id {
			exists = true
			break
		}
	}
	if !exists {
		writeAPIErr(w, errBody(404, "账号不存在或不是可检查的独立资源", "not_found"))
		return nil, nil, ""
	}
	return rt, p, id
}
func (s *Server) adminAccountCheck(w http.ResponseWriter, r *http.Request) {
	rt, p, id := s.checkTarget(w, r)
	if rt == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	settings, _ := s.o.db.GetSettings()
	result := s.o.accountChecks.run(ctx, rt, p, id, true, checkPeriod(settings))
	writeJSON(w, 200, result)
}
func canonicalCheckTarget(name, id string) string {
	if name == "qoder" {
		return "qoder:" + id
	}
	if name == "opencode" && id == "anonymous" {
		return "opencode:anonymous"
	}
	return id
}

// The diagnostic writer discards model text after validation; a bounded buffer
// prevents an upstream ignoring the small requested budget from exhausting RAM.
type checkWriter struct {
	header http.Header
	code   int
	body   []byte
	cancel context.CancelFunc
}

func (w *checkWriter) Header() http.Header { return w.header }
func (w *checkWriter) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}
func (w *checkWriter) Write(p []byte) (int, error) {
	if w.code == 0 {
		w.code = 200
	}
	if len(w.body)+len(p) > 256<<10 {
		w.cancel()
		return 0, io.ErrShortBuffer
	}
	w.body = append(w.body, p...)
	return len(p), nil
}
func (s *Server) adminAccountTest(w http.ResponseWriter, r *http.Request) {
	rt, p, id := s.checkTarget(w, r)
	if rt == nil {
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeAPIErr(w, errBody(400, "请选择模型", "invalid_request_error"))
		return
	}
	model := strings.TrimSpace(body.Model)
	found := false
	for _, m := range rt.Models(r.Context()) {
		if physicalModel(m.ID) == physicalModel(model) {
			found = true
			break
		}
	}
	if !found {
		writeAPIErr(w, errBody(400, "请选择该渠道目录中的物理模型，不支持统一路由或别名", "invalid_request_error"))
		return
	}
	payload := map[string]any{"model": model, "messages": []any{map[string]any{"role": "user", "content": "Reply OK."}}, "stream": false, "max_tokens": 64}
	principal := &Principal{AppName: "model-test"}
	if e := s.prepareModel(principal, payload); e != nil {
		writeAPIErr(w, e)
		return
	}
	if rt.Name() == "workbuddy" {
		payload["model"] = strings.TrimPrefix(model, "workbuddy/")
	}
	testMu.Lock()
	if testRunning || time.Since(testLastDone) < testMinInterval {
		testMu.Unlock()
		writeAPIErr(w, errBody(429, "已有模型测试运行或刚结束，请稍后再试", "diagnostic_busy"))
		return
	}
	testRunning = true
	testMu.Unlock()
	defer func() { testMu.Lock(); testRunning = false; testLastDone = time.Now(); testMu.Unlock() }()
	identity := checkIdentity(rt, p, id)
	target := canonicalCheckTarget(rt.Name(), id)
	ctx, cancel := context.WithTimeout(r.Context(), testTimeout)
	defer cancel()
	caller := provider.Caller{AppName: "account-test", TestAccount: target, AccountScope: map[string]bool{target: true, id: true}}
	call := provider.Invocation{Diagnostic: true, Provider: rt.Name(), Caller: caller, LogCapture: &provider.LogCapturePolicy{Enabled: false}, Check: func(ctx context.Context, ref provider.AccountRef, model string) error {
		if live, ok := s.o.runtimes.ByName(rt.Name()); !ok || live != rt {
			return fmt.Errorf("渠道配置已变化，请重新测试")
		}
		if identity != checkIdentity(rt, p, id) {
			return fmt.Errorf("账号凭据已变化，请重新测试")
		}
		return nil
	}}
	ctx = provider.WithInvocation(ctx, call)
	rec := &checkWriter{header: make(http.Header), cancel: cancel}
	start := time.Now()
	report, err := rt.Serve(ctx, provider.ServeRequest{Caller: caller, Protocol: provider.ProtocolChat, Payload: payload, Writer: rec, AppName: "account-test"})
	result := provider.Checked("warning", "指定模型调用", "该账号未返回有效模型响应")
	var response struct {
		Error   any `json:"error"`
		Choices []struct {
			Finish  string `json:"finish_reason"`
			Message struct {
				Content      string `json:"content"`
				Reasoning    string `json:"reasoning_content"`
				ReasoningAlt string `json:"reasoning"`
			} `json:"message"`
		} `json:"choices"`
	}
	if ctx.Err() != nil {
		result.Message = "模型测试超时或取消"
	} else if err == nil && report.Status != "error" && rec.code < 400 && json.Unmarshal(rec.body, &response) == nil && response.Error == nil && len(response.Choices) > 0 {
		result.Status = "valid"
		result.Message = "指定账号已返回有效模型响应"
		choice := response.Choices[0]
		if choice.Finish == "error" || choice.Finish == "content_filter" || strings.TrimSpace(choice.Message.Content+choice.Message.Reasoning+choice.Message.ReasoningAlt) == "" {
			result.Status = "warning"
			result.Message = "上游未返回有效生成内容，未确认调用成功"
		}
		if response.Choices[0].Finish == "length" {
			result.Status = "warning"
			result.Message = "模型已响应，但测试输出预算耗尽"
		}
	} else if rec.code >= 400 {
		result.Message = fmt.Sprintf("指定账号测试失败（HTTP %d）；未切换其他账号", rec.code)
	}
	if report.Status == "incomplete" {
		result.Status = "warning"
		result.Message = "模型已响应，但输出未完整结束"
	}
	result.Model = model
	result.LatencyMS = time.Since(start).Milliseconds()
	result.ExpiresAt = time.Now().Add(30 * time.Minute).Unix()
	if identity != checkIdentity(rt, p, id) {
		result = provider.Checked("unknown", "指定模型调用", "凭据已变化，测试结果不适用于新凭据")
	}
	s.o.accountChecks.saveTest(rt, p, id, identity, result)
	writeJSON(w, 200, result)
}

// This endpoint reads existing observations only; it never refreshes quotas or
// calls model discovery merely because the account page is open.
func (s *Server) adminAccountCheckSnapshots(w http.ResponseWriter, r *http.Request) {
	results := []map[string]any{}
	for _, rt := range s.o.runtimes.Runtimes() {
		p, ok := rt.(provider.AccountChecker)
		if !ok {
			continue
		}
		for _, id := range p.CheckAccountIDs() {
			check, test := s.o.accountChecks.snapshot(rt, p, id)
			results = append(results, map[string]any{"provider": rt.Name(), "id": id, "check": check, "test": test})
		}
	}
	writeJSON(w, 200, map[string]any{"results": results})
}

func (s *Server) adminAccountIdentity(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	rt, _, id := s.checkTarget(w, r)
	if rt == nil {
		return
	}
	inspector, ok := rt.(provider.AccountIdentityInspector)
	if !ok {
		writeAPIErr(w, errBody(400, "该渠道尚未提供出站身份诊断", "unsupported"))
		return
	}
	value, err := inspector.AccountIdentityPreview(id)
	if err != nil {
		writeAPIErr(w, errBody(503, "本地账号身份不可读取，请检查授权文件", "identity_unavailable"))
		return
	}
	writeJSON(w, 200, value)
}
