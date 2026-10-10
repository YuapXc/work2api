package app

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
)

func isModelRoute(r *http.Request) bool {
	return r.Method == "POST" && (r.URL.Path == "/v1/chat/completions" || r.URL.Path == "/v1/messages" || r.URL.Path == "/v1/responses")
}

func isRefresh(r *http.Request) bool {
	return r.Method == "POST" && (strings.HasSuffix(r.URL.Path, "/refresh") || r.URL.Path == "/admin/models")
}

func isHeavyAdmin(r *http.Request) bool {
	return strings.HasPrefix(r.URL.Path, "/admin/") && (isRefresh(r) || strings.HasSuffix(r.URL.Path, "/checkin") || strings.HasSuffix(r.URL.Path, "/upload") || strings.HasSuffix(r.URL.Path, "/export") || strings.Contains(r.URL.Path, "/oauth/") || strings.HasSuffix(r.URL.Path, "/config") && r.Method == "POST")
}

func (s *Server) writeOverload(w http.ResponseWriter, code string) {
	s.modelsAdmission.mu.Lock()
	s.modelsAdmission.rejected[code]++
	s.modelsAdmission.mu.Unlock()
	w.Header().Set("Retry-After", "2")
	writeAPIErr(w, localOverload(code))
}

type bodyBudget struct {
	mu          sync.Mutex
	limit, used int64
}

func (b *bodyBudget) reserve(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.limit-b.used {
		return false
	}
	b.used += n
	return true
}
func (b *bodyBudget) release(n int64) { b.mu.Lock(); b.used -= n; b.mu.Unlock() }
func (b *bodyBudget) usage() int64    { b.mu.Lock(); defer b.mu.Unlock(); return b.used }

// Charges allocated capacity, including both buffers during growth. The tiny
// reader scratch space is separately bounded by BODY_READ_CONCURRENCY.
func (b *bodyBudget) read(r io.Reader, length int64) ([]byte, int64, error) {
	var body []byte
	var reserved int64
	defer func() {
		if v := recover(); v != nil {
			b.release(reserved)
			panic(v)
		}
	}()
	chunk := make([]byte, 32<<10)
	for {
		n, err := r.Read(chunk)
		if n > 0 {
			if len(body)+n > cap(body) {
				size := max(cap(body)*2, len(body)+n)
				if length > 0 && int64(size) < length {
					size = int(length)
				}
				if !b.reserve(int64(size)) {
					return nil, reserved, localOverload("body_budget_exhausted")
				}
				next := make([]byte, len(body), size)
				copy(next, body)
				b.release(reserved)
				reserved = int64(size)
				body = next
			}
			body = append(body, chunk[:n]...)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return body, reserved, nil
			}
			return nil, reserved, err
		}
	}
}

type refreshFlight struct {
	done    chan struct{}
	waiters int
	header  http.Header
	code    int
	body    []byte
}
type refreshRecorder struct {
	header   http.Header
	code     int
	body     bytes.Buffer
	overflow bool
}

func (r *refreshRecorder) Header() http.Header { return r.header }
func (r *refreshRecorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}
func (r *refreshRecorder) Write(p []byte) (int, error) {
	if r.body.Len()+len(p) > 4<<20 {
		r.overflow = true
		return 0, errors.New("refresh response too large")
	}
	r.WriteHeader(200)
	return r.body.Write(p)
}

func (s *Server) serveRefresh(w http.ResponseWriter, r *http.Request, next http.Handler) {
	key := r.URL.Path
	if key == "/admin/models" {
		key = "/admin/models/refresh"
	}
	if key == "/admin/models/refresh" {
		if selected := r.URL.Query().Get("provider"); selected != "" {
			key += "?provider=" + selected
		}
	}
	s.refreshMu.Lock()
	if f := s.refreshes[key]; f != nil {
		f.waiters++
		s.refreshMu.Unlock()
		select {
		case <-r.Context().Done():
			return
		case <-f.done:
		}
		for k, v := range f.header {
			w.Header()[k] = v
		}
		w.WriteHeader(f.code)
		_, _ = w.Write(f.body)
		return
	}
	select {
	case s.heavySlots <- struct{}{}:
	default:
		s.refreshMu.Unlock()
		s.writeOverload(w, "heavy_admin_capacity")
		return
	}
	f := &refreshFlight{done: make(chan struct{})}
	s.refreshes[key] = f
	s.refreshMu.Unlock()
	rec := &refreshRecorder{header: make(http.Header)}
	defer func() {
		// Always wake followers, including when net/http recovers a handler panic.
		if f.code == 0 {
			f.code = 500
			f.body = []byte(`{"error":{"message":"刷新失败","type":"server_error"}}`)
		}
		s.refreshMu.Lock()
		delete(s.refreshes, key)
		close(f.done)
		s.refreshMu.Unlock()
		<-s.heavySlots
	}()
	next.ServeHTTP(rec, r)
	if rec.overflow {
		rec.code = 502
		rec.body.Reset()
		rec.body.WriteString(`{"error":{"message":"刷新响应超过限制","type":"response_too_large"}}`)
	}
	f.header = rec.header.Clone()
	f.code = rec.code
	if f.code == 0 {
		f.code = 200
	}
	f.body = rec.body.Bytes()
	for k, v := range f.header {
		w.Header()[k] = v
	}
	w.WriteHeader(f.code)
	_, _ = w.Write(f.body)
}

// A linear scan bounds JSON allocation amplification before Decode builds maps
// and slices. JSON syntax remains the decoder's job. Multipart is not scanned.
func jsonWithinComplexity(body []byte, itemsLimit, depthLimit int) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' && trimmed[0] != '[' {
		return true
	}
	inString, escaped := false, false
	depth, items := 0, 0
	for _, c := range trimmed {
		if inString {
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
			items++
		case '}', ']':
			depth--
		case ',', ':':
			items++
		}
		if depth > depthLimit || items > itemsLimit {
			return false
		}
	}
	return true
}
