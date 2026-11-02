package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAnalyticsReadOnlyDoesNotBlockWriter(t *testing.T) {
	d, err := New(filepath.Join(t.TempDir(), "统计 # test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.analyticsDB == nil {
		t.Fatal("read-only connection not opened")
	}
	if _, err := d.analyticsDB.Exec("DELETE FROM usage_logs"); err == nil {
		t.Fatal("analytics connection is writable")
	}
	conn, err := d.analyticsDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Pin an actual WAL read snapshot while a writer commits.
	if _, err := conn.ExecContext(context.Background(), "BEGIN"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	var n int
	if err := conn.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM usage_logs").Scan(&n); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.LogUsage(UsageParams{Model: "test", Status: "ok", InputTokens: 2, OutputTokens: 3}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("analytics reader blocked log writer")
	}
}

func TestAnalyticsCacheCoalescesAndCopies(t *testing.T) {
	var c analyticsCache[map[string]any]
	var calls atomic.Int32
	load := func() (map[string]any, error) {
		calls.Add(1)
		return map[string]any{"by_account": []map[string]any{{"tokens": int64(8)}}}, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, e := c.get(time.Minute, 1, load, cloneAnalyticsMap)
			if e != nil {
				t.Error(e)
				return
			}
			v["by_account"].([]map[string]any)[0]["tokens"] = int64(99)
		}()
	}
	wg.Wait()
	v, _ := c.get(time.Minute, 1, load, cloneAnalyticsMap)
	if calls.Load() != 1 || v["by_account"].([]map[string]any)[0]["tokens"] != int64(8) {
		t.Fatalf("cache shared mutable state or recomputed: %v %d", v, calls.Load())
	}
	_, _ = c.get(time.Minute, 86401, load, cloneAnalyticsMap)
	if calls.Load() != 2 {
		t.Fatal("changed window reused stale cache")
	}
}

func TestAnalyticsMemoryFallbackAndFreshToday(t *testing.T) {
	d, err := New(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if d.analyticsDB != nil {
		t.Fatal("memory DB must use its original connection")
	}
	before, err := d.UsageSummary()
	if err != nil {
		t.Fatal(err)
	}
	if err := d.LogUsage(UsageParams{Model: "test", Status: "ok", InputTokens: 2, OutputTokens: 3}); err != nil {
		t.Fatal(err)
	}
	today, err := d.UsageToday()
	if err != nil {
		t.Fatal(err)
	}
	if today["today_requests"] != int64(1) || today["today_tokens"] != int64(5) {
		t.Fatalf("fresh overview: %v", today)
	}
	if before["total_requests"] != int64(0) {
		t.Fatal("snapshot mutated")
	}
	if _, err := d.UsageFilters(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ListApps(); err != nil {
		t.Fatal(err)
	}
}

func TestRequestGroupingExpressionIndex(t *testing.T) {
	d, err := New(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// Malformed historical diagnostics and uncorrelated legacy rows remain valid.
	if _, err := d.db.Exec(`INSERT INTO usage_logs(model,diagnostics) VALUES('legacy','broken'),('legacy','{}')`); err != nil {
		t.Fatal(err)
	}
	_, total, err := d.UsageRequests(nil, 20, 0, "", "", nil, "", "")
	if err != nil || total != 2 {
		t.Fatalf("legacy grouping %d %v", total, err)
	}
	rows, err := d.db.Query("EXPLAIN QUERY PLAN " + strings.Replace(requestLogCTE, "%s", "", 1) + "SELECT COUNT(*) FROM final")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var a, b, c int
		var detail string
		if err := rows.Scan(&a, &b, &c, &detail); err != nil {
			t.Fatal(err)
		}
		found = found || strings.Contains(detail, "idx_usage_request_group")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("grouping query did not use expression index")
	}
}

// Synthetic data only; compare uncached work against the actual dashboard path.
func BenchmarkDashboardAnalytics(b *testing.B) {
	d, err := New(filepath.Join(b.TempDir(), "analytics.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	if _, err := d.db.Exec("DROP INDEX idx_usage_request_group"); err != nil {
		b.Fatal(err)
	}
	tx, err := d.db.Begin()
	if err != nil {
		b.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO usage_logs(ts,model,protocol,account_uid,input_tokens,output_tokens,total_tokens,status,diagnostics) VALUES(?,?,?,?,10,10,20,'ok',?)`)
	if err != nil {
		b.Fatal(err)
	}
	now := time.Now().Unix()
	for i := 0; i < 100000; i++ {
		_, err = stmt.Exec(now-int64(i%30)*86400, fmt.Sprintf("model-%d", i%12), "chat", fmt.Sprintf("account-%d", i%20), fmt.Sprintf(`{"performance":{"request_id":"%032x"},"billing_domain":"cn","billing_coefficient":0.1}`, i))
		if err != nil {
			b.Fatal(err)
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	b.Run("full-summary", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := d.usageSummary(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("overview-today", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := d.UsageToday(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("cached-summary", func(b *testing.B) {
		d.UsageSummary()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, err := d.UsageSummary(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("request-groups", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, _, err := d.UsageRequests(nil, 20, 0, "", "", nil, "", ""); err != nil {
				b.Fatal(err)
			}
		}
	})
	if _, err := d.db.Exec("CREATE INDEX idx_usage_request_group ON usage_logs(" + requestGroupExpression + ")"); err != nil {
		b.Fatal(err)
	}
	b.Run("indexed-request-groups", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, _, err := d.UsageRequests(nil, 20, 0, "", "", nil, "", ""); err != nil {
				b.Fatal(err)
			}
		}
	})
}
