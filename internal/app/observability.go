package app

import (
	"crypto/rand"
	"encoding/hex"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"
	"work2api/internal/streamwatch"
)

// One hour of bounded aggregate histograms, independent of user/model cardinality.
type callBucket struct {
	minute                                           int64
	Count, Errors, Incomplete, Attempts, Upstream429 int64
	Wait, First, Total                               [19]int64
	Queue, Account, Execution                        [19]int64
	FirstCount                                       int64
	ExecutionCount                                   int64
}
type callObservation struct {
	RequestID   string `json:"request_id"`
	Status      int    `json:"http_status"`
	Outcome     string `json:"outcome"`
	Attempts    int    `json:"attempts"`
	Upstream429 int    `json:"upstream_429"`
	Queue       int64  `json:"queue_ms"`
	Account     int64  `json:"account_wait_ms"`
	Total       int64  `json:"total_ms"`
	First       *int64 `json:"first_byte_ms"`
	Execution   *int64 `json:"execution_ms"`
}
type callMonitor struct {
	mu         sync.Mutex
	buckets    [60]callBucket
	recent     [128]callObservation
	next, size int
}

var timingBounds = [...]int64{0, 1, 5, 10, 25, 50, 100, 250, 500, 1000, 2000, 5000, 10000, 30000, 60000, 120000, 300000, 3600000, math.MaxInt64}

func addTiming(hist *[19]int64, ms int64) {
	i := sort.Search(len(timingBounds), func(i int) bool { return timingBounds[i] >= ms })
	if i == len(timingBounds) {
		i--
	}
	hist[i]++
}
func percentile(hist [19]int64, count int64) any {
	if count == 0 {
		return nil
	}
	threshold := (count*95 + 99) / 100
	var n int64
	for i, v := range hist {
		n += v
		if n >= threshold {
			if i == len(timingBounds)-1 {
				return nil
			} // overflow has no finite upper bound
			return timingBounds[i]
		}
	}
	return nil
}
func (m *callMonitor) record(t streamwatch.TimingSnapshot, status int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	minute := time.Now().Unix() / 60
	b := &m.buckets[minute%60]
	if b.minute != minute {
		*b = callBucket{minute: minute}
	}
	b.Count++
	b.Attempts += int64(t.Attempts)
	b.Upstream429 += int64(t.Upstream429)
	if t.Outcome == "incomplete" {
		b.Incomplete++
	} else if t.Outcome == "error" || status >= 400 {
		b.Errors++
	}
	addTiming(&b.Wait, (t.Queue + t.Account).Milliseconds())
	addTiming(&b.Queue, t.Queue.Milliseconds())
	addTiming(&b.Account, t.Account.Milliseconds())
	duration := time.Since(t.Start)
	addTiming(&b.Total, duration.Milliseconds())
	observation := callObservation{RequestID: t.RequestID, Status: status, Outcome: t.Outcome, Attempts: t.Attempts, Upstream429: t.Upstream429, Queue: t.Queue.Milliseconds(), Account: t.Account.Milliseconds(), Total: duration.Milliseconds()}
	if observation.Outcome == "" {
		if status >= 400 {
			observation.Outcome = "error"
		} else {
			observation.Outcome = "ok"
		}
	}
	if t.Attempts > 0 {
		ms := max(int64(0), (duration - t.FirstAttempt - (t.Queue + t.Account - t.WaitAtAttempt)).Milliseconds())
		observation.Execution = &ms
		b.ExecutionCount++
		addTiming(&b.Execution, ms)
	}
	if t.BytesSeen {
		b.FirstCount++
		addTiming(&b.First, t.FirstByte.Milliseconds())
		ms := t.FirstByte.Milliseconds()
		observation.First = &ms
	}
	m.recent[m.next] = observation
	m.next = (m.next + 1) % len(m.recent)
	if m.size < len(m.recent) {
		m.size++
	}
}
func (m *callMonitor) snapshot() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	var total callBucket
	minute := time.Now().Unix() / 60
	for _, b := range m.buckets {
		if minute-b.minute < 0 || minute-b.minute >= 60 {
			continue
		}
		total.Count += b.Count
		total.Errors += b.Errors
		total.Incomplete += b.Incomplete
		total.Attempts += b.Attempts
		total.Upstream429 += b.Upstream429
		total.FirstCount += b.FirstCount
		total.ExecutionCount += b.ExecutionCount
		for i := range total.Wait {
			total.Wait[i] += b.Wait[i]
			total.First[i] += b.First[i]
			total.Total[i] += b.Total[i]
			total.Queue[i] += b.Queue[i]
			total.Account[i] += b.Account[i]
			total.Execution[i] += b.Execution[i]
		}
	}
	recent := make([]callObservation, 0, m.size)
	for i := 0; i < m.size; i++ {
		recent = append(recent, m.recent[(m.next-1-i+len(m.recent))%len(m.recent)])
	}
	return map[string]any{"window_seconds": 3600, "requests": total.Count, "errors": total.Errors, "incomplete": total.Incomplete, "attempts": total.Attempts, "upstream_429": total.Upstream429, "wait_p95_ms": percentile(total.Wait, total.Count), "queue_p95_ms": percentile(total.Queue, total.Count), "account_wait_p95_ms": percentile(total.Account, total.Count), "execution_p95_ms": percentile(total.Execution, total.ExecutionCount), "first_byte_p95_ms": percentile(total.First, total.FirstCount), "total_p95_ms": percentile(total.Total, total.Count), "approximate": true, "recent": recent}
}

type observedWriter struct {
	http.ResponseWriter
	timing *streamwatch.Timing
	status int
}

func (w *observedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *observedWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	if status >= 100 && status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *observedWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	n, err := w.ResponseWriter.Write(p)
	if n > 0 {
		w.timing.FirstWrite()
	}
	return n, err
}
func (w *observedWriter) Flush() { _ = http.NewResponseController(w.ResponseWriter).Flush() }
func (s *Server) observeCalls(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isModelRoute(r) {
			next.ServeHTTP(w, r)
			return
		}
		ctx, t := streamwatch.WithTiming(r.Context())
		r = r.WithContext(ctx)
		var id [16]byte
		if _, err := rand.Read(id[:]); err == nil {
			t.RequestID = hex.EncodeToString(id[:])
			w.Header().Set("X-Request-Id", t.RequestID)
		}
		writer := &observedWriter{ResponseWriter: w, timing: t}
		defer func() {
			if r.Context().Err() != nil {
				streamwatch.Outcome(ctx, "error")
			}
			s.calls.record(t.Snapshot(), writer.status)
		}()
		next.ServeHTTP(writer, r)
	})
}
