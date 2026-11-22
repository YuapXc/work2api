package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"work2api/internal/core/provider"
)

type queueCheckKey struct{}
type queueHeartbeatKey struct{}

func WithQueueDispatchCheck(ctx context.Context, check func(context.Context) error) context.Context {
	return context.WithValue(ctx, queueCheckKey{}, check)
}
func withQueueHeartbeat(ctx context.Context, beat func()) context.Context {
	return context.WithValue(ctx, queueHeartbeatKey{}, beat)
}

type queueDiagnosticsKey struct{}
type queueDiagnostics struct {
	mu    sync.Mutex
	value *provider.QueueDiagnostics
}
type queueObservation struct {
	mu      sync.Mutex
	closed  bool
	started time.Time
	value   provider.QueueDiagnostics
}

func WithQueueDiagnostics(ctx context.Context) (context.Context, func() *provider.QueueDiagnostics) {
	d := &queueDiagnostics{}
	return context.WithValue(ctx, queueDiagnosticsKey{}, d), func() *provider.QueueDiagnostics {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.value == nil {
			return nil
		}
		value := *d.value
		return &value
	}
}

func queueCount(n *float64) *float64 {
	if n == nil || *n < 0 || math.IsNaN(*n) || math.IsInf(*n, 0) || math.Trunc(*n) != *n {
		return nil
	}
	v := *n
	return &v
}
func (o *queueObservation) observe(state queueState) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	o.value.LastCount = queueCount(state.Count)
	o.value.Queued, o.value.Available = state.Queued, state.Available
}
func (o *queueObservation) query() {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}
	o.value.Queries++
	o.value.LastHTTPStatus = 0
}
func (o *queueObservation) queryStatus(status int) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.closed {
		o.value.LastHTTPStatus = status
	}
}
func (o *queueObservation) reason(reason string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.closed {
		o.value.ExitReason = reason
	}
}
func (d *queueDiagnostics) finish(o *queueObservation, ready bool, err error) {
	o.mu.Lock()
	o.closed = true
	value := o.value
	o.mu.Unlock()
	value.Waits = 1
	value.WaitMS = max(0, time.Since(o.started).Milliseconds())
	switch {
	case errors.Is(err, context.Canceled):
		value.ExitReason = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		value.ExitReason = "timeout"
	case errors.Is(err, errQueueStalled):
		value.ExitReason = "stalled"
	case ready:
		value.ExitReason = "ready"
	case value.ExitReason == "":
		value.ExitReason = "wait_failed"
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.value != nil {
		value.Waits += d.value.Waits
		value.WaitMS += d.value.WaitMS
		value.Queries += d.value.Queries
		value.FirstCount = d.value.FirstCount
	}
	d.value = &value
}

type queueState struct {
	Queued    *bool    `json:"isQueued"`
	Available *bool    `json:"serviceAvailable"`
	Model     string   `json:"modelKey"`
	Type      string   `json:"queueType"`
	Retry     *float64 `json:"retryAfterSeconds"`
	Count     *float64 `json:"queueCount"`
	Sub       string   `json:"subCode"`
}

func readQueueState(raw []byte) (queueState, bool) {
	var walk func([]byte, int) (queueState, bool)
	nodes := 32
	walk = func(raw []byte, depth int) (queueState, bool) {
		if depth > 6 || nodes == 0 || len(raw) > 65536 {
			return queueState{}, false
		}
		nodes--
		var m map[string]json.RawMessage
		if json.Unmarshal(raw, &m) != nil {
			return queueState{}, false
		}
		flag := func(key string) *bool {
			var value bool
			if json.Unmarshal(m[key], &value) == nil && string(m[key]) != "null" {
				return &value
			}
			var text string
			if json.Unmarshal(m[key], &text) == nil && (text == "true" || text == "false") {
				value = text == "true"
				return &value
			}
			return nil
		}
		number := func(key string) *float64 {
			var n float64
			if json.Unmarshal(m[key], &n) == nil && string(m[key]) != "null" {
				return &n
			}
			var text string
			if json.Unmarshal(m[key], &text) == nil && text != "" {
				if n, err := strconv.ParseFloat(text, 64); err == nil && !math.IsNaN(n) && !math.IsInf(n, 0) {
					return &n
				}
			}
			return nil
		}
		state := queueState{Queued: flag("isQueued"), Available: flag("serviceAvailable"), Retry: number("retryAfterSeconds"), Count: number("queueCount")}
		_ = json.Unmarshal(m["modelKey"], &state.Model)
		_ = json.Unmarshal(m["queueType"], &state.Type)
		_ = json.Unmarshal(m["subCode"], &state.Sub)
		if state.Queued != nil || state.Available != nil {
			return state, true
		}
		for _, key := range []string{"data", "result", "message", "body", "error", "details"} {
			child := m[key]
			var text string
			if json.Unmarshal(child, &text) == nil {
				child = []byte(text)
			}
			if result, ok := walk(child, depth+1); ok {
				return result, true
			}
		}
		return queueState{}, false
	}
	return walk(raw, 0)
}

func queuedFailure(err error) (queueState, bool) {
	var upstream *UpstreamError
	if !errors.As(err, &upstream) || upstream.ErrType != "upstream_queued" {
		return queueState{}, false
	}
	state, ok := readQueueState([]byte(upstream.Detail))
	return state, ok && (state.Queued == nil || *state.Queued)
}

func queuePause(state queueState) time.Duration {
	if state.Retry == nil {
		return 30 * time.Second
	}
	n := *state.Retry
	if n >= 30 {
		return 30 * time.Second
	}
	if n <= .5 {
		return 500 * time.Millisecond
	}
	return time.Duration(n * float64(time.Second))
}

func queueStuckKey(state queueState) string {
	if state.Count != nil && *state.Count > 1 {
		return ""
	}
	// Model names do not affect whether the queue has progressed.
	state.Model = ""
	raw, _ := json.Marshal(state)
	return string(raw)
}

var errQueueStalled = errors.New("queue stalled")
var queueNow = time.Now

func queueProgress(state queueState, previous string, since, now time.Time) (string, time.Time, bool) {
	key := queueStuckKey(state)
	if key == "" || key != previous {
		return key, now, false
	}
	return key, since, now.Sub(since) >= 5*time.Minute && (state.Queued == nil || *state.Queued)
}

func (b *Bridge) waitQueued(ctx context.Context, endpoint string, body map[string]interface{}, extra map[string]string, first queueState) (ready bool, err error) {
	if provider.CurrentInvocation(ctx).Diagnostic {
		return false, nil
	}
	var observation *queueObservation
	if diagnostics, _ := ctx.Value(queueDiagnosticsKey{}).(*queueDiagnostics); diagnostics != nil {
		observation = &queueObservation{started: time.Now(), value: provider.QueueDiagnostics{FirstCount: queueCount(first.Count)}}
		observation.observe(first)
		defer func() { diagnostics.finish(observation, ready, err) }()
	}
	err = provider.Wait(ctx, func(ctx context.Context) error {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		type result struct {
			ready bool
			err   error
		}
		done := make(chan result, 1)
		go func() {
			ok, err := b.pollQueue(ctx, endpoint, body, extra, first, observation)
			done <- result{ok, err}
		}()
		beat, _ := ctx.Value(queueHeartbeatKey{}).(func())
		if beat != nil {
			beat()
		}
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case r := <-done:
				ready = r.ready
				return r.err
			case <-ctx.Done():
				return ctx.Err()
			case <-ticker.C:
				if beat != nil {
					beat()
				}
			}
		}
	})
	return ready, err
}

func (b *Bridge) pollQueue(ctx context.Context, endpoint string, body map[string]interface{}, extra map[string]string, state queueState, observation *queueObservation) (bool, error) {
	base, err := url.Parse(endpoint)
	if err != nil {
		observation.reason("request_error")
		return false, err
	}
	base.Path, base.RawPath, base.RawQuery = "/algo/api/v2/service/ask/queue/status", "", ""
	model, _ := body["model_config"].(map[string]interface{})
	if state.Model == "" {
		state.Model = StrVal(model, "key")
	}
	requestSet := StrVal(body, "request_set_id")
	if requestSet == "" || state.Model == "" {
		observation.reason("invalid_ticket")
		return false, nil
	}
	key, since, failures := queueStuckKey(state), queueNow(), 0
	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if err := sleepCtx(ctx, queuePause(state)); err != nil {
				return false, err
			}
		}
		if check, _ := ctx.Value(queueCheckKey{}).(func(context.Context) error); check != nil {
			if err := check(ctx); err != nil {
				observation.reason("dispatch_denied")
				return false, err
			}
		}
		q := url.Values{"requestSetId": {requestSet}, "modelKey": {state.Model}}
		if state.Type != "" {
			q.Set("queueType", state.Type)
		}
		base.RawQuery = q.Encode()
		askCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		req, err := http.NewRequestWithContext(askCtx, http.MethodGet, base.String(), nil)
		if err != nil {
			cancel()
			return false, err
		}
		path, err := PathSigFrom(base.String())
		if err != nil {
			cancel()
			return false, err
		}
		headers, err := b.client.buildHeaders(path, "", "application/json", extra)
		if err != nil {
			cancel()
			return false, err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		req.Header.Del("Content-Type")
		req.Header.Set("X-Request-ID", StrVal(body, "request_id"))
		req.Header.Set("X-Session-ID", StrVal(body, "session_id"))
		observation.query()
		resp, err := streamHTTPClient.Do(req)
		var raw []byte
		status := 0
		if resp != nil {
			status = resp.StatusCode
			raw, err = io.ReadAll(io.LimitReader(resp.Body, 65537))
			resp.Body.Close()
		}
		cancel()
		observation.queryStatus(status)
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if err == nil && status == 404 {
			observation.reason("unsupported")
			return false, nil
		}
		if err == nil && status >= 400 && status < 500 && status != 429 {
			observation.reason("rejected")
			return false, NewUpstreamError(status, string(raw))
		}
		if err != nil || status != 200 || len(raw) > 65536 {
			failures++
			if failures >= 3 {
				observation.reason("query_failed")
				return false, nil
			}
			state.Retry = nil
			continue
		}
		next, ok := readQueueState(raw)
		if !ok {
			observation.reason("invalid_status")
			return false, nil
		}
		failures = 0
		if next.Model == "" {
			next.Model = state.Model
		}
		if next.Type == "" {
			next.Type = state.Type
		}
		state = next
		observation.observe(state)
		var stalled bool
		key, since, stalled = queueProgress(state, key, since, queueNow())
		if stalled {
			return false, errQueueStalled
		}
		if state.Queued != nil && !*state.Queued {
			if state.Retry != nil && *state.Retry > 0 {
				if err := sleepCtx(ctx, queuePause(state)); err != nil {
					return false, err
				}
			}
			return true, nil
		}
		if state.Queued != nil && *state.Queued || state.Available != nil && !*state.Available {
			continue
		}
		observation.reason("not_waiting")
		return false, nil
	}
}

func queueWaitError(previous, cause error, requestExpired bool) error {
	if errors.Is(cause, context.Canceled) {
		return cause
	}
	var original *UpstreamError
	if !errors.As(previous, &original) {
		return cause
	}
	copy := *original
	if copy.RetryAfter <= 0 {
		if state, ok := readQueueState([]byte(copy.Detail)); ok && state.Retry != nil && *state.Retry > 0 && *state.Retry < float64(math.MaxInt64)/float64(time.Second) {
			copy.RetryAfter = time.Duration(*state.Retry * float64(time.Second))
		}
	}
	copy.cause = cause
	switch {
	case errors.Is(cause, errQueueStalled):
		copy.Status = 503
		copy.ErrType = ErrTypeQueueStalled
		copy.Message += "（队列5分钟无进展且前方无人，已停止等待）"
	case errors.Is(cause, context.DeadlineExceeded):
		copy.Status = 503
		copy.ErrType = ErrTypeQueueTimeout
		copy.Message += "（上游排队等待已达上限）"
		if requestExpired {
			copy.Status = 504
			copy.Message = original.Message + "（请求在等待上游队列期间超时）"
		}
	default:
		return cause
	}
	return &copy
}
