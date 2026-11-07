package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
	"unicode"

	"work2api/internal/store"
)

const (
	usageExportDir     = ".usage-exports"
	usageExportBudget  = 128 << 20
	usageExportTimeout = 2 * time.Minute
	usageExportTTL     = 15 * time.Minute
)

var errExportBudget = errors.New("CSV 超过 128 MiB，请缩小筛选范围后重试")

type usageExportInput struct {
	Protocol string  `json:"protocol"`
	Model    string  `json:"model"`
	AppName  *string `json:"app_name"`
	Status   string  `json:"status"`
	Search   string  `json:"search"`
	Timezone string  `json:"timezone"`
}

type usageExport struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Count     int    `json:"count"`
	Error     string `json:"error,omitempty"`
	ExpiresAt int64  `json:"expires_at,omitempty"`

	owner    [32]byte
	path     string
	filename string
	cancel   context.CancelFunc
	done     chan struct{}
	timer    *time.Timer
	download bool
}

type usageExportQueue struct {
	mu     sync.Mutex
	job    *usageExport
	closed bool
}

func exportOwner(r *http.Request) [32]byte {
	method := identityFromRequest(r).Actor.Method
	if method == "password" || method == "recovery_session" {
		if cookie, err := r.Cookie(cookieName); err == nil {
			return sha256.Sum256([]byte("session:" + cookie.Value))
		}
	}
	return sha256.Sum256([]byte(method))
}

func (s *Server) mountUsageExport(mux *http.ServeMux) {
	mux.HandleFunc("POST /admin/usage/exports", s.startUsageExport)
	mux.HandleFunc("GET /admin/usage/exports/{id}", s.usageExportStatus)
	mux.HandleFunc("DELETE /admin/usage/exports/{id}", s.cancelUsageExport)
	mux.HandleFunc("GET /admin/usage/exports/{id}/download", s.downloadUsageExport)
}

func (s *Server) startUsageExport(w http.ResponseWriter, r *http.Request) {
	var input usageExportInput
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil || json.Unmarshal(raw, &input) != nil {
		writeAPIErr(w, errBody(400, "无效的导出筛选条件", "invalid_request_error"))
		return
	}
	location := time.UTC
	if input.Timezone != "" && input.Timezone != "Local" {
		location, err = time.LoadLocation(input.Timezone)
		if err != nil {
			writeAPIErr(w, errBody(400, "无法识别导出时区", "invalid_request_error"))
			return
		}
	}
	queue := &s.usageExports
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.closed || queue.job != nil && (queue.job.Status == "running" || queue.job.Status == "cancelling" || queue.job.download) {
		writeAPIErr(w, errBody(409, "已有导出任务正在生成或下载，请稍后重试", "export_busy"))
		return
	}
	dir := filepath.Join(s.o.cfg.DataDir, usageExportDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeAPIErr(w, errBody(500, "无法创建导出临时目录", "server_error"))
		return
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		writeAPIErr(w, errBody(500, "导出临时目录不可用", "server_error"))
		return
	}
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		writeAPIErr(w, errBody(500, "无法创建导出任务", "server_error"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), usageExportTimeout)
	job := &usageExport{ID: hex.EncodeToString(id), Status: "running", owner: exportOwner(r), cancel: cancel, done: make(chan struct{})}
	job.path = filepath.Join(dir, "usage-"+job.ID+".csv")
	job.filename = "usage-" + time.Now().In(location).Format("20060102-150405") + ".csv"
	queue.discard()
	queue.job = job
	go s.generateUsageExport(ctx, job, input, location)
	writeJSON(w, http.StatusAccepted, job)
}

func (q *usageExportQueue) lookup(r *http.Request) *usageExport {
	job := q.job
	if job == nil || job.ID != r.PathValue("id") || job.owner != exportOwner(r) ||
		job.Expired() {
		return nil
	}
	return job
}

func (job *usageExport) Expired() bool {
	return job.ExpiresAt != 0 && time.Now().UnixMilli() >= job.ExpiresAt
}

func (q *usageExportQueue) discard() {
	if q.job == nil {
		return
	}
	if q.job.timer != nil {
		q.job.timer.Stop()
	}
	removeUsageExport(q.job.path)
	q.job = nil
}

func removeUsageExport(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("清理 CSV 导出临时文件失败: %v", err)
	}
}

func (s *Server) usageExportStatus(w http.ResponseWriter, r *http.Request) {
	q := &s.usageExports
	q.mu.Lock()
	defer q.mu.Unlock()
	if job := q.lookup(r); job != nil {
		writeJSON(w, http.StatusOK, job)
		return
	}
	writeAPIErr(w, errBody(404, "导出任务不存在或已过期", "export_not_found"))
}

func (s *Server) cancelUsageExport(w http.ResponseWriter, r *http.Request) {
	q := &s.usageExports
	q.mu.Lock()
	defer q.mu.Unlock()
	job := q.lookup(r)
	if job == nil {
		writeAPIErr(w, errBody(404, "导出任务不存在或已过期", "export_not_found"))
		return
	}
	if job.Status == "running" {
		job.Status = "cancelling"
		job.cancel()
	} else if job.Status == "ready" && !job.download {
		job.Status = "cancelled"
		removeUsageExport(job.path)
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Server) generateUsageExport(ctx context.Context, job *usageExport, input usageExportInput, location *time.Location) {
	defer close(job.done)
	defer job.cancel()
	count, err := s.writeUsageExport(ctx, job.path, input, location)
	q := &s.usageExports
	q.mu.Lock()
	defer q.mu.Unlock()
	switch {
	case ctx.Err() == context.Canceled:
		job.Status = "cancelled"
	case ctx.Err() == context.DeadlineExceeded:
		job.Status, job.Error = "failed", "导出超时，请缩小筛选范围后重试"
	case errors.Is(err, errExportBudget):
		job.Status, job.Error = "failed", errExportBudget.Error()
	case err != nil:
		log.Printf("生成 CSV 导出失败: %v", err)
		job.Status, job.Error = "failed", "无法生成 CSV，请检查服务器磁盘空间或缩小筛选范围后重试"
	default:
		job.Status, job.Count = "ready", count
	}
	if job.Status != "ready" {
		removeUsageExport(job.path)
	}
	job.ExpiresAt = time.Now().Add(usageExportTTL).UnixMilli()
	job.timer = time.AfterFunc(usageExportTTL, func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		if q.job == job && !job.download {
			q.discard()
		}
	})
}

func (s *Server) downloadUsageExport(w http.ResponseWriter, r *http.Request) {
	q := &s.usageExports
	q.mu.Lock()
	job := q.lookup(r)
	if job == nil || job.Status != "ready" {
		q.mu.Unlock()
		writeAPIErr(w, errBody(404, "CSV 未就绪或已过期", "export_not_found"))
		return
	}
	if job.download {
		q.mu.Unlock()
		writeAPIErr(w, errBody(409, "该文件已在下载", "export_busy"))
		return
	}
	file, err := os.Open(job.path)
	if err != nil {
		q.mu.Unlock()
		writeAPIErr(w, errBody(404, "导出文件暂不可用，请重新导出", "export_not_found"))
		return
	}
	job.download = true
	q.mu.Unlock()
	defer func() {
		file.Close()
		q.mu.Lock()
		defer q.mu.Unlock()
		job.download = false
		if q.closed || job.Expired() {
			q.discard()
		}
	}()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(5 * time.Minute))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+job.filename+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "usage.csv", time.Time{}, file)
}

func (s *Server) CloseUsageExports() {
	q := &s.usageExports
	q.mu.Lock()
	q.closed = true
	job := q.job
	if job != nil {
		job.cancel()
	}
	q.mu.Unlock()
	if job != nil {
		<-job.done
		q.mu.Lock()
		defer q.mu.Unlock()
		if q.job == job && !job.download {
			q.discard()
		}
	}
}

func clearOldUsageExports(dataDir string) {
	if dataDir == "" {
		return
	}
	dir := filepath.Join(dataDir, usageExportDir)
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		id, suffixOK := strings.CutSuffix(strings.TrimPrefix(entry.Name(), "usage-"), ".csv")
		if !suffixOK || !strings.HasPrefix(entry.Name(), "usage-") || len(id) != 32 || !entry.Type().IsRegular() {
			continue
		}
		if _, err := hex.DecodeString(id); err == nil {
			removeUsageExport(filepath.Join(dir, entry.Name()))
		}
	}
}

type exportWriter struct {
	io.Writer
	remaining int
}

func (w *exportWriter) Write(b []byte) (int, error) {
	if len(b) > w.remaining {
		return 0, errExportBudget
	}
	n, err := w.Writer.Write(b)
	w.remaining -= n
	return n, err
}

func (s *Server) writeUsageExport(ctx context.Context, path string, input usageExportInput, location *time.Location) (int, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	out := &exportWriter{Writer: file, remaining: usageExportBudget}
	if _, err = io.WriteString(out, "\ufeff"); err != nil {
		return 0, err
	}
	writer := csv.NewWriter(out)
	if err = writer.Write(usageCSVHeader); err != nil {
		return 0, err
	}
	count := 0
	err = s.o.db.VisitUsageExport(ctx, s.o.cfg.DBPath, input.Protocol, input.Model, input.AppName, input.Status, input.Search, func(row map[string]any) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		count++
		return writer.Write(usageCSVRow(row, location))
	})
	if err != nil {
		return 0, err
	}
	writer.Flush()
	if err = writer.Error(); err != nil {
		return 0, err
	}
	return count, file.Close()
}

var usageCSVHeader = []string{"时间", "请求标识", "调用模型", "实际模型", "协议", "Key", "账号", "输入Tokens", "输出Tokens", "缓存读取Tokens", "最终命中消耗", "消耗渠道", "总耗时ms", "排队ms", "账号等待ms", "首响应ms", "首生成ms", "生成阶段耗时ms", "上游尝试次数", "客户端HTTP状态", "结果", "结束原因"}

func usageCSVRow(row map[string]any, location *time.Location) []string {
	d, _ := row["diagnostics"].(store.UsageDiagnostics)
	model := str2(row["model"])
	requested, physical := strOr(d.RequestedModel, model), strOr(d.PhysicalModel, model)
	channel, _, found := strings.Cut(physical, "/")
	if !found {
		channel = "workbuddy"
	}
	input, output, total := row["input_tokens"], row["output_tokens"], row["total_tokens"]
	i, iOK := input.(int64)
	o, oOK := output.(int64)
	t, tOK := total.(int64)
	legacy := row["tokens_known"] == nil && iOK && oOK && tOK && i >= 0 && o >= 0 && t > 0 && t <= (1<<53)-1 && t-i == o
	if !legacy && row["tokens_known"] != int64(1) && row["tokens_known"] != true {
		input, output = nil, nil
	}
	date := "—"
	if ts, ok := row["ts"].(float64); ok && ts != 0 {
		date = time.Unix(int64(ts), 0).In(location).Format("2006-01-02 15:04:05")
	}
	var requestID, queue, accountWait, firstByte, firstGen, generation, attempts, httpStatus any
	duration := row["latency_ms"]
	if p := d.Performance; p != nil {
		requestID, queue, accountWait, duration = p.RequestID, p.QueueMS, p.AccountWaitMS, p.TotalMS
		firstByte, firstGen, generation, attempts = p.FirstByteMS, p.FirstGenerationMS, p.GenerationMS, p.Attempts
		if p.HTTPStatus != 0 {
			httpStatus = p.HTTPStatus
		}
	}
	values := []any{date, requestID, requested, physical, row["protocol"], row["app_name"], row["account_uid"], input, output, row["cached_tokens"], row["credits"], channel, duration, queue, accountWait, firstByte, firstGen, generation, attempts, httpStatus, row["status"], d.FinishReason}
	result := make([]string, len(values))
	for i, value := range values {
		result[i] = usageCSVCell(value)
	}
	return result
}

func usageCSVCell(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case *int64:
		if v == nil {
			return ""
		}
		return fmt.Sprint(*v)
	case string:
		trimmed := strings.TrimLeftFunc(v, func(r rune) bool { return unicode.IsSpace(r) || r == '\ufeff' })
		if strings.HasPrefix(v, "\t") || strings.HasPrefix(v, "\r") || strings.HasPrefix(v, "\n") ||
			len(trimmed) > 0 && strings.ContainsRune("=+-@", rune(trimmed[0])) {
			return "'" + v
		}
		return v
	default:
		return fmt.Sprint(value)
	}
}
