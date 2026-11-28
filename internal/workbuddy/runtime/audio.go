// 参照 magpie-community/plugins 的 WorkBuddy 语音协议（MIT）；来源与许可见 docs/providers.md。
package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"unicode"
	"unicode/utf8"

	"work2api/internal/core/provider"
	"work2api/internal/streamwatch"
	"work2api/internal/workbuddy/credentials"
)

const speechModel = "workbuddy-tts"
const transcribeModel = "workbuddy-transcribe"

func audioBody(operation, model string, payload map[string]any) (map[string]any, error) {
	bad := func(message string) (map[string]any, error) {
		return nil, errBody(400, message, "invalid_request_error")
	}
	format, _ := payload["response_format"].(string)
	if value, exists := payload["response_format"]; exists {
		if _, ok := value.(string); !ok {
			return bad("response_format 必须是字符串")
		}
	}
	if operation == "speech" && model == speechModel {
		text, ok := payload["input"].(string)
		if !ok || strings.TrimFunc(text, spokenSpace) == "" || !utf8.ValidString(text) {
			return bad("input 必须是非空UTF-8文本")
		}
		if payload["stream_format"] != nil && payload["stream_format"] != "audio" {
			return bad("语音仅支持音频响应，不支持SSE输出")
		}
		if format == "" {
			format = "wav"
		}
		if format != "wav" && format != "pcm" {
			return bad("语音 response_format 仅支持 wav 或 pcm")
		}
		return map[string]any{"input": text, "response_format": format}, nil
	}
	if operation == "transcription" && model == transcribeModel {
		if format == "" {
			format = "json"
		}
		if format != "json" && format != "text" {
			return bad("转写 response_format 仅支持 json 或 text")
		}
		data, ok := payload["audio_data"].(map[string]any)
		if !ok {
			return bad("此转写接口需要上传file音频文件")
		}
		if text, _ := data["data"].(string); text == "" && len(asRawAudio(data)) == 0 {
			return bad("音频文件为空")
		}
		return map[string]any{"audio_data": data, "response_format": format}, nil
	}
	return bad("模型与语音操作不匹配")
}
func spokenSpace(r rune) bool { return r == '\ufeff' || (unicode.IsSpace(r) && r != '\u0085') }

func spokenParts(text string) []string {
	text = strings.TrimFunc(text, spokenSpace)
	var parts []string
	for len(text) > 1000 {
		end := 1000
		for !utf8.RuneStart(text[end]) {
			end--
		}
		head := text[:end]
		cut := 0
		for _, marks := range []string{"。！？!?\n", "，,；;：:", " \t"} {
			for at, r := range head {
				if strings.ContainsRune(marks, r) || marks == " \t" && spokenSpace(r) {
					cut = at + utf8.RuneLen(r)
				}
			}
			if cut > 0 {
				break
			}
		}
		if cut == 0 {
			cut = end
		}
		if part := strings.TrimFunc(text[:cut], spokenSpace); part != "" {
			parts = append(parts, part)
		}
		text = strings.TrimFunc(text[cut:], spokenSpace)
	}
	if text != "" {
		parts = append(parts, text)
	}
	return parts
}
func (o *Runtime) audioRequest(ctx context.Context, req provider.MediaRequest, mgr *credentials.Manager, path, kind string, body []byte) (*http.Response, error) {
	current := func() bool {
		return mgr != nil && o.Manager(req.AccountID) == mgr && o.Pool.EnabledForCredential(req.AccountID, mgr) && !mgr.AuthorizationInvalid()
	}
	client := o.MediaClient
	if client == nil {
		client = defaultMediaClient
	}
	for attempt := 0; attempt < 2; attempt++ {
		if !current() {
			return nil, errBody(403, "账号已停用或授权已变更", "account_unavailable")
		}
		headers, err := mgr.GetHeadersContext(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errBody(401, "账号授权不可用，请重新授权", "auth_error")
		}
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, mgr.Endpoint()+path, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		for _, k := range []string{"Authorization", "X-API-Key", "X-User-Id", "X-Enterprise-Id", "X-Tenant-Id", "X-Domain"} {
			if v := headers[k]; v != "" {
				r.Header.Set(k, v)
			}
		}
		r.Header.Set("Content-Type", kind)
		r.Header.Set("User-Agent", "node")
		r.Header.Set("Accept", "*/*")
		r.Header.Set("Accept-Language", "*")
		r.Header.Set("Sec-Fetch-Mode", "cors")
		if err := provider.CheckDispatch(ctx, provider.AccountRef{Provider: o.Name(), LocalID: req.AccountID}, req.Model); err != nil {
			return nil, err
		}
		if !current() {
			return nil, errBody(403, "账号已停用或授权已变更", "account_unavailable")
		}
		resp, err := client.Do(r)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errBody(502, "语音请求未确认完成，请勿自动重复提交", "media_outcome_unknown")
		}
		if resp.Request == nil {
			resp.Request = r
		}
		if resp.StatusCode == 401 && strings.Contains(strings.ToLower(resp.Header.Get("WWW-Authenticate")), "invalid_token") && attempt == 0 {
			resp.Body.Close()
			if !current() {
				return nil, errBody(403, "账号已停用或授权已变更", "account_unavailable")
			}
			if err := mgr.RecoverChatAuthorization(ctx, headers["Authorization"]); err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, errBody(401, "语音接口授权失效，请重新授权", "auth_error")
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			raw, _ := streamwatch.ReadBody(ctx, resp.Body, 1<<20)
			resp.Body.Close()
			return nil, speechFailure(resp.StatusCode, raw, "上游语音接口拒绝请求", r.Header)
		}
		return resp, nil
	}
	return nil, errBody(401, "语音接口未通过授权", "auth_error")
}
func speechOutputSize(n int64, format string) int64 {
	if format == "pcm" {
		return (n/2*3 + 1) / 2 * 2
	}
	return n + 44
}

func readSpoken(ctx context.Context, body io.ReadCloser, pcm *bytes.Buffer, format string, limit int64, headers http.Header) error {
	watch := streamwatch.NewWatch(body, streamwatch.WithResponseLimit(ctx, 0), 0, 0)
	defer watch.Close()
	scan := bufio.NewScanner(watch.Reader(body))
	scan.Buffer(make([]byte, 4096), streamwatch.DefaultMaxLineBytes)
	start := pcm.Len()
	var budget streamwatch.SSEEventBudget
	kind := ""
	var event strings.Builder
	flush := func() error {
		if kind == "error" {
			return speechFailure(502, []byte(event.String()), "上游未能完成语音生成", headers)
		}
		if kind == "audio" && event.Len() > 0 {
			data, err := base64.StdEncoding.DecodeString(event.String())
			if err != nil {
				return errBody(502, "上游音频编码无效", "upstream_error")
			}
			if limit > 0 && speechOutputSize(int64(pcm.Len())+int64(len(data)), format) > limit {
				return streamwatch.ErrResponseTooLarge
			}
			pcm.Write(data)
		}
		kind = ""
		event.Reset()
		return nil
	}
	for scan.Scan() {
		line := strings.TrimSuffix(scan.Text(), "\r")
		if err := budget.AddLine(line); err != nil {
			return err
		}
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "event:") {
			kind = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			if event.Len() > 0 {
				event.WriteByte('\n')
			}
			event.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := watch.Err(); err != nil {
		return err
	}
	if err := scan.Err(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	if pcm.Len() == start || (pcm.Len()-start)%2 != 0 {
		return errBody(502, "上游未返回完整PCM音频", "upstream_error")
	}
	return nil
}
func speechWAV(pcm []byte) []byte {
	out := make([]byte, 44+len(pcm))
	copy(out, "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(36+len(pcm)))
	copy(out[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(out[16:], 16)
	binary.LittleEndian.PutUint16(out[20:], 1)
	binary.LittleEndian.PutUint16(out[22:], 1)
	binary.LittleEndian.PutUint32(out[24:], 16000)
	binary.LittleEndian.PutUint32(out[28:], 32000)
	binary.LittleEndian.PutUint16(out[32:], 2)
	binary.LittleEndian.PutUint16(out[34:], 16)
	copy(out[36:], "data")
	binary.LittleEndian.PutUint32(out[40:], uint32(len(pcm)))
	copy(out[44:], pcm)
	return out
}
func speechPCM24(pcm []byte) []byte {
	n := len(pcm) / 2
	m := (n*3 + 1) / 2
	out := make([]byte, m*2)
	for i := 0; i < m; i++ {
		j := i * 2 / 3
		frac := float64(i*2%3) / 3
		a := int16(binary.LittleEndian.Uint16(pcm[min(j, n-1)*2:]))
		b := int16(binary.LittleEndian.Uint16(pcm[min(j+1, n-1)*2:]))
		v := float64(a) + (float64(b)-float64(a))*frac
		v = math.Floor(v + .5)
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(v)))
	}
	return out
}
func (o *Runtime) CallAudio(ctx context.Context, req provider.MediaRequest) (provider.AudioResponse, error) {
	mgr := o.Manager(req.AccountID)
	limit := int64(128 << 20)
	if o.cfg != nil {
		limit = o.cfg.MaxResponseBytes
	}
	if req.Model == speechModel && req.Operation == "speech" {
		text, _ := req.Payload["input"].(string)
		format, _ := req.Payload["response_format"].(string)
		var pcm bytes.Buffer
		for _, part := range spokenParts(text) {
			if limit > 0 && speechOutputSize(int64(pcm.Len())+2, format) > limit {
				return provider.AudioResponse{}, streamwatch.ErrResponseTooLarge
			}
			wire, _ := json.Marshal(map[string]string{"text": part})
			resp, err := o.audioRequest(ctx, req, mgr, "/agenttool/v1/tts", "application/json", wire)
			if err != nil {
				return provider.AudioResponse{}, err
			}
			err = readSpoken(ctx, resp.Body, &pcm, format, limit, resp.Request.Header)
			resp.Body.Close()
			if err != nil {
				return provider.AudioResponse{}, err
			}
		}
		if pcm.Len() == 0 {
			return provider.AudioResponse{}, errBody(502, "上游未返回音频", "upstream_error")
		}
		if req.Payload["response_format"] == "pcm" {
			return provider.AudioResponse{Data: speechPCM24(pcm.Bytes()), ContentType: "audio/pcm"}, nil
		}
		return provider.AudioResponse{Data: speechWAV(pcm.Bytes()), ContentType: "audio/wav"}, nil
	}
	if req.Model != transcribeModel || req.Operation != "transcription" {
		return provider.AudioResponse{}, provider.ErrUnsupported
	}
	file, _ := req.Payload["audio_data"].(map[string]any)
	encoded, _ := file["data"].(string)
	data := asRawAudio(file)
	var err error
	if data == nil {
		data, err = base64.StdEncoding.DecodeString(encoded)
	}
	if err != nil || len(data) == 0 {
		return provider.AudioResponse{}, errBody(400, "音频文件无效", "invalid_request_error")
	}
	kind, _ := file["mime"].(string)
	if original, ok := file["original_mime"].(string); ok {
		kind = original
	}
	if kind == "" {
		kind = "application/octet-stream"
	}
	if _, _, err := mime.ParseMediaType(kind); err != nil {
		return provider.AudioResponse{}, errBody(400, "文件MIME类型无效", "invalid_request_error")
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	header := textproto.MIMEHeader{}
	name, _ := file["name"].(string)
	if name == "" {
		name = "audio.wav"
	}
	header.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "audio", "filename": name}))
	header.Set("Content-Type", kind)
	part, err := form.CreatePart(header)
	if err != nil {
		return provider.AudioResponse{}, err
	}
	_, _ = part.Write(data)
	_ = form.Close()
	resp, err := o.audioRequest(ctx, req, mgr, "/agenttool/v1/asr", form.FormDataContentType(), body.Bytes())
	if err != nil {
		return provider.AudioResponse{}, err
	}
	defer resp.Body.Close()
	raw, err := streamwatch.ReadBody(ctx, resp.Body, limit)
	if err != nil {
		return provider.AudioResponse{}, err
	}
	var result map[string]any
	if json.Unmarshal(raw, &result) != nil || result == nil {
		return provider.AudioResponse{}, errBody(502, "上游语音识别返回无效JSON", "upstream_error")
	}
	if code := result["code"]; code != nil && code != float64(0) && code != "" && code != false {
		return provider.AudioResponse{}, speechFailure(502, raw, "上游语音识别失败", resp.Request.Header)
	}
	text, _ := result["text"].(string)
	if req.Payload["response_format"] == "text" {
		return provider.AudioResponse{Data: []byte(text), ContentType: "text/plain; charset=utf-8"}, nil
	}
	output, _ := json.Marshal(map[string]string{"text": text})
	return provider.AudioResponse{Data: output, ContentType: "application/json"}, nil
}

func asRawAudio(file map[string]any) []byte { data, _ := file["raw"].([]byte); return data }

func speechFailure(status int, raw []byte, fallback string, headers http.Header) error {
	message := strings.TrimSpace(string(raw))
	if strings.HasPrefix(message, "{") || strings.HasPrefix(message, "[") {
		var result map[string]any
		_ = json.Unmarshal(raw, &result)
		message = ""
		inner, _ := result["error"].(map[string]any)
		for _, v := range []any{result["msg"], result["message"], inner["message"]} {
			if text, ok := v.(string); ok && text != "" {
				message = text
				break
			}
		}
		switch code := result["code"].(type) {
		case float64:
			if code != 0 {
				message += fmt.Sprintf("（code %v）", code)
			}
		case string:
			if code != "" {
				message += "（code " + code + "）"
			}
		}
	}
	for _, secret := range []string{strings.TrimPrefix(headers.Get("Authorization"), "Bearer "), headers.Get("X-API-Key")} {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[redacted]")
		}
	}
	n := 0
	for i := range message {
		if n == 300 {
			message = message[:i]
			break
		}
		n++
	}
	if message != "" {
		fallback += "：" + message
	}
	if status < 500 {
		return errBody(status, fallback, "invalid_request_error")
	}
	return errBody(status, fallback, "api_error")
}
