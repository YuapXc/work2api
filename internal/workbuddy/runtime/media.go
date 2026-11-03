package runtime

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"work2api/internal/core/provider"
	"work2api/internal/workbuddy/httpclient"
	"work2api/internal/workbuddy/siterouting"
)

var defaultMediaClient = func() *http.Client {
	c := httpclient.New(180 * time.Second)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}()

func (o *Runtime) PrepareMedia(ctx context.Context, operation string, payload map[string]any, caller provider.Caller) (provider.MediaRequest, error) {
	model := strings.TrimPrefix(ToStr(payload["model"]), "workbuddy/")
	entry, ready := o.Catalog.CachedModel(model)
	if !ready || entry == nil {
		return provider.MediaRequest{}, errBody(400, "媒体模型不在当前目录中，请刷新目录并检查模型名", "invalid_request_error")
	}
	tag := "text-to-image"
	kind := "image"
	switch operation {
	case "transcription":
		tag, kind = "audio-to-text", "audio"
	case "image-edit":
		tag = "image-to-image"
	case "video":
		kind = "video"
		tag = "text-to-video"
		if payload["image_url"] != nil || payload["last_image_url"] != nil || mediaArrayLen(payload["reference_images"]) > 0 || mediaArrayLen(payload["reference_videos"]) > 0 {
			tag = "image-to-video"
		}
	case "image":
	default:
		return provider.MediaRequest{}, provider.ErrUnsupported
	}
	tags, _ := entry["media_tags"].([]any)
	found := false
	for _, v := range tags {
		if v == tag {
			found = true
		}
	}
	if entry["model_type"] != kind || !found {
		return provider.MediaRequest{}, errBody(400, "该模型不支持请求的媒体生成方式", "invalid_request_error")
	}
	allowed := o.ModelEntryAccountUIDs(entry)
	if caller.UserID > 0 || caller.TestAccount != "" {
		for uid := range allowed {
			if !caller.AccountScope[uid] {
				delete(allowed, uid)
			}
		}
	} else {
		excluded, err := o.db.PrivatePoolExcludedUIDs()
		if err != nil {
			return provider.MediaRequest{}, errBody(503, "账号权限查询失败", "server_error")
		}
		for uid := range excluded {
			delete(allowed, uid)
		}
	}
	for uid := range allowed {
		own := o.Catalog.AccountMedia(uid, model)
		if own == nil {
			delete(allowed, uid)
			continue
		}
		tags, _ := own["media_tags"].([]any)
		valid := false
		for _, value := range tags {
			if value == tag {
				valid = true
			}
		}
		if !valid {
			delete(allowed, uid)
		}
	}
	acc, aerr := o.PickAccountExcludingIn(model, "", nil, allowed)
	if aerr != nil {
		return provider.MediaRequest{}, aerr
	}
	own := o.Catalog.AccountMedia(acc.UID, model)
	body, err := buildMediaBody(operation, model, payload, own)
	if err != nil {
		return provider.MediaRequest{}, err
	}
	if err = provider.CheckDispatch(ctx, provider.AccountRef{Provider: o.Name(), LocalID: acc.UID}, model); err != nil {
		return provider.MediaRequest{}, err
	}
	return provider.MediaRequest{Operation: operation, Model: model, AccountID: acc.UID, Payload: body}, nil
}

func buildMediaBody(operation, model string, payload, entry map[string]any) (map[string]any, error) {
	bad := func(message string) (map[string]any, error) {
		return nil, errBody(400, message, "invalid_request_error")
	}
	if operation == "transcription" {
		if format, ok := payload["response_format"]; ok && format != "json" && format != "text" {
			return bad("转写 response_format 仅支持 json 或 text")
		}
		audio, ok := payload["audio_url"].(string)
		if !ok && payload["audio_data"] == nil {
			return bad("请提供 audio_url 或上传音频文件")
		}
		if ok && !mediaInputURL(audio) {
			return bad("audio_url 必须为 HTTP(S) 地址")
		}
		return map[string]any{"model": model, "audio_transcription": map[string]any{"audio_url": audio, "channel_num": 1, "speaker_diarization": 1}, "_audio_data": payload["audio_data"]}, nil
	}
	prompt, ok := payload["prompt"].(string)
	if _, present := payload["prompt"]; present && !ok {
		return bad("prompt 必须是字符串")
	}
	if strings.TrimSpace(prompt) == "" && !(operation == "video" && (payload["image_url"] != nil || payload["last_image_url"] != nil || mediaArrayLen(payload["reference_images"])+mediaArrayLen(payload["reference_videos"]) > 0)) {
		return bad("prompt 必须是非空字符串")
	}
	b := map[string]any{"model": model, "prompt": prompt}
	if operation != "video" {
		if payload["mask"] != nil {
			return bad("该上游图片编辑接口未提供 mask 语义，不能静默忽略")
		}
		size := payload["size"]
		if size == nil || size == "auto" {
			size = "1024x1024"
		}
		if _, ok := size.(string); !ok {
			return bad("size 必须是字符串")
		}
		b["size"] = size
		if n, ok := payload["n"]; ok {
			f, valid := n.(float64)
			if !valid || f < 1 || math.Trunc(f) != f {
				return bad("n 必须是正整数")
			}
			b["n"] = n
		} else if strings.HasPrefix(model, "hunyuan-") {
			b["n"] = 1
		}
		if format, ok := payload["response_format"]; ok && format != "url" && format != "b64_json" {
			return bad("response_format 仅支持 url 或 b64_json")
		}
		if strings.HasPrefix(model, "hunyuan-") && payload["response_format"] == "b64_json" {
			return bad("该模型返回图片 URL，不支持 b64_json")
		}
		fields := []string{"quality", "style", "background", "response_format"}
		if strings.HasPrefix(model, "hunyuan-") {
			fields = []string{"footnote", "revise"}
		} else if strings.HasPrefix(model, "gemini-") {
			fields = nil
			delete(b, "n")
		} else if payload["response_format"] == nil {
			b["response_format"] = "b64_json"
		}
		for _, k := range fields {
			if v, ok := payload[k]; ok {
				b[k] = v
			}
		}
		if operation == "image-edit" {
			images, ok := payload["image"].([]any)
			if !ok || len(images) == 0 {
				return bad("image 必须是非空图片 data URL 数组")
			}
			for _, v := range images {
				s, ok := v.(string)
				if !ok || !strings.HasPrefix(s, "data:image/") || !strings.Contains(s, ";base64,") {
					return bad("图片编辑仅接受 image 数组中的图片 data URL 或 multipart 文件")
				}
			}
			b["image"] = images
			if v, ok := payload["input_fidelity"]; ok {
				b["input_fidelity"] = v
			}
		}
		return b, nil
	}
	cfg, _ := entry["video_generation"].(map[string]any)
	seconds := payload["seconds"]
	if seconds == nil {
		seconds = cfg["defaultSeconds"]
	}
	if seconds == nil {
		seconds = float64(5)
	}
	if text, ok := seconds.(string); ok {
		value, err := strconv.Atoi(text)
		if err != nil {
			return bad("seconds 必须是正整数")
		}
		seconds = float64(value)
	}
	n, ok := seconds.(float64)
	if !ok || n <= 0 || math.Trunc(n) != n {
		return bad("seconds 必须是正整数")
	}
	if min, ok := cfg["minSeconds"].(float64); ok && n < min {
		return bad("视频时长小于该模型支持的最短时长")
	}
	if max, ok := cfg["maxSeconds"].(float64); ok && n > max {
		return bad("视频时长超过该模型支持的最长时长")
	}
	if max, ok := cfg["maxPromptLength"].(float64); ok && len([]rune(prompt)) > int(max) {
		return bad("prompt 超过该视频模型的长度要求")
	}
	b["seconds"], b["watermark"], b["negative_prompt"] = n, true, ""
	if v, ok := payload["negative_prompt"]; ok {
		if _, ok := v.(string); !ok {
			return bad("negative_prompt 必须是字符串")
		}
		b["negative_prompt"] = v
	}
	image := false
	for _, k := range []string{"image_url", "last_image_url"} {
		if v, ok := payload[k]; ok {
			s, valid := v.(string)
			if !valid || !(mediaInputURL(s) || strings.HasPrefix(s, "data:image/")) {
				return bad(k + " 必须是 HTTP(S) 图片地址或图片 data URL")
			}
			b[k] = s
			image = true
		}
	}
	if b["last_image_url"] != nil && b["image_url"] == nil && cfg["lastFrameRequiresFirstFrame"] == true {
		return bad("last_image_url 需要同时提供 image_url")
	}
	counts := map[string]int{}
	for _, pair := range [][3]string{{"reference_images", "image_url", "maxReferenceImages"}, {"reference_videos", "video_url", "maxReferenceVideos"}, {"reference_audios", "audio_url", "maxReferenceAudios"}} {
		value, present := payload[pair[0]]
		if !present {
			continue
		}
		items, ok := value.([]any)
		if !ok {
			return bad(pair[0] + " 必须是数组")
		}
		if max, known := cfg[pair[2]].(float64); known && len(items) > int(max) {
			return bad(pair[0] + " 超过该模型允许的数量")
		}
		refs := []any{}
		for _, v := range items {
			text, ok := v.(string)
			if !ok {
				if obj, yes := v.(map[string]any); yes {
					text, ok = obj[pair[1]].(string)
				}
			}
			if !ok || !(mediaInputURL(text) || pair[0] == "reference_images" && strings.HasPrefix(text, "data:image/")) {
				return bad(pair[0] + " 需要 HTTP(S) 地址（图片也支持 data URL）")
			}
			refs = append(refs, map[string]any{pair[1]: text})
		}
		counts[pair[0]] = len(refs)
		if len(refs) > 0 {
			b[pair[0]] = refs
		}
	}
	total, kinds := 0, 0
	for _, n := range counts {
		total += n
		if n > 0 {
			kinds++
		}
	}
	if max, known := cfg["maxReferenceMaterials"].(float64); known && total > int(max) {
		return bad("参考素材总数超过该模型允许的数量")
	}
	if kinds > 1 && cfg["allowMixedReferenceMaterials"] == false {
		return bad("该模型不支持混合参考素材")
	}
	if image && total > 0 && cfg["framesExclusiveWithReferenceMaterials"] == true {
		return bad("该模型的首尾帧不能与参考素材同时使用")
	}
	if counts["reference_audios"] > 0 && !image && counts["reference_images"]+counts["reference_videos"] == 0 && cfg["referenceAudioRequiresVisualMaterial"] == true {
		return bad("参考音频需要同时提供图片或视频")
	}
	image = image || counts["reference_images"]+counts["reference_videos"] > 0
	if allowed, known := cfg["supportedSeconds"].([]any); known && len(allowed) > 0 {
		found := false
		for _, v := range allowed {
			if v == n {
				found = true
			}
		}
		if !found {
			return bad("seconds 不在该模型支持的时长列表中")
		}
	}
	extra := map[string]any{}
	for _, pair := range [][2]string{{"resolution", "defaultResolution"}, {"aspect_ratio", "defaultAspectRatio"}, {"enable_audio", "defaultEnableAudio"}} {
		v, ok := payload[pair[0]]
		if !ok {
			v = cfg[pair[1]]
		}
		if v != nil {
			extra[pair[0]] = v
		}
	}
	for _, pair := range [][2]string{{"resolution", "resolutions"}, {"aspect_ratio", "aspectRatios"}} {
		if v, exists := extra[pair[0]]; exists {
			s, ok := v.(string)
			if !ok {
				return bad(pair[0] + " 必须是字符串")
			}
			allowed, _ := cfg[pair[1]].([]any)
			found := len(allowed) == 0
			for _, x := range allowed {
				if text, ok := x.(string); ok && strings.EqualFold(text, s) {
					extra[pair[0]] = text
					found = true
				}
			}
			if !found {
				return bad(pair[0] + " 不在该模型支持范围")
			}
		}
	}
	if v, ok := extra["enable_audio"]; ok {
		if _, ok := v.(bool); !ok {
			return bad("enable_audio 必须是布尔值")
		}
	}
	if image {
		if supported, known := cfg["supportsFirstFrame"].(bool); b["image_url"] != nil && known && !supported {
			return bad("该模型不支持首帧图片")
		}
		if b["last_image_url"] != nil {
			if supported, known := cfg["supportsFirstLastFrame"].(bool); known && !supported {
				return bad("该模型不支持首尾帧图片")
			}
		}
		delete(extra, "aspect_ratio")
	}
	if cfg["audioMode"] == "unsupported" {
		if extra["enable_audio"] == true {
			return bad("该模型不支持音频生成")
		}
		delete(extra, "enable_audio")
	}
	if cfg["audioMode"] == "always-on" {
		if extra["enable_audio"] == false {
			return bad("该模型始终生成音频，不支持关闭")
		}
		extra["enable_audio"] = true
	}
	b["extra_parameters"] = extra
	return b, nil
}

func (o *Runtime) CallMedia(ctx context.Context, req provider.MediaRequest) (map[string]any, error) {
	acc := o.Pool.Get(req.AccountID)
	if acc == nil || req.Operation != "video-query" && req.Operation != "transcription-query" && !acc.Enabled || o.authorizationInvalid(req.AccountID) {
		return nil, errBody(403, "原任务账号已停用或授权失效，请重新授权后查询", "account_unavailable")
	}
	if err := provider.CheckDispatch(ctx, provider.AccountRef{Provider: o.Name(), LocalID: req.AccountID}, req.Model); err != nil {
		return nil, err
	}
	mgr := o.Manager(req.AccountID)
	if mgr == nil {
		return nil, errBody(503, "账号凭据不可用", "auth_error")
	}
	h, err := mgr.GetChatHeadersContext(ctx)
	if err != nil {
		return nil, errBody(401, "账号授权不可用，请重新授权", "auth_error")
	}
	endpoint, err := siterouting.EndpointForProfile(acc.Profile)
	if err != nil {
		return nil, err
	}
	paths := map[string]string{"image": "/v2/images/generations", "image-edit": "/v2/images/edits", "video": "/v2/videos/generations", "video-query": "/v2/videos/tasks", "transcription": "/v2/async/tasks/create", "transcription-query": "/v2/async/tasks/query"}
	path, ok := paths[req.Operation]
	if !ok {
		return nil, provider.ErrUnsupported
	}
	if req.Operation == "transcription" {
		if data, ok := req.Payload["_audio_data"].(map[string]any); ok && data != nil {
			audioURL, err := o.uploadMediaInput(ctx, endpoint, h, data)
			if err != nil {
				return nil, err
			}
			req.Payload["audio_transcription"].(map[string]any)["audio_url"] = audioURL
		}
		delete(req.Payload, "_audio_data")
	}
	raw, err := json.Marshal(req.Payload)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	for k, v := range h {
		request.Header.Set(k, v)
	}
	request.Header.Set("Content-Type", "application/json")
	// Never replay a media submission: a lost response may already have created a paid task.
	if err = provider.CheckDispatch(ctx, provider.AccountRef{Provider: o.Name(), LocalID: req.AccountID}, req.Model); err != nil {
		return nil, err
	}
	client := o.MediaClient
	if client == nil {
		client = defaultMediaClient
	}
	resp, err := client.Do(request)
	if err != nil {
		return nil, errBody(502, "媒体请求未确认完成；不要自动重新提交，已有任务请继续查询", "media_outcome_unknown")
	}
	defer resp.Body.Close()
	limit := int64(128 << 20)
	if o.cfg != nil {
		limit = o.cfg.MaxResponseBytes
	}
	reader := io.Reader(resp.Body)
	if limit > 0 {
		reader = io.LimitReader(reader, limit+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil || limit > 0 && int64(len(data)) > limit {
		return nil, errBody(502, "媒体响应未完整读取；请勿重复提交", "media_outcome_unknown")
	}
	var envelope map[string]any
	if json.Unmarshal(data, &envelope) != nil {
		return nil, errBody(502, "上游返回无效媒体响应，请勿重复提交", "media_outcome_unknown")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &provider.APIError{Status: resp.StatusCode, Body: envelope}
	}
	if code, ok := envelope["code"]; ok && code != nil && code != float64(0) && code != "0" {
		return nil, &provider.APIError{Status: 400, Body: envelope}
	}
	result, ok := envelope["data"].(map[string]any)
	if !ok {
		return nil, errBody(502, "上游媒体响应缺少结果，请勿重复提交", "media_outcome_unknown")
	}
	if req.Operation != "video-query" && req.Operation != "transcription-query" {
		o.Pool.OnSuccess(acc.UID)
	}
	return result, nil
}

var _ provider.MediaProvider = (*Runtime)(nil)

func mediaArrayLen(v any) int { a, _ := v.([]any); return len(a) }

func (o *Runtime) uploadMediaInput(ctx context.Context, endpoint string, headers map[string]string, input map[string]any) (string, error) {
	encoded, _ := input["data"].(string)
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(data) == 0 {
		return "", errBody(400, "音频文件无效", "invalid_request_error")
	}
	mimeType, _ := input["mime"].(string)
	if !strings.HasPrefix(mimeType, "audio/") && !strings.HasPrefix(mimeType, "video/") && !strings.HasPrefix(mimeType, "image/") {
		return "", errBody(400, "需要音频或视频文件", "invalid_request_error")
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return "", err
	}
	extension := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "audio/wav": ".wav", "audio/x-wav": ".wav", "audio/wave": ".wav", "audio/mpeg": ".mp3", "audio/mp4": ".m4a", "video/mp4": ".mp4", "audio/flac": ".flac", "audio/ogg": ".ogg"}[mimeType]
	body, _ := json.Marshal(map[string]any{"object_keys": []string{"uploads/" + hex.EncodeToString(nonce[:]) + "-audio" + extension}})
	request, _ := http.NewRequestWithContext(ctx, "POST", endpoint+"/v2/as/support/presigned_url", bytes.NewReader(body))
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	client := o.MediaClient
	if client == nil {
		client = defaultMediaClient
	}
	response, err := client.Do(request)
	if err != nil {
		return "", errBody(502, "获取媒体上传地址失败，未提交转写任务", "media_upload_failed")
	}
	defer response.Body.Close()
	var envelope map[string]any
	if response.StatusCode != 200 || json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&envelope) != nil {
		return "", errBody(502, "媒体上传地址无效，未提交转写任务", "media_upload_failed")
	}
	if code, ok := envelope["code"].(float64); ok && code != 0 {
		return "", errBody(502, "上游未允许媒体上传", "media_upload_failed")
	}
	result, _ := envelope["data"].(map[string]any)
	items, _ := result["items"].([]any)
	if len(items) == 0 {
		return "", errBody(502, "未获取媒体上传地址", "media_upload_failed")
	}
	item, _ := items[0].(map[string]any)
	upload, _ := item["upload_url"].(string)
	download, _ := item["download_url"].(string)
	for _, target := range []string{upload, download} {
		u, e := url.Parse(target)
		if e != nil || u.Scheme != "https" || u.User != nil || !strings.HasSuffix(strings.ToLower(u.Hostname()), ".myqcloud.com") {
			return "", errBody(502, "上游媒体存储地址无效", "media_upload_failed")
		}
	}
	put, _ := http.NewRequestWithContext(ctx, "PUT", upload, bytes.NewReader(data))
	put.Header.Set("Content-Type", mimeType)
	// Signed COS URLs carry their own authorization; never send the account token.
	response, err = client.Do(put)
	if err != nil {
		return "", errBody(502, "媒体上传失败，未提交转写任务", "media_upload_failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", errBody(502, "媒体存储拒绝上传", "media_upload_failed")
	}
	return download, nil
}

func (o *Runtime) UploadMedia(ctx context.Context, req provider.MediaRequest, data map[string]any) (string, error) {
	if err := provider.CheckDispatch(ctx, provider.AccountRef{Provider: o.Name(), LocalID: req.AccountID}, req.Model); err != nil {
		return "", err
	}
	acc := o.Pool.Get(req.AccountID)
	mgr := o.Manager(req.AccountID)
	if acc == nil || mgr == nil || !acc.Enabled || o.authorizationInvalid(req.AccountID) {
		return "", errBody(403, "账号授权不可用", "account_unavailable")
	}
	headers, err := mgr.GetChatHeadersContext(ctx)
	if err != nil {
		return "", err
	}
	endpoint, err := siterouting.EndpointForProfile(acc.Profile)
	if err != nil {
		return "", err
	}
	return o.uploadMediaInput(ctx, endpoint, headers, data)
}

func mediaInputURL(text string) bool {
	u, err := url.Parse(text)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && u.User == nil
}
