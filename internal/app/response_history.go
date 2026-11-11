package app

import (
	"bufio"
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"work2api/internal/config"
	"work2api/internal/core/provider"
)

const responseHistoryCaptureLimit = 64 << 20

type responseHistoryKey struct{ scope, id string }
type responseHistoryOrigin struct{ Provider, Account, Model string }
type responseHistoryContextKey struct{}
type responseHistoryReplay struct {
	mu       sync.Mutex
	current  responseHistoryOrigin
	restored []responseHistoryOrigin
	total    int
}

func (h *responseHistoryReplay) dispatched(ref provider.AccountRef, model string) {
	h.mu.Lock()
	h.current = responseHistoryOrigin{ref.Provider, ref.LocalID, model}
	h.mu.Unlock()
}
func (h *responseHistoryReplay) origin() responseHistoryOrigin {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.current
}
func (h *responseHistoryReplay) prepareBody(ref provider.AccountRef, model string, body []byte) ([]byte, error) {
	if len(h.restored) == 0 {
		return body, nil
	}
	target := responseHistoryOrigin{ref.Provider, ref.LocalID, model}
	foreign := false
	for _, origin := range h.restored {
		if origin != target || origin.Provider == "" {
			foreign = true
			break
		}
	}
	if !foreign {
		return body, nil
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return body, nil
	}
	var items []json.RawMessage
	if json.Unmarshal(payload["input"], &items) != nil || len(items) != h.total {
		return body, nil
	}
	next := make([]json.RawMessage, 0, len(items))
	changed := false
	for index, raw := range items {
		if index >= len(h.restored) || h.restored[index] == target && target.Provider != "" {
			next = append(next, raw)
			continue
		}
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil {
			next = append(next, raw)
			continue
		}
		var kind string
		_ = json.Unmarshal(item["type"], &kind)
		if kind == "" {
			var role string
			_ = json.Unmarshal(item["role"], &role)
			if role == "user" || role == "assistant" || role == "system" || role == "developer" {
				kind = "message"
			}
		}
		switch kind {
		case "reasoning":
			var encrypted string
			_ = json.Unmarshal(item["encrypted_content"], &encrypted)
			if encrypted == "" {
				changed = true
				continue
			}
			if _, ok := item["id"]; !ok {
				next = append(next, raw)
				continue
			}
		case "message", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output":
			if _, ok := item["id"]; !ok {
				next = append(next, raw)
				continue
			}
		default:
			next = append(next, raw)
			continue
		}
		delete(item, "id")
		encoded, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		next = append(next, encoded)
		changed = true
	}
	if !changed {
		return body, nil
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return nil, err
	}
	payload["input"] = encoded
	return json.Marshal(payload)
}

type responseHistorySegment struct {
	origin responseHistoryOrigin
	items  []json.RawMessage
	bytes  int64
	refs   int
}
type responseHistoryEntry struct {
	key      responseHistoryKey
	segments []*responseHistorySegment
	created  time.Time
	bytes    int64
	refs     int
	expiry   *list.Element
}
type responseHistoryCache struct {
	mu          sync.Mutex
	items       map[responseHistoryKey]*list.Element
	order       list.List
	expirations list.List
	used        int64
	limit       int64
	ttl         time.Duration
	now         func() time.Time
	timer       *time.Timer
}

func newResponseHistoryCache(limit int64, ttl time.Duration) *responseHistoryCache {
	return &responseHistoryCache{items: map[responseHistoryKey]*list.Element{}, limit: limit, ttl: ttl, now: time.Now}
}
func (c *responseHistoryCache) enabled() bool { return c != nil && c.limit > 0 && c.ttl > 0 }

// Entry leases keep shared ancestors charged even when their IDs are evicted.
func (c *responseHistoryCache) releaseLocked(e *responseHistoryEntry) {
	e.refs--
	if e.refs != 0 {
		return
	}
	c.used -= e.bytes
	for _, segment := range e.segments {
		segment.refs--
		if segment.refs == 0 {
			c.used -= segment.bytes
		}
	}
}
func (c *responseHistoryCache) drop(element *list.Element) {
	e := element.Value.(*responseHistoryEntry)
	c.order.Remove(element)
	c.expirations.Remove(e.expiry)
	delete(c.items, e.key)
	c.releaseLocked(e)
}
func (c *responseHistoryCache) acquire(key responseHistoryKey) *responseHistoryEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	element := c.items[key]
	if element == nil {
		return nil
	}
	e := element.Value.(*responseHistoryEntry)
	if c.now().Sub(e.created) >= c.ttl {
		c.drop(element)
		return nil
	}
	e.refs++
	c.order.MoveToFront(element)
	return e
}
func (c *responseHistoryCache) release(e *responseHistoryEntry) {
	if e == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.releaseLocked(e)
}
func (c *responseHistoryCache) put(key responseHistoryKey, parent *responseHistoryEntry, items []json.RawMessage, origins ...responseHistoryOrigin) bool {
	if len(key.id) == 0 || len(key.id) > 512 {
		return false
	}
	segment := &responseHistorySegment{items: items, bytes: 128 + int64(cap(items))*24}
	if len(origins) > 0 {
		segment.origin = origins[0]
		segment.bytes += int64(len(segment.origin.Provider)+len(segment.origin.Account)+len(segment.origin.Model)) + 48
	}
	for _, raw := range items {
		segment.bytes += int64(cap(raw))
	}
	count := 1
	if parent != nil {
		count += len(parent.segments)
	}
	meta := int64(512 + len(key.scope) + len(key.id) + count*8)
	if segment.bytes+meta > c.limit {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing := c.items[key]; existing != nil {
		if c.now().Sub(existing.Value.(*responseHistoryEntry).created) >= c.ttl {
			c.drop(existing)
		} else {
			return false
		}
	}
	for c.used+segment.bytes+meta > c.limit && c.order.Len() > 0 {
		c.drop(c.order.Back())
	}
	if c.used+segment.bytes+meta > c.limit {
		return false
	}
	e := &responseHistoryEntry{key: key, created: c.now(), bytes: meta, refs: 1, segments: make([]*responseHistorySegment, count)}
	if parent != nil {
		copy(e.segments, parent.segments)
	}
	e.segments[count-1] = segment
	for _, s := range e.segments {
		if s.refs == 0 {
			c.used += s.bytes
		}
		s.refs++
	}
	c.used += meta
	c.items[key] = c.order.PushFront(e)
	e.expiry = c.expirations.PushBack(e)
	if c.timer == nil {
		c.timer = time.AfterFunc(c.ttl, c.expire)
	}
	return true
}

func (c *responseHistoryCache) expire() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.timer = nil
	for c.expirations.Len() > 0 {
		entry := c.expirations.Front().Value.(*responseHistoryEntry)
		remaining := c.ttl - c.now().Sub(entry.created)
		if remaining > 0 {
			c.timer = time.AfterFunc(remaining, c.expire)
			break
		}
		c.drop(c.items[entry.key])
	}
}

func responseHistoryInput(value any) ([]any, bool) {
	switch input := value.(type) {
	case nil:
		return nil, true
	case string:
		return []any{map[string]any{"type": "message", "role": "user", "content": input}}, true
	case []any:
		for _, item := range input {
			if _, ok := item.(map[string]any); !ok {
				return nil, false
			}
		}
		return input, true
	default:
		return nil, false
	}
}

// Estimate before encoding so oversized inputs simply skip caching.
func responseHistoryFits(value any, remaining *int64, depth int) bool {
	if depth > 64 || *remaining < 0 {
		return false
	}
	*remaining -= 32
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			*remaining -= responseHistoryStringSize(key) + 8
			if !responseHistoryFits(child, remaining, depth+1) {
				return false
			}
		}
	case []any:
		for _, child := range v {
			if !responseHistoryFits(child, remaining, depth+1) {
				return false
			}
		}
	case string:
		*remaining -= responseHistoryStringSize(v)
	case nil, bool, float64, int, int64, json.Number:
	default:
		return false
	}
	return *remaining >= 0
}

func responseHistoryStringSize(s string) int64 {
	n := int64(len(s) + 2)
	for _, b := range []byte(s) {
		switch {
		case b < 32 || b == '<' || b == '>' || b == '&':
			n += 5
		case b == '\\' || b == '"':
			n++
		}
	}
	return n + int64(strings.Count(s, "\u2028")+strings.Count(s, "\u2029"))*3
}

type responseHistoryLimitError struct{}

func (responseHistoryLimitError) Error() string {
	return "续接后的完整请求超过现有字节或 JSON 结构保护，请缩短历史或调整现有配置"
}

func responseHistoryWithinLimits(value any, maxBytes int64, maxItems, maxDepth int) bool {
	var items int
	var walk func(any, int) bool
	walk = func(value any, depth int) bool {
		if maxBytes < 0 || (maxItems > 0 && items > maxItems) {
			return false
		}
		switch v := value.(type) {
		case map[string]any:
			if maxDepth > 0 && depth > maxDepth {
				return false
			}
			items += 1 + len(v)*2
			maxBytes -= 2
			for k, child := range v {
				maxBytes -= responseHistoryStringSize(k) + 2
				if !walk(child, depth+1) {
					return false
				}
			}
		case []any:
			if maxDepth > 0 && depth > maxDepth {
				return false
			}
			items += len(v)
			maxBytes -= 2 + int64(len(v))
			for _, child := range v {
				if !walk(child, depth+1) {
					return false
				}
			}
		case string:
			maxBytes -= responseHistoryStringSize(v)
		case float64:
			maxBytes -= int64(len(strconv.FormatFloat(v, 'g', -1, 64)))
		case json.Number:
			maxBytes -= int64(len(v))
		default:
			maxBytes -= 5
		}
		return maxBytes >= 0 && (maxItems <= 0 || items <= maxItems)
	}
	return walk(value, 1)
}

func responseHistoryRawAllowed(raw []byte, remaining *int, bounded bool, maxDepth int) bool {
	depth := 2
	inString, escaped := false, false
	for _, ch := range raw {
		if inString {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case '{', '[':
			depth++
			*remaining -= 1
		case '}', ']':
			depth--
		case ',', ':':
			*remaining -= 1
		}
		if (bounded && *remaining < 0) || (maxDepth > 0 && depth > maxDepth) {
			return false
		}
	}
	*remaining -= 1
	return !bounded || *remaining >= 0
}

func (s *Server) prepareResponseHistory(w http.ResponseWriter, r *http.Request, payload map[string]any, principal *Principal) (http.ResponseWriter, func(), error) {
	c := s.responseHistory
	noop := func() {}
	if !c.enabled() {
		return w, noop, nil
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d/%d/", principal.AppID, principal.UserID) + r.Header.Get("Authorization") + "\x00" + r.Header.Get("X-Api-Key")))
	scope := hex.EncodeToString(sum[:])
	history, _ := r.Context().Value(responseHistoryContextKey{}).(*responseHistoryReplay)
	if history == nil {
		history = &responseHistoryReplay{}
	}
	fresh, valid := responseHistoryInput(payload["input"])
	var parent *responseHistoryEntry
	if previous := payload["previous_response_id"]; previous != nil && previous != "" {
		id, ok := previous.(string)
		if !ok || !valid || len(id) > 512 {
			return w, noop, errors.New("previous_response_id 或 input 无效，请发送完整对话历史")
		}
		parent = c.acquire(responseHistoryKey{scope, id})
		if parent == nil {
			return w, noop, errors.New("找不到 previous_response_id：缓存可能已过期、被淘汰或服务已重启，请发送完整对话历史")
		}
		limit := s.o.cfg.MaxRequestBytes
		if limit <= 0 {
			limit = config.DefaultMaxRequestBytes
		}
		remainingBytes, remainingItems := limit, s.o.cfg.MaxJSONItems
		for _, segment := range parent.segments {
			for _, raw := range segment.items {
				remainingBytes -= int64(len(raw))
				if remainingBytes < 0 || !responseHistoryRawAllowed(raw, &remainingItems, s.o.cfg.MaxJSONItems > 0, s.o.cfg.MaxJSONDepth) {
					c.release(parent)
					return w, noop, responseHistoryLimitError{}
				}
			}
		}
		for _, segment := range parent.segments {
			for range segment.items {
				history.restored = append(history.restored, segment.origin)
			}
		}
		history.total = len(history.restored) + len(fresh)
		var restoredItems []any
		for _, segment := range parent.segments {
			for _, raw := range segment.items {
				var item map[string]any
				if json.Unmarshal(raw, &item) != nil {
					c.release(parent)
					return w, noop, errors.New("续接历史不可用，请发送完整对话历史")
				}
				restoredItems = append(restoredItems, item)
			}
		}
		payload["input"] = append(restoredItems, fresh...)
		delete(payload, "previous_response_id")
		if !responseHistoryWithinLimits(payload, limit, s.o.cfg.MaxJSONItems, s.o.cfg.MaxJSONDepth) {
			c.release(parent)
			return w, noop, responseHistoryLimitError{}
		}
	}
	release := func() { c.release(parent) }
	store := payload["store"] != false
	budget := c.limit
	if !store || !valid || !responseHistoryFits(fresh, &budget, 0) {
		return w, release, nil
	}
	inputs := make([]json.RawMessage, len(fresh))
	for i, item := range fresh {
		raw, err := json.Marshal(item)
		if err != nil {
			return w, release, nil
		}
		inputs[i] = raw
	}
	capture := &responseHistoryWriter{ResponseWriter: w, limit: responseHistoryCaptureLimit}
	var output http.ResponseWriter = capture
	if _, ok := w.(http.Flusher); ok {
		output = &responseHistoryFlushWriter{capture}
	}

	capture.publish = func() bool {
		if r.Context().Err() != nil || capture.failed || capture.status != http.StatusOK || capture.file == nil {
			return false
		}
		if _, err := capture.file.Seek(0, io.SeekStart); err != nil {
			return false
		}
		id, result, ok := readResponseHistoryResult(capture.file, capture.Header().Get("Content-Type"))
		_, _ = capture.file.Seek(0, io.SeekEnd)
		if !ok {
			return false
		}
		items := make([]json.RawMessage, len(inputs)+len(result))
		copy(items, inputs)
		copy(items[len(inputs):], result)
		capture.saved = c.put(responseHistoryKey{scope, id}, parent, items, history.origin())
		capture.savedID = id
		return true
	}
	return output, func() {
		defer release()
		defer capture.close()
		if capture.failed {
			if capture.saved {
				c.mu.Lock()
				if e := c.items[responseHistoryKey{scope, capture.savedID}]; e != nil {
					c.drop(e)
				}
				c.mu.Unlock()
			}
			return
		}
		if !capture.published && r.Context().Err() == nil {
			capture.publish()
		}
	}, nil
}

type responseHistoryWriter struct {
	http.ResponseWriter
	file          *os.File
	limit         int64
	kept          int64
	status        int
	failed        bool
	publish       func() bool
	published     bool
	saved         bool
	savedID       string
	terminal      bool
	frameBoundary bool
	tail          string
}

func (w *responseHistoryWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseHistoryWriter) WriteHeader(status int) {
	if status >= 100 && status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *responseHistoryWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.status == http.StatusOK && !w.failed && !w.published {
		if int64(len(p)) > w.limit-w.kept {
			w.failed = true
			w.close()
		} else {
			if w.file == nil {
				var err error
				w.file, err = os.CreateTemp("", "work2api-prevresp-*")
				if err != nil {
					w.failed = true
				} else {
					// On platforms permitting it, the open handle survives unlinking.
					_ = os.Remove(w.file.Name())
				}
			}
			if !w.failed {
				if n, err := w.file.Write(p); err != nil || n != len(p) {
					w.failed = true
					w.close()
				} else {
					w.kept += int64(len(p))
					if strings.Contains(w.Header().Get("Content-Type"), "event-stream") {
						// The marker can be split across writes; ordinary deltas never rescan the spool.
						prefix := p
						if len(prefix) > 64 {
							prefix = prefix[:64]
						}
						text := w.tail + string(prefix)
						boundaryTail := w.tail
						if len(boundaryTail) > 3 {
							boundaryTail = boundaryTail[len(boundaryTail)-3:]
						}
						boundaryPrefix := p
						if len(boundaryPrefix) > 4 {
							boundaryPrefix = boundaryPrefix[:4]
						}
						boundary := boundaryTail + string(boundaryPrefix)
						w.frameBoundary = bytes.Contains(p, []byte("\n\n")) || bytes.Contains(p, []byte("\r\n\r\n")) || strings.Contains(boundary, "\n\n") || strings.Contains(boundary, "\r\n\r\n")
						for _, marker := range []string{`"type":"response.completed"`, `"type": "response.completed"`, `"type":"response.incomplete"`, `"type": "response.incomplete"`, "event: response.completed", "event: response.incomplete"} {
							if bytes.Contains(p, []byte(marker)) || strings.Contains(text, marker) {
								w.terminal = true
								break
							}
						}
						if len(p) >= 64 {
							w.tail = string(p[len(p)-64:])
						} else {
							if len(text) > 64 {
								text = text[len(text)-64:]
							}
							w.tail = text
						}
					} else {
						w.terminal = len(bytes.TrimSpace(p)) > 0 && bytes.TrimSpace(p)[len(bytes.TrimSpace(p))-1] == '}'
					}
					ready := w.terminal
					if strings.Contains(w.Header().Get("Content-Type"), "event-stream") {
						ready = ready && w.frameBoundary
					}
					if ready && w.publish != nil {
						w.published = w.publish()
					}
				}
			}
		}
	}
	n, err := w.ResponseWriter.Write(p)
	if err != nil || n != len(p) {
		w.failed = true
	}
	return n, err
}
func (w *responseHistoryWriter) close() {
	if w.file != nil {
		name := w.file.Name()
		_ = w.file.Close()
		_ = os.Remove(name)
		w.file = nil
	}
}

type responseHistoryFlushWriter struct{ *responseHistoryWriter }

func (w *responseHistoryFlushWriter) FlushError() error {
	err := http.NewResponseController(w.ResponseWriter).Flush()
	if err != nil {
		w.failed = true
	}
	return err
}
func (w *responseHistoryFlushWriter) Flush() { _ = w.FlushError() }

func readResponseHistoryResult(reader io.Reader, contentType string) (string, []json.RawMessage, bool) {
	type response struct {
		ID     string            `json:"id"`
		Object string            `json:"object"`
		Status string            `json:"status"`
		Output []json.RawMessage `json:"output"`
	}

	valid := func(r response) bool {
		if r.ID == "" || len(r.ID) > 512 || (r.Object != "" && r.Object != "response") || (r.Status != "completed" && r.Status != "incomplete") || r.Output == nil {
			return false
		}
		for _, raw := range r.Output {
			if len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
				return false
			}
		}
		return true
	}
	if !strings.Contains(contentType, "event-stream") {
		var result response
		dec := json.NewDecoder(reader)
		if dec.Decode(&result) != nil || !valid(result) {
			return "", nil, false
		}
		var extra any
		if dec.Decode(&extra) != io.EOF {
			return "", nil, false
		}
		return result.ID, result.Output, true
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), responseHistoryCaptureLimit)
	var final response
	var data []byte
	var eventName string
	done := map[int]json.RawMessage{}
	consume := func() bool {
		if len(data) == 0 {
			eventName = ""
			return true
		}
		var event struct {
			Type     string          `json:"type"`
			Response response        `json:"response"`
			Index    int             `json:"output_index"`
			Item     json.RawMessage `json:"item"`
		}
		err := json.Unmarshal(data, &event)
		data = nil
		if err != nil {
			eventName = ""
			return true
		}
		if event.Type == "" {
			event.Type = eventName
		}
		eventName = ""
		switch event.Type {
		case "response.failed", "error":
			return false
		case "response.output_item.done":
			if event.Index >= 0 && len(event.Item) > 0 {
				done[event.Index] = event.Item
			}
		case "response.completed", "response.incomplete":
			final = event.Response
			if final.Status == "" {
				final.Status = strings.TrimPrefix(event.Type, "response.")
			}
			if final.Output == nil && len(done) > 0 {
				indices := make([]int, 0, len(done))
				for index := range done {
					indices = append(indices, index)
				}
				sort.Ints(indices)
				for i, index := range indices {
					if i != index {
						return false
					}
					final.Output = append(final.Output, done[index])
				}
			}
		}
		return true
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			if !consume() {
				return "", nil, false
			}
			continue
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			eventName = strings.TrimSpace(string(line[6:]))
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			field := line[5:]
			if len(field) > 0 && field[0] == ' ' {
				field = field[1:]
			}
			if len(data) > 0 {
				data = append(data, '\n')
			}
			if len(data)+len(field) > responseHistoryCaptureLimit {
				return "", nil, false
			}
			data = append(data, field...)
		}
	}
	// An unterminated SSE frame is not an acknowledged terminal response.
	return final.ID, final.Output, scanner.Err() == nil && valid(final)
}
