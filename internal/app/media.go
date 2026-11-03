package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"work2api/internal/core/provider"
	"work2api/internal/store"
)

func mediaError(w http.ResponseWriter, err error) {
	var ae *provider.APIError
	if errors.As(err, &ae) {
		writeAPIErr(w, ae)
	} else {
		writeAPIErr(w, errBody(502, "媒体服务暂不可用", "upstream_error"))
	}
}

func (s *Server) readMediaRequest(r *http.Request) (map[string]any, error) {
	if !isMediaMultipart(r) {
		return readJSON(r)
	}
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, err
	}
	payload := map[string]any{}
	images := []any{}
	for count := 0; ; count++ {
		if s.o.cfg.MaxJSONItems > 0 && count >= s.o.cfg.MaxJSONItems {
			return nil, errors.New("表单结构过于复杂")
		}
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		name := part.FormName()
		data, err := io.ReadAll(part)
		part.Close()
		if err != nil {
			return nil, err
		}
		if name == "file" && r.URL.Path == "/v1/audio/transcriptions" {
			if payload["audio_data"] != nil {
				return nil, errors.New("仅支持一个音频文件")
			}
			kind := part.Header.Get("Content-Type")
			if !strings.HasPrefix(kind, "audio/") && !strings.HasPrefix(kind, "video/") {
				kind = http.DetectContentType(data)
			}
			payload["audio_data"] = map[string]any{"data": base64.StdEncoding.EncodeToString(data), "mime": kind}
			continue
		}
		if name == "image" || name == "image[]" {
			kind := http.DetectContentType(data)
			if !strings.HasPrefix(kind, "image/") {
				return nil, errors.New("image 文件必须是图片")
			}
			images = append(images, "data:"+kind+";base64,"+base64.StdEncoding.EncodeToString(data))
			continue
		}
		if part.FileName() != "" {
			return nil, errors.New("不支持的文件字段")
		}
		if _, duplicate := payload[name]; duplicate {
			return nil, errors.New("表单参数不能重复")
		}
		if name == "n" {
			n, err := strconv.Atoi(string(data))
			if err != nil {
				return nil, err
			}
			payload[name] = float64(n)
		} else {
			payload[name] = string(data)
		}
	}
	payload["image"] = images
	return payload, nil
}

func isMediaMultipart(r *http.Request) bool {
	kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return (r.URL.Path == "/v1/images/edits" || r.URL.Path == "/v1/audio/transcriptions") && kind == "multipart/form-data"
}

func (s *Server) handleMedia(w http.ResponseWriter, r *http.Request) {
	p, aerr := s.auth(r)
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	payload, err := s.readMediaRequest(r)
	if err != nil {
		writeAPIErr(w, errBody(400, "无效的媒体请求体", "invalid_request_error"))
		return
	}
	if model, ok := payload["model"].(string); !ok || strings.TrimSpace(model) == "" {
		writeAPIErr(w, errBody(400, "请明确指定媒体 model", "invalid_request_error"))
		return
	}
	if aerr = s.prepareModel(p, payload); aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	if p.RouteModel != "" {
		writeAPIErr(w, errBody(400, "媒体生成请使用实际渠道模型名", "invalid_request_error"))
		return
	}
	resolved := s.o.resolveModel(strOr(payload["model"], ""))
	rt, ok := s.o.runtimes.Resolve(resolved)
	if !ok {
		writeAPIErr(w, errBody(400, "媒体渠道不可用", "invalid_request_error"))
		return
	}
	media, ok := rt.(provider.MediaProvider)
	if !ok {
		writeAPIErr(w, errBody(400, "该渠道尚不支持媒体生成", "invalid_request_error"))
		return
	}
	var refreshed bool
	r, refreshed = s.refreshPortalRequest(w, r, p, payload)
	if !refreshed {
		return
	}
	call := provider.CurrentInvocation(r.Context())
	call.Provider = rt.Name()
	call.Caller = p.SessionCaller()
	ctx := s.o.invocationContext(provider.WithInvocation(r.Context(), call))
	operation, kind := "image", "image"
	if r.URL.Path == "/v1/media/uploads" {
		operation, kind = "video", "video"
		payload["prompt"] = "reference upload"
	}
	if r.URL.Path == "/v1/images/edits" {
		operation = "image-edit"
	}
	if r.URL.Path == "/v1/audio/transcriptions" {
		operation, kind = "transcription", "audio"
	}
	if r.URL.Path == "/v1/videos" {
		operation, kind = "video", "video"
	}
	providerPayload := make(map[string]any, len(payload))
	for k, v := range payload {
		providerPayload[k] = v
	}
	providerPayload["model"] = resolved
	request, err := media.PrepareMedia(ctx, operation, providerPayload, p.SessionCaller())
	if err != nil {
		mediaError(w, err)
		return
	}
	if r.URL.Path == "/v1/media/uploads" {
		uploader, ok := rt.(provider.MediaUploader)
		if !ok {
			writeAPIErr(w, errBody(400, "该渠道不支持素材上传", "unsupported_operation"))
			return
		}
		data, ok := payload["file"].(map[string]any)
		if !ok {
			writeAPIErr(w, errBody(400, "需要 file 对象包含 data 和 mime", "invalid_request_error"))
			return
		}
		uploaded, err := uploader.UploadMedia(ctx, request, data)
		if err != nil {
			mediaError(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"url": uploaded})
		return
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		mediaError(w, err)
		return
	}
	id := kind + "_" + hex.EncodeToString(random[:])
	task := store.MediaTask{ID: id, Provider: rt.Name(), AccountUID: request.AccountID, AppID: p.AppID, UserID: p.UserID, Model: resolved, RequestModel: strOr(payload["model"], resolved), Kind: kind, Metadata: mediaMetadata(request)}
	if err = s.o.db.CreateMediaTask(task, p.AppName); err != nil {
		writeAPIErr(w, errBody(503, "任务保存失败，未提交上游", "server_error"))
		return
	}
	w.Header().Set("X-Media-Task-Id", id)
	result, err := media.CallMedia(ctx, request)
	if err != nil {
		status := "unknown"
		var ae *provider.APIError
		if errors.As(err, &ae) && ae.Status >= 400 && ae.Status < 500 {
			status = "failed"
		}
		_ = s.o.db.UpdateMediaTask(id, "", status, map[string]any{"id": id, "status": status}, nil, "媒体提交未成功，详情见客户端响应")
		if errors.As(err, &ae) {
			ae.Body["media_task_id"] = id
		}
		mediaError(w, err)
		return
	}
	if kind == "image" {
		items, ok := result["data"].([]any)
		if !ok || len(items) == 0 {
			_ = s.o.db.UpdateMediaTask(id, "", "unknown", map[string]any{"id": id, "status": "unknown"}, mediaCredit(result), "上游未返回图片，请勿重复提交")
			writeAPIErr(w, errBody(502, "上游未返回图片，请勿重复提交", "media_outcome_unknown"))
			return
		}
		output := map[string]any{"created": result["created"], "data": items}
		if output["created"] == nil {
			output["created"] = time.Now().Unix()
		}
		if v, ok := result["usage"]; ok {
			output["usage"] = v
		}
		if err = s.o.db.UpdateMediaTask(id, "", "completed", storedImageResult(id, output), mediaCredit(result), ""); err != nil {
			w.Header().Set("X-Media-Log-Status", "pending")
		}
		writeJSON(w, 200, output)
		return
	}
	upstreamID, _ := result["id"].(string)
	if kind == "audio" {
		upstreamID, _ = result["task_id"].(string)
	}
	if upstreamID == "" {
		_ = s.o.db.UpdateMediaTask(id, "", "unknown", map[string]any{"id": id, "status": "unknown"}, nil, "上游未返回任务标识")
		writeAPIErr(w, errBody(502, "上游未返回任务标识，请勿重复提交", "media_outcome_unknown"))
		return
	}
	output := map[string]any{"id": id, "object": kind, "model": resolved, "status": "queued", "created_at": time.Now().Unix()}
	if err = s.o.db.UpdateMediaTask(id, upstreamID, "queued", output, mediaCredit(result), ""); err != nil {
		writeAPIErr(w, errBody(503, "上游已受理，但任务状态保存失败；请勿重复提交", "media_outcome_unknown"))
		return
	}
	if kind == "audio" && payload["async"] != true && payload["async"] != "true" {
		s.awaitTranscription(w, r, ctx, media, request, id, upstreamID, strOr(payload["response_format"], "json"))
		return
	}
	writeJSON(w, http.StatusAccepted, output)
}

func mediaCredit(result map[string]any) *float64 {
	usage, _ := result["usage"].(map[string]any)
	if n, ok := usage["credit"].(float64); ok && n >= 0 && !math.IsNaN(n) && !math.IsInf(n, 0) {
		return &n
	}
	return nil
}

func (s *Server) handleVideoTask(w http.ResponseWriter, r *http.Request) {
	p, aerr := s.auth(r)
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	t, err := s.o.db.MediaTask(r.PathValue("id"))
	if err != nil {
		mediaError(w, err)
		return
	}
	if t == nil || r.Pattern == "GET /v1/videos/{id}" && t.Kind != "video" || t.AppID != p.AppID || t.UserID != p.UserID {
		writeAPIErr(w, errBody(404, "任务不存在", "not_found"))
		return
	}
	requested := t.RequestModel
	if requested == "" {
		requested = t.Model
	}
	payload := map[string]any{"model": requested}
	if aerr = s.prepareModel(p, payload); aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	var refreshed bool
	r, refreshed = s.refreshPortalRequest(w, r, p, payload)
	if !refreshed {
		return
	}
	if p.EffectiveModel != t.Model || p.UserID > 0 && !p.AccountScope[t.AccountUID] {
		writeAPIErr(w, errBody(403, "任务模型或账号权限已改变", "model_not_allowed"))
		return
	}
	if validate, ok := r.Context().Value(portalDispatchCheckKey{}).(func(string, string) *apiError); ok {
		if aerr := validate(t.AccountUID, t.Model); aerr != nil {
			writeAPIErr(w, aerr)
			return
		}
	}
	if t.Status == "completed" || t.Status == "failed" || t.Status == "cancelled" || t.UpstreamID == "" {
		if t.Status == "submitting" && t.UpstreamID == "" {
			t.Status = "unknown"
		}
		if len(t.Response) == 0 {
			t.Response = map[string]any{"id": t.ID, "status": t.Status, "model": t.Model}
		}
		writeJSON(w, 200, t.Response)
		return
	}
	rt, ok := s.o.runtimes.Resolve(t.Model)
	if !ok || rt.Name() != t.Provider {
		writeAPIErr(w, errBody(503, "原任务渠道不可用", "upstream_error"))
		return
	}
	media, ok := rt.(provider.MediaProvider)
	if !ok {
		writeAPIErr(w, errBody(503, "原任务渠道不可用", "upstream_error"))
		return
	}
	call := provider.CurrentInvocation(r.Context())
	call.Provider = t.Provider
	call.Caller = p.SessionCaller()
	ctx := s.o.invocationContext(provider.WithInvocation(r.Context(), call))
	queryOperation := "video-query"
	if t.Kind == "audio" {
		queryOperation = "transcription-query"
	}
	result, err := media.CallMedia(ctx, provider.MediaRequest{Operation: queryOperation, Model: strings.TrimPrefix(t.Model, "workbuddy/"), AccountID: t.AccountUID, Payload: map[string]any{"task_id": t.UpstreamID}})
	if err != nil {
		mediaError(w, err)
		return
	}
	status, _ := result["status"].(string)
	if status == "pending" {
		status = "queued"
	}
	if status == "processing" {
		status = "in_progress"
	}
	if status == "" {
		status = "unknown"
	}
	output := map[string]any{"id": t.ID, "object": t.Kind, "model": t.Model, "status": status, "created_at": int64(t.CreatedAt)}
	if status == "completed" && t.Kind == "audio" {
		transcript, _ := result["audio_transcription"].(map[string]any)
		output["text"] = transcript["text"]
	}
	if status == "completed" && t.Kind == "video" {
		items, ok := result["videos"].([]any)
		if !ok {
			items, _ = result["data"].([]any)
		}
		if len(items) == 0 {
			writeAPIErr(w, errBody(502, "任务已完成但尚无视频产物，请稍后查询原任务", "media_result_unavailable"))
			return
		}
		output["data"] = items
	}
	if v, ok := result["usage"]; ok {
		output["usage"] = v
	}
	message := ""
	if status == "failed" || status == "cancelled" {
		message = "上游媒体任务" + status
		output["error"] = map[string]any{"message": message}
		for _, key := range []string{"error", "message", "msg", "reason", "fail_reason"} {
			if v, ok := result[key]; ok {
				output[key] = v
			}
		}
	}
	if err = s.o.db.UpdateMediaTask(t.ID, t.UpstreamID, status, output, mediaCredit(result), message); err != nil {
		writeAPIErr(w, errBody(503, "任务状态保存失败，可继续查询原任务", "server_error"))
		return
	}
	writeJSON(w, 200, output)
}

func mediaMetadata(req provider.MediaRequest) map[string]any {
	out := map[string]any{"operation": req.Operation}
	for _, k := range []string{"size", "n", "seconds"} {
		if v, ok := req.Payload[k]; ok {
			out[k] = v
		}
	}
	if extra, ok := req.Payload["extra_parameters"].(map[string]any); ok {
		for _, k := range []string{"resolution", "aspect_ratio", "enable_audio"} {
			if v, ok := extra[k]; ok {
				out[k] = v
			}
		}
	}
	return out
}

func storedImageResult(id string, result map[string]any) map[string]any {
	out := map[string]any{"id": id, "status": "completed", "created": result["created"], "usage": result["usage"]}
	items, _ := result["data"].([]any)
	urls := []any{}
	for _, v := range items {
		m, _ := v.(map[string]any)
		if url, ok := m["url"].(string); ok && strings.HasPrefix(url, "https://") {
			urls = append(urls, map[string]any{"url": url})
		}
	}
	out["data"] = urls
	if len(urls) != len(items) {
		out["result_ephemeral"] = true
	}
	return out
}
func (s *Server) handleMediaTaskList(w http.ResponseWriter, r *http.Request) {
	p, aerr := s.auth(r)
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	before, _ := strconv.ParseFloat(r.URL.Query().Get("before"), 64)
	tasks, err := s.o.db.MediaTaskList(p.AppID, p.UserID, before)
	if err != nil {
		mediaError(w, err)
		return
	}
	visible := []map[string]any{}
	for _, task := range tasks {
		if s.authorizeModel(p, strOr(task["requested_model"], strOr(task["model"], ""))) == nil {
			visible = append(visible, task)
		}
	}
	next := float64(0)
	if len(tasks) == 50 {
		next, _ = tasks[len(tasks)-1]["created_at"].(float64)
	}
	writeJSON(w, 200, map[string]any{"data": visible, "next_before": next})
}

func (s *Server) awaitTranscription(w http.ResponseWriter, r *http.Request, ctx context.Context, media provider.MediaProvider, req provider.MediaRequest, id, upstreamID, format string) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	for {
		timer := time.NewTimer(4 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			if r.Context().Err() == nil {
				writeJSON(w, 202, map[string]any{"id": id, "status": "in_progress"})
			}
			return
		case <-timer.C:
		}
		result, err := media.CallMedia(ctx, provider.MediaRequest{Operation: "transcription-query", Model: req.Model, AccountID: req.AccountID, Payload: map[string]any{"task_id": upstreamID}})
		if err != nil {
			mediaError(w, err)
			return
		}
		status, _ := result["status"].(string)
		transcript, _ := result["audio_transcription"].(map[string]any)
		output := map[string]any{"id": id, "status": status, "text": transcript["text"], "usage": result["usage"]}
		message := ""
		if status == "failed" {
			message = "上游音频转写失败"
			output["error"] = result["error"]
		}
		if err = s.o.db.UpdateMediaTask(id, upstreamID, status, output, mediaCredit(result), message); err != nil {
			mediaError(w, err)
			return
		}
		if status == "failed" {
			writeAPIErr(w, errBody(502, message, "transcription_failed"))
			return
		}
		if status == "completed" {
			if format == "text" {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				_, _ = io.WriteString(w, strOr(transcript["text"], ""))
			} else {
				writeJSON(w, 200, map[string]any{"text": transcript["text"], "usage": result["usage"]})
			}
			return
		}
	}
}
