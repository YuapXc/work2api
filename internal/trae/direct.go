package trae

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"

	"work2api/internal/core/protocol"
	"work2api/internal/core/provider"
	"work2api/internal/streamwatch"
	"work2api/internal/trae/upstream"
)

type directBody struct {
	io.Reader
	close func() error
}

func (b *directBody) Close() error { return b.close() }

func (r *Runtime) directAllowed(key string, config *upstream.DirectConfig) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.directRefusals[key] != sha256.Sum256([]byte(config.Key))
}

func (r *Runtime) rememberDirectRefusal(key string, config *upstream.DirectConfig, status int) {
	if status != 401 && status != 403 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.directRefusals == nil {
		r.directRefusals = map[string][32]byte{}
	}
	if len(r.directRefusals) >= 256 {
		for oldest := range r.directRefusals {
			delete(r.directRefusals, oldest)
			break
		}
	}
	r.directRefusals[key] = sha256.Sum256([]byte(config.Key))
}

func (r *Runtime) openDirect(ctx context.Context, config *upstream.DirectConfig, encoded []byte, model upstream.ModelInfo, limits map[string]int) (io.ReadCloser, int, error) {
	var source map[string]json.RawMessage
	if json.Unmarshal(encoded, &source) != nil {
		return nil, 0, errors.New("Trae 专用端点请求无法解析")
	}
	body := map[string]json.RawMessage{}
	for _, key := range []string{"messages", "tools", "tool_choice", "parallel_tool_calls", "temperature", "top_p", "stop", "presence_penalty", "frequency_penalty", "seed", "response_format"} {
		if value := source[key]; len(value) > 0 {
			body[key] = value
		}
	}
	body["model"], _ = json.Marshal(model.Enterprise.ModelName)
	body["stream"] = json.RawMessage("true")
	body["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	limit := model.MaxTokens
	asked := source["max_completion_tokens"]
	if len(asked) == 0 {
		asked = source["max_tokens"]
	}
	if len(asked) > 0 {
		var n float64
		if json.Unmarshal(asked, &n) != nil || n <= 0 || n > 1<<53 || math.Trunc(n) != n {
			return nil, 0, errors.New("Trae 专用端点输出限制无效")
		}
		if limit <= 0 || n < float64(limit) {
			limit = int64(n)
		}
	}
	if limit > 0 {
		body["max_tokens"], _ = json.Marshal(limit)
		limits["max_tokens"] = int(limit)
	}
	wire, err := json.Marshal(body)
	if err != nil {
		return nil, 0, errors.New("Trae 专用端点请求无法编码")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, config.URL, bytes.NewReader(wire))
	if err != nil {
		return nil, 0, errors.New("Trae 专用端点地址无效")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "hertz")
	req.Header.Set("X-Request-ID", "req_"+rand.Text())
	req.Header.Set("X-Model-Provider", "custom_openai_compatible")
	req.Header.Set("Authorization", "Bearer "+config.Key)
	resp, err := r.client(ctx).StreamHTTP.Do(req)
	if err != nil {
		return nil, 0, errors.New("Trae 专用端点连接失败")
	}
	status := resp.StatusCode
	if status < 200 || status >= 300 {
		resp.Body.Close()
		return nil, status, errors.New("Trae 专用端点拒绝请求")
	}
	watch := streamwatch.NewWatch(resp.Body, ctx, 0, 0)
	closeBody := func() error { watch.Close(); return resp.Body.Close() }
	reader := watch.Reader(resp.Body)
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "application/json") {
		raw, readErr := io.ReadAll(io.LimitReader(reader, streamwatch.DefaultMaxEventBytes+1))
		closeBody()
		var doc map[string]json.RawMessage
		if readErr != nil || len(raw) > streamwatch.DefaultMaxEventBytes || json.Unmarshal(raw, &doc) != nil || len(doc["error"]) > 0 || protocol.CompleteEmptyResponse(protocol.Chat, raw) {
			return nil, status, errors.New("Trae 专用端点未返回有效响应")
		}
		var choices []map[string]json.RawMessage
		if json.Unmarshal(doc["choices"], &choices) != nil || len(choices) == 0 {
			return nil, status, errors.New("Trae 专用端点未返回有效回答")
		}
		for _, choice := range choices {
			if len(choice["message"]) == 0 {
				return nil, status, errors.New("Trae 专用端点未返回有效回答")
			}
			choice["delta"] = choice["message"]
			delete(choice, "message")
		}
		doc["choices"], _ = json.Marshal(choices)
		raw, _ = json.Marshal(doc)
		return io.NopCloser(bytes.NewReader(append(append([]byte("data: "), raw...), []byte("\n\ndata: [DONE]\n\n")...))), status, nil
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		closeBody()
		return nil, status, errors.New("Trae 专用端点响应类型不支持")
	}
	buffered := bufio.NewReader(reader)
	prefix, err := protocol.ReadStreamHead(buffered, protocol.Chat)
	if err != nil {
		closeBody()
		var refusal *protocol.StreamHeadError
		if errors.As(err, &refusal) {
			status = refusal.Status
		}
		return nil, status, errors.New("Trae 专用端点在输出前返回异常")
	}
	probe := protocol.NewEmptyReplyProbe(protocol.Chat)
	probe.Observe(prefix)
	if probe.Done && probe.Empty {
		closeBody()
		return nil, status, errors.New("Trae 专用端点返回空回答")
	}
	return &directBody{Reader: io.MultiReader(bytes.NewReader(prefix), buffered), close: closeBody}, status, nil
}

func deliverDirect(ctx context.Context, req provider.ServeRequest, stream io.ReadCloser, model string) (protocol.Usage, bool, error) {
	defer stream.Close()
	stop := context.AfterFunc(ctx, func() { stream.Close() })
	defer stop()
	if streaming, _ := req.Payload["stream"].(bool); streaming {
		req.Writer.Header().Set("Content-Type", "text/event-stream")
		req.Writer.Header().Set("Cache-Control", "no-cache")
		return protocol.TranscodeStreamWithRewrite(ctx, req.Writer, stream, protocol.Chat, protocol.Protocol(req.Protocol), model, directChunk)
	}
	reply, usage, known, err := protocol.CollapseChatStreamAs(ctx, stream, protocol.Protocol(req.Protocol), model, directChunk)
	if err == nil {
		req.Writer.Header().Set("Content-Type", "application/json")
		_, err = req.Writer.Write(reply)
	}
	return usage, known, err
}

func directChunk(chunk map[string]any) error {
	if chunk != nil && chunk["error"] != nil {
		chunk["error"] = map[string]any{"message": "Trae 专用端点返回错误", "type": "upstream_error"}
	}
	return nil
}
