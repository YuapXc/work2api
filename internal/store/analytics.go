package store

import (
	"database/sql"
	"log"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// One read-only connection lets WAL readers coexist with authentication and
// log writes, without allowing unbounded parallel full-history scans. Memory
// and custom URI databases retain their original connection semantics.
func (d *DB) openAnalytics(path string) {
	reader, err := newReadOnlyAnalytics(path)
	if err == nil && reader != nil {
		err = reader.Ping()
	}
	if err != nil {
		if reader != nil {
			_ = reader.Close()
		}
		log.Printf("统计只读连接不可用，沿用主连接: %v", err)
		return
	}
	d.analyticsDB = reader
}

func newReadOnlyAnalytics(path string) (*sql.DB, error) {
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	name := filepath.ToSlash(abs)
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	u := url.URL{Scheme: "file", Path: name}
	q := url.Values{"mode": {"ro"}, "_pragma": {"query_only(1)", "busy_timeout(5000)"}}
	u.RawQuery = q.Encode()
	reader, err := sql.Open("sqlite", u.String())
	if err == nil {
		reader.SetMaxOpenConns(1)
	}
	return reader, err
}

func (d *DB) analyticsReader() (*sql.DB, func()) {
	if d.analyticsDB != nil {
		return d.analyticsDB, func() {}
	}
	d.mu.Lock()
	return d.db, d.mu.Unlock
}

// Hold only the cache's own lock while building: concurrent dashboard requests
// share a computation, while live traffic continues on the writer connection.
type analyticsCache[T any] struct {
	mu    sync.Mutex
	value T
	until time.Time
	key   float64
}

func (c *analyticsCache[T]) get(ttl time.Duration, key float64, load func() (T, error), clone func(T) T) (T, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	// Budget callers supply seconds; keep the requested window within the TTL.
	if now.Before(c.until) && key >= c.key && key-c.key < ttl.Seconds() {
		return clone(c.value), nil
	}
	v, err := load()
	if err != nil {
		return v, err
	}
	c.value, c.key, c.until = v, key, time.Now().Add(ttl)
	return clone(v), nil
}

func cloneAnalyticsMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		switch x := v.(type) {
		case []string:
			out[k] = append([]string{}, x...)
		case map[string]any:
			out[k] = cloneAnalyticsMap(x)
		case []map[string]any:
			items := make([]map[string]any, len(x))
			for i := range x {
				items[i] = cloneAnalyticsMap(x[i])
			}
			out[k] = items
		default:
			out[k] = v
		}
	}
	return out
}

func cloneBudgetSamples(in []BudgetSample) []BudgetSample {
	out := append([]BudgetSample{}, in...)
	for i := range out {
		if in[i].Coefficient != nil {
			v := *in[i].Coefficient
			out[i].Coefficient = &v
		}
	}
	return out
}

// UsageToday is the overview's bounded, index-backed query. Historical charts
// keep their full summary endpoint rather than computing unused groups here.
func (d *DB) UsageToday() (map[string]any, error) {
	db, release := d.analyticsReader()
	defer release()
	var count, tokens int64
	err := db.QueryRow("SELECT COUNT(*), COALESCE(SUM(total_tokens),0) FROM usage_logs WHERE ts>=?", localMidnight()).Scan(&count, &tokens)
	return map[string]any{"today_requests": count, "today_tokens": tokens}, err
}
