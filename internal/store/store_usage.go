package store

import (
	"database/sql"
	"strings"
	"time"
)

// UsageCreditStats aggregates credits & tokens per model over rows with
// credits > 0 (real, trustworthy consumption), used by the overview to estimate
// "tokens per credit". Returns per-model rows plus totals.
func (d *DB) UsageCreditStats() (models []map[string]any, totalCredits, totalTokens float64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	models = []map[string]any{}
	rows, err := d.db.Query("SELECT model, COALESCE(SUM(credits),0), COALESCE(SUM(total_tokens),0) FROM usage_logs WHERE credits > 0 GROUP BY model ORDER BY 2 DESC")
	if err != nil {
		return models, 0, 0
	}
	defer rows.Close()
	for rows.Next() {
		var m sql.NullString
		var c, t float64
		if err := rows.Scan(&m, &c, &t); err != nil {
			continue
		}
		models = append(models, map[string]any{"model": m.String, "credits": c, "tokens": t})
		totalCredits += c
		totalTokens += t
	}
	return models, totalCredits, totalTokens
}

// GetSettings returns settings merged over defaults.
func (d *DB) GetSettings() (map[string]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query("SELECT key, value FROM settings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	merged := map[string]string{}
	for k, v := range DefaultSettings {
		merged[k] = v
	}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		merged[k] = v
	}
	return merged, nil
}

// SaveSettings persists whitelisted settings keys.
func (d *DB) SaveSettings(kv map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, v := range kv {
		if _, ok := DefaultSettings[k]; !ok {
			continue
		}
		if _, err := d.db.Exec(
			"INSERT INTO settings (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
			k, v); err != nil {
			return err
		}
	}
	return nil
}

// UsageParams are the fields for a usage log entry.
type UsageParams struct {
	Model            string
	Protocol         string
	AccountUID       string
	InputTokens      int
	OutputTokens     int
	LatencyMs        float64
	Status           string
	Error            string
	InputContent     string
	OutputContent    string
	ReasoningContent string
	Credits          *float64
	AppName          string
	ReasoningEffort  string
}

// LogUsage inserts one usage record.
func (d *DB) LogUsage(p UsageParams) error {
	if p.Status == "" {
		p.Status = "ok"
	}
	total := p.InputTokens + p.OutputTokens
	var credits any
	if p.Credits != nil {
		credits = *p.Credits
	}
	var effort any
	if p.ReasoningEffort != "" {
		effort = p.ReasoningEffort
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(
		`INSERT INTO usage_logs (ts, model, protocol, account_uid, input_tokens, output_tokens,
		   total_tokens, latency_ms, status, error, input_content, output_content,
		   reasoning_content, credits, app_name, user_id, reasoning_effort)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		float64(time.Now().UnixNano())/1e9, p.Model, p.Protocol, p.AccountUID,
		p.InputTokens, p.OutputTokens, total, p.LatencyMs, p.Status, p.Error,
		p.InputContent, p.OutputContent, p.ReasoningContent, credits, p.AppName, nil, effort)
	return err
}

// UsageRowCount returns the exact usage_logs row count (O(n); avoid on hot path).
func (d *DB) UsageRowCount() (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var c int
	err := d.db.QueryRow("SELECT COUNT(*) FROM usage_logs").Scan(&c)
	return c, err
}

// CleanupUsage deletes usage_logs older than retentionDays and returns how many
// rows were removed. retentionDays <= 0 is treated as a no-op (never wipe all).
func (d *DB) CleanupUsage(retentionDays int) (int64, error) {
	if retentionDays <= 0 {
		return 0, nil
	}
	cutoff := float64(time.Now().UnixNano())/1e9 - float64(retentionDays)*86400
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.db.Exec("DELETE FROM usage_logs WHERE ts < ?", cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// UsageRowSpan returns (min id, max id) via primary-key index reads — an O(1)
// precheck for whether the row count might exceed a cap, without a full COUNT(*).
// Span (hi-lo+1) equals the row count only when nothing was deleted; deletes
// inflate it (deleting old rows doesn't lower max id), so it's a "maybe over"
// hint — the exact trim count comes from CleanupUsageRows. Empty table => (0,0).
func (d *DB) UsageRowSpan() (int64, int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var lo, hi sql.NullInt64
	err := d.db.QueryRow("SELECT MIN(id), MAX(id) FROM usage_logs").Scan(&lo, &hi)
	return lo.Int64, hi.Int64, err
}

// CleanupUsageRows keeps only the newest maxRows usage_logs rows (by id, which
// is monotonic with insert order) and returns how many were deleted. maxRows<=0
// is a no-op. Uses a MAX(id) boundary subquery so SQLite walks the primary-key
// index instead of scanning the whole table.
func (d *DB) CleanupUsageRows(maxRows int) (int64, error) {
	if maxRows <= 0 {
		return 0, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.db.Exec(
		`DELETE FROM usage_logs WHERE id <= (
			SELECT MAX(id) FROM (
				SELECT id FROM usage_logs ORDER BY id DESC LIMIT -1 OFFSET ?
			)
		)`, maxRows)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// UsageSummary returns aggregate stats.
func (d *DB) UsageSummary() (map[string]any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]any{}
	var tc, tt int64
	_ = d.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(total_tokens),0) FROM usage_logs").Scan(&tc, &tt)
	out["total_requests"], out["total_tokens"] = tc, tt
	todayStart := localMidnight()
	var dc, dt int64
	_ = d.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(total_tokens),0) FROM usage_logs WHERE ts >= ?", todayStart).Scan(&dc, &dt)
	out["today_requests"], out["today_tokens"] = dc, dt

	byProtocol, _ := d.groupCount("SELECT protocol, COUNT(*), COALESCE(SUM(total_tokens),0) FROM usage_logs GROUP BY protocol", "protocol")
	out["by_protocol"] = byProtocol
	byModel, _ := d.groupCount("SELECT model, COUNT(*), COALESCE(SUM(total_tokens),0) FROM usage_logs GROUP BY model ORDER BY 2 DESC LIMIT 20", "model")
	out["by_model"] = byModel
	byApp, _ := d.groupCount("SELECT COALESCE(NULLIF(app_name,''),'(未命名)'), COUNT(*), COALESCE(SUM(total_tokens),0) FROM usage_logs GROUP BY app_name ORDER BY 2 DESC", "app")
	out["by_app"] = byApp

	out["by_account"] = []map[string]any{}
	rows, err := d.db.Query(`SELECT account_uid, COUNT(*), COALESCE(SUM(total_tokens),0),
		COALESCE(SUM(credits),0), SUM(CASE WHEN status='ok' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status='ok' AND credits IS NULL THEN 1 ELSE 0 END)
		FROM usage_logs GROUP BY account_uid ORDER BY 2 DESC`)
	if err == nil {
		defer rows.Close()
		byAccount := []map[string]any{}
		for rows.Next() {
			var uid sql.NullString
			var c, t, okc, unk int64
			var cr float64
			_ = rows.Scan(&uid, &c, &t, &cr, &okc, &unk)
			byAccount = append(byAccount, map[string]any{
				"account_uid": uid.String, "count": c, "tokens": t,
				"credits": round2(cr), "ok_count": okc, "unknown_count": unk,
			})
		}
		out["by_account"] = byAccount
	}
	return out, nil
}

func (d *DB) groupCount(query, keyName string) ([]map[string]any, error) {
	rows, err := d.db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var key sql.NullString
		var c, t int64
		if err := rows.Scan(&key, &c, &t); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{keyName: key.String, "count": c, "tokens": t})
	}
	return out, nil
}

// usageWhere builds the shared WHERE clause. app_name "" (non-nil) matches
// the unnamed bucket; pass nil (empty string with useApp=false) to skip.
func usageWhere(protocol, model string, appName *string, status, search string) (string, []any) {
	var clauses []string
	var params []any
	if protocol != "" {
		clauses = append(clauses, "protocol = ?")
		params = append(params, protocol)
	}
	if model != "" {
		clauses = append(clauses, "model = ?")
		params = append(params, model)
	}
	if appName != nil {
		clauses = append(clauses, "COALESCE(app_name, '') = ?")
		params = append(params, *appName)
	}
	if status != "" {
		clauses = append(clauses, "status = ?")
		params = append(params, status)
	}
	if search != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(search) + "%"
		clauses = append(clauses, `(input_content LIKE ? ESCAPE '\' OR output_content LIKE ? ESCAPE '\' OR reasoning_content LIKE ? ESCAPE '\')`)
		params = append(params, like, like, like)
	}
	if len(clauses) == 0 {
		return "", params
	}
	return "WHERE " + strings.Join(clauses, " AND "), params
}

const usageLightCols = "id, ts, model, protocol, account_uid, input_tokens, output_tokens, total_tokens, latency_ms, status, error, credits, app_name, user_id, reasoning_effort"

// UsageRecent returns recent usage records with optional filters.
func (d *DB) UsageRecent(limit int, protocol, model string, appName *string, status string, light bool, offset int, search string) ([]map[string]any, error) {
	where, params := usageWhere(protocol, model, appName, status, search)
	params = append(params, limit, offset)
	cols := "*"
	if light {
		cols = usageLightCols
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query("SELECT "+cols+" FROM usage_logs "+where+" ORDER BY id DESC LIMIT ? OFFSET ?", params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := scanRows(rows)
	if err != nil {
		return nil, err
	}
	for _, r := range list {
		_, known := r["credits"]
		r["credit_known"] = known && r["credits"] != nil
	}
	return list, nil
}

// UsageCount returns the count matching filters.
func (d *DB) UsageCount(protocol, model string, appName *string, status, search string) (int, error) {
	where, params := usageWhere(protocol, model, appName, status, search)
	d.mu.Lock()
	defer d.mu.Unlock()
	var c int
	err := d.db.QueryRow("SELECT COUNT(*) FROM usage_logs "+where, params...).Scan(&c)
	return c, err
}

// GetUsage returns one full usage record by id.
func (d *DB) GetUsage(id int) (map[string]any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query("SELECT * FROM usage_logs WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := scanRows(rows)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	r := list[0]
	_, known := r["credits"]
	r["credit_known"] = known && r["credits"] != nil
	return r, nil
}

// UsageFilters returns distinct filter values.
func (d *DB) UsageFilters() (map[string]any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	col := func(field string) []string {
		rows, err := d.db.Query("SELECT DISTINCT " + field + " FROM usage_logs WHERE " + field + " IS NOT NULL AND " + field + " != '' ORDER BY " + field)
		if err != nil {
			return []string{}
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var v sql.NullString
			_ = rows.Scan(&v)
			out = append(out, v.String)
		}
		return out
	}
	currentApps := []string{}
	if rows, err := d.db.Query("SELECT name FROM apps ORDER BY id"); err == nil {
		for rows.Next() {
			var n string
			_ = rows.Scan(&n)
			currentApps = append(currentApps, n)
		}
		rows.Close()
	}
	usedApps := col("app_name")
	currentSet := map[string]struct{}{}
	for _, a := range currentApps {
		currentSet[a] = struct{}{}
	}
	var history []string = []string{}
	for _, a := range usedApps {
		if _, ok := currentSet[a]; !ok {
			history = append(history, a)
		}
	}
	var hasUnnamed bool
	var n int
	if err := d.db.QueryRow("SELECT COUNT(*) FROM usage_logs WHERE app_name IS NULL OR app_name = ''").Scan(&n); err == nil {
		hasUnnamed = n > 0
	}
	return map[string]any{
		"protocols":    col("protocol"),
		"models":       col("model"),
		"apps":         currentApps,
		"apps_history": history,
		"has_unnamed":  hasUnnamed,
		"statuses":     col("status"),
	}, nil
}

// UsageTimeseries returns bucketed call trend.
func (d *DB) UsageTimeseries(granularity string, points int, model string) ([]map[string]any, error) {
	bucketSec := int64(3600)
	if granularity == "day" {
		bucketSec = 86400
	}
	now := time.Now().Unix()
	var end int64
	var bucketExpr string
	if granularity == "day" {
		end = localMidnight()
		shift := end - (now - now%86400)
		bucketExpr = "((CAST(ts AS INTEGER) - " + i64(shift) + ") / " + i64(bucketSec) + ") * " + i64(bucketSec) + " + " + i64(shift)
	} else {
		end = now - (now % bucketSec)
		bucketExpr = "(CAST(ts AS INTEGER) / " + i64(bucketSec) + ") * " + i64(bucketSec)
	}
	start := end - int64(points-1)*bucketSec
	query := "SELECT " + bucketExpr + " AS bucket_ts, COUNT(*), COALESCE(SUM(total_tokens),0) FROM usage_logs WHERE ts >= ? AND ts < ?"
	args := []any{start, end + bucketSec}
	if model != "" {
		query += " AND model = ?"
		args = append(args, model)
	}
	query += " GROUP BY bucket_ts"
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byBucket := map[int64][2]int64{}
	for rows.Next() {
		var bt, c, t int64
		_ = rows.Scan(&bt, &c, &t)
		byBucket[bt] = [2]int64{c, t}
	}
	out := []map[string]any{}
	layout := "01-02 15:04"
	if bucketSec == 86400 {
		layout = "01-02"
	}
	for i := 0; i < points; i++ {
		bt := start + int64(i)*bucketSec
		v := byBucket[bt]
		out = append(out, map[string]any{
			"bucket_ts": bt,
			"bucket":    time.Unix(bt, 0).Format(layout),
			"count":     v[0],
			"tokens":    v[1],
		})
	}
	return out, nil
}
