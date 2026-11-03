package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync/atomic"
	"time"
)

// BudgetSample contains aggregate billing evidence only, never prompt content.
type BudgetSample struct {
	AccountUID, Model              string
	Domain                         string
	Coefficient                    *float64
	Recent                         bool
	Credits, Tokens, ZeroTokens    float64
	Calls, ZeroCalls, UnknownCalls int
	LegacyCalls                    int
	UnknownTokens                  float64
}

// UsageBudgetSamples groups the last 30 days into a 7-day window and its older
// fallback. Successful rows with known positive tokens can calibrate cost.
// Legacy positive-cost rows with consistent token totals provide a separately
// marked reference estimate; explicitly unknown tokens are never promoted.
// Reported zero consumption is kept separate from unknown consumption.
func (d *DB) UsageBudgetSamples(now float64) ([]BudgetSample, error) {
	return d.budgetCache.get(time.Minute, now, func() ([]BudgetSample, error) { return d.usageBudgetSamples(now) }, cloneBudgetSamples)
}

func (d *DB) usageBudgetSamples(now float64) ([]BudgetSample, error) {
	db, release := d.analyticsReader()
	defer release()
	rows, err := db.Query(`SELECT COALESCE(account_uid,''),COALESCE(model,''),ts>=?,
	 COALESCE(SUM(CASE WHEN credits>0 THEN credits ELSE 0 END),0),
	 COALESCE(SUM(CASE WHEN credits>0 THEN total_tokens ELSE 0 END),0),
	 COUNT(CASE WHEN credits>0 THEN 1 END),
	 COALESCE(SUM(CASE WHEN credits=0 THEN total_tokens ELSE 0 END),0),
	 COUNT(CASE WHEN credits=0 THEN 1 END),COUNT(CASE WHEN credits IS NULL THEN 1 END),
	 COUNT(CASE WHEN tokens_known IS NULL THEN 1 END),
	 COALESCE(SUM(CASE WHEN credits IS NULL THEN total_tokens ELSE 0 END),0),
	 COALESCE(json_extract(diagnostics,'$.billing_domain'),''),
	 json_extract(diagnostics,'$.billing_coefficient')
	 FROM usage_logs WHERE ts>=? AND ts<=? AND status='ok' AND total_tokens>0
	 AND (tokens_known=1 OR (tokens_known IS NULL AND credits>0
	 AND input_tokens>=0 AND output_tokens>=0 AND total_tokens=input_tokens+output_tokens))
	 GROUP BY account_uid,model,ts>=?,json_extract(diagnostics,'$.billing_domain'),
	 json_extract(diagnostics,'$.billing_coefficient')`, now-7*86400, now-30*86400, now, now-7*86400)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []BudgetSample{}
	for rows.Next() {
		var s BudgetSample
		if err := rows.Scan(&s.AccountUID, &s.Model, &s.Recent, &s.Credits, &s.Tokens, &s.Calls, &s.ZeroTokens, &s.ZeroCalls, &s.UnknownCalls, &s.LegacyCalls, &s.UnknownTokens, &s.Domain, &s.Coefficient); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetSettings returns settings merged over defaults. Results are memoized in
// memory (invalidated by SaveSettings) since this is read on the per-request
// hot path. The returned map is a fresh copy the caller may mutate freely.
func (d *DB) GetSettings() (map[string]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.settingsCache == nil {
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
		d.settingsCache = merged
	}
	out := make(map[string]string, len(d.settingsCache))
	for k, v := range d.settingsCache {
		out[k] = v
	}
	return out, nil
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
	d.settingsCache = nil // invalidate memoized GetSettings
	return nil
}

// UsageParams are the fields for a usage log entry.
type UsageParams struct {
	Timestamp    float64
	Diagnostics  *UsageDiagnostics
	TokensKnown  *bool
	Model        string
	Protocol     string
	AccountUID   string
	InputTokens  int
	OutputTokens int
	// CachedTokens is the upstream prompt-cache hit tokens; nil = unknown
	// (upstream didn't report), 0 = reported but no hit.
	CachedTokens        *int
	ReasoningTokens     *int
	CacheCreationTokens *int
	LatencyMs           float64
	Status              string
	Error               string
	InputContent        string
	OutputContent       string
	ReasoningContent    string
	Credits             *float64
	AppName             string
	ReasoningEffort     string
	// UserID attributes the row to a portal user (0 = private/admin key).
	UserID int64
	AppID  int64
}

// UsageDiagnostics contains protocol facts only, never prompts or credentials.
type UsageDiagnostics struct {
	MediaTaskID        string             `json:"media_task_id,omitempty"`
	Media              map[string]any     `json:"media,omitempty"`
	ImagesRemoved      int                `json:"images_removed,omitempty"`
	ImagesRepaired     int                `json:"images_repaired,omitempty"`
	ImageRecovery      string             `json:"image_recovery,omitempty"`
	BillingDomain      string             `json:"billing_domain,omitempty"`
	BillingCoefficient *float64           `json:"billing_coefficient,omitempty"`
	ContentCleared     bool               `json:"content_cleared,omitempty"`
	TokenDetails       *UsageTokenDetails `json:"token_details,omitempty"`
	RequestedModel     string             `json:"requested_model,omitempty"`
	PhysicalModel      string             `json:"physical_model,omitempty"`
	FinishReason       string             `json:"finish_reason,omitempty"`
	RequestedLimits    map[string]int     `json:"requested_output_limits,omitempty"`
	EffectiveLimits    map[string]int     `json:"effective_output_limits,omitempty"`
	Compatibility      string             `json:"compatibility,omitempty"`
	ErrorKind          string             `json:"error_kind,omitempty"`
	Started            bool               `json:"upstream_started"`
	Performance        *UsagePerformance  `json:"performance,omitempty"`
}

// UsageTokenDetails stores observed counts, never estimates. Nil means unreported.
type UsageTokenDetails struct {
	Reasoning     *int `json:"reasoning_tokens,omitempty"`
	CacheCreation *int `json:"cache_creation_tokens,omitempty"`
}

// LogUsage inserts one usage record.
func (d *DB) LogUsage(p UsageParams) error {
	_, err := d.LogUsageWithID(p)
	return err
}

func (d *DB) LogUsageWithID(p UsageParams) (id int64, resultErr error) {
	started := time.Now()
	defer func() {
		d.usageRuntime.writes.Add(1)
		d.usageRuntime.writeNS.Add(uint64(time.Since(started)))
		if resultErr != nil {
			d.usageRuntime.writeErrors.Add(1)
		}
	}()
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
	var cached any
	if p.CachedTokens != nil {
		cached = *p.CachedTokens
	}
	var userID any
	var diagnostics any
	if p.ReasoningTokens != nil || p.CacheCreationTokens != nil {
		diag := UsageDiagnostics{}
		if p.Diagnostics != nil {
			diag = *p.Diagnostics
		}
		details := UsageTokenDetails{}
		if p.ReasoningTokens != nil && *p.ReasoningTokens >= 0 && *p.ReasoningTokens <= p.OutputTokens {
			n := *p.ReasoningTokens
			details.Reasoning = &n
		}
		if p.CacheCreationTokens != nil && *p.CacheCreationTokens >= 0 && *p.CacheCreationTokens <= p.InputTokens {
			n := *p.CacheCreationTokens
			details.CacheCreation = &n
		}
		if details.Reasoning != nil || details.CacheCreation != nil {
			diag.TokenDetails = &details
			p.Diagnostics = &diag
		}
	}
	if p.Diagnostics != nil {
		raw, err := json.Marshal(p.Diagnostics)
		if err != nil {
			return 0, err
		}
		diagnostics = string(raw)
	}
	if p.UserID != 0 {
		userID = p.UserID
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	timestamp := p.Timestamp
	if timestamp <= 0 {
		timestamp = float64(time.Now().UnixNano()) / 1e9
	}
	result, err := d.db.Exec(
		`INSERT INTO usage_logs (ts, model, protocol, account_uid, input_tokens, output_tokens,
		   total_tokens, latency_ms, status, error, input_content, output_content,
		   reasoning_content, credits, app_name, user_id, reasoning_effort, tokens_known, cached_tokens, app_id, diagnostics, content_bytes)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		timestamp, p.Model, p.Protocol, p.AccountUID,
		p.InputTokens, p.OutputTokens, total, p.LatencyMs, p.Status, p.Error,
		p.InputContent, p.OutputContent, p.ReasoningContent, credits, p.AppName, userID, effort, p.TokensKnown, cached, nullableID(p.AppID), diagnostics, len(p.InputContent)+len(p.OutputContent)+len(p.ReasoningContent))
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
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

// UsageSummary returns full historical stats with a 10-second shared snapshot.
// Fresh today counters use UsageToday; continuous writes do not bust this cache.
func (d *DB) UsageSummary() (map[string]any, error) {
	return d.summaryCache.get(10*time.Second, float64(localMidnight()), d.usageSummary, cloneAnalyticsMap)
}

func (d *DB) usageSummary() (map[string]any, error) {
	db, release := d.analyticsReader()
	defer release()
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out := map[string]any{}
	var tc, tt int64
	if err := tx.QueryRow("SELECT COUNT(*), COALESCE(SUM(total_tokens),0) FROM usage_logs").Scan(&tc, &tt); err != nil {
		return nil, err
	}
	out["total_requests"], out["total_tokens"] = tc, tt
	todayStart := localMidnight()
	var dc, dt int64
	if err := tx.QueryRow("SELECT COUNT(*), COALESCE(SUM(total_tokens),0) FROM usage_logs WHERE ts >= ?", todayStart).Scan(&dc, &dt); err != nil {
		return nil, err
	}
	out["today_requests"], out["today_tokens"] = dc, dt

	byProtocol, err := groupCount(tx, "SELECT protocol, COUNT(*), COALESCE(SUM(total_tokens),0) FROM usage_logs GROUP BY protocol", "protocol")
	if err != nil {
		return nil, err
	}
	out["by_protocol"] = byProtocol
	byModel, err := groupCount(tx, "SELECT model, COUNT(*), COALESCE(SUM(total_tokens),0) FROM usage_logs GROUP BY model ORDER BY 2 DESC LIMIT 20", "model")
	if err != nil {
		return nil, err
	}
	out["by_model"] = byModel
	byApp, err := groupCount(tx, "SELECT COALESCE(NULLIF(app_name,''),'(未命名)'), COUNT(*), COALESCE(SUM(total_tokens),0) FROM usage_logs GROUP BY app_name ORDER BY 2 DESC", "app")
	if err != nil {
		return nil, err
	}
	out["by_app"] = byApp

	out["by_account"] = []map[string]any{}
	rows, err := tx.Query(`SELECT account_uid, COUNT(*), COALESCE(SUM(total_tokens),0),
		COALESCE(SUM(credits),0), SUM(CASE WHEN status='ok' THEN 1 ELSE 0 END),
		SUM(CASE WHEN status IN ('ok','incomplete') AND credits IS NULL THEN 1 ELSE 0 END)
		FROM usage_logs GROUP BY account_uid ORDER BY 2 DESC`)
	if err != nil {
		return nil, err
	}
	{
		defer rows.Close()
		byAccount := []map[string]any{}
		for rows.Next() {
			var uid sql.NullString
			var c, t, okc, unk int64
			var cr float64
			if err := rows.Scan(&uid, &c, &t, &cr, &okc, &unk); err != nil {
				return nil, err
			}
			byAccount = append(byAccount, map[string]any{
				"account_uid": uid.String, "count": c, "tokens": t,
				"credits": round2(cr), "ok_count": okc, "unknown_count": unk,
			})
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		rows.Close()
		out["by_account"] = byAccount
	}

	// 缓存命中聚合：只统计上游确实上报过的行（cached_tokens IS NOT NULL），
	// cached 输入 + 未命中输入 = 可命中输入总量，命中率 = cached / (cached + miss)。
	var knownRows, cachedSum, missSum int64
	if err := tx.QueryRow(`SELECT COUNT(*),
		COALESCE(SUM(cached_tokens),0),
		COALESCE(SUM(CASE WHEN cached_tokens IS NOT NULL THEN input_tokens - cached_tokens ELSE 0 END),0)
		FROM usage_logs WHERE cached_tokens IS NOT NULL AND cached_tokens >= 0 AND input_tokens >= cached_tokens AND status IN ('ok','incomplete')`).Scan(&knownRows, &cachedSum, &missSum); err != nil {
		return nil, err
	}
	out["cache"] = map[string]any{
		"known_rows": knownRows, "cached_tokens": cachedSum, "uncached_tokens": missSum,
		"hit_rate": func() any {
			if knownRows == 0 {
				return nil
			}
			total := cachedSum + missSum
			if total <= 0 {
				return nil
			}
			return float64(cachedSum) / float64(total)
		}(),
	}
	return out, nil
}

func groupCount(db interface {
	Query(string, ...any) (*sql.Rows, error)
}, query, keyName string) ([]map[string]any, error) {
	rows, err := db.Query(query)
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
	return out, rows.Err()
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

const usageLightCols = "id, ts, model, protocol, account_uid, input_tokens, output_tokens, total_tokens, latency_ms, status, error, credits, app_name, user_id, reasoning_effort, tokens_known, cached_tokens, app_id, diagnostics"

// UsageRecent returns recent usage records with optional filters.
func (d *DB) UsageRecent(limit int, protocol, model string, appName *string, status string, light bool, offset int, search string) ([]map[string]any, error) {
	where, params := usageWhere(protocol, model, appName, status, search)
	params = append(params, limit, offset)
	cols := "*"
	if light {
		cols = usageLightCols
	}
	db, release := d.analyticsReader()
	defer release()
	rows, err := db.Query("SELECT "+cols+" FROM usage_logs "+where+" ORDER BY id DESC LIMIT ? OFFSET ?", params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := scanRows(rows)
	if err != nil {
		return nil, err
	}
	for _, r := range list {
		decodeUsageDiagnostics(r)
		_, known := r["credits"]
		r["credit_known"] = known && r["credits"] != nil
	}
	return list, nil
}

// UsageCount returns the count matching filters.
func (d *DB) UsageCount(protocol, model string, appName *string, status, search string) (int, error) {
	where, params := usageWhere(protocol, model, appName, status, search)
	db, release := d.analyticsReader()
	defer release()
	var c int
	err := db.QueryRow("SELECT COUNT(*) FROM usage_logs "+where, params...).Scan(&c)
	return c, err
}

// GetUsage returns one full usage record by id.
func (d *DB) GetUsage(id int) (map[string]any, error) {
	db, release := d.analyticsReader()
	defer release()
	rows, err := db.Query("SELECT * FROM usage_logs WHERE id = ?", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := scanRows(rows)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	r := list[0]
	decodeUsageDiagnostics(r)
	_, known := r["credits"]
	r["credit_known"] = known && r["credits"] != nil
	return r, nil
}

func decodeUsageDiagnostics(row map[string]any) {
	if raw, ok := row["diagnostics"].(string); ok {
		var value UsageDiagnostics
		if len(raw) <= 16384 && json.Unmarshal([]byte(raw), &value) == nil {
			row["diagnostics"] = value
		} else {
			row["diagnostics"] = nil
		}
	}
}

// UsageFilters returns distinct filter values.
func (d *DB) UsageFilters() (map[string]any, error) {
	historical, err := d.filterCache.get(30*time.Second, 0, d.usageFilterValues, cloneAnalyticsMap)
	if err != nil {
		return nil, err
	}
	db, release := d.analyticsReader()
	defer release()
	currentApps := []string{}
	rows, err := db.Query("SELECT name FROM apps ORDER BY id")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return nil, err
		}
		currentApps = append(currentApps, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	usedApps := historical["apps_history"].([]string)
	currentSet := map[string]bool{}
	for _, name := range currentApps {
		currentSet[name] = true
	}
	history := []string{}
	for _, name := range usedApps {
		if !currentSet[name] {
			history = append(history, name)
		}
	}
	historical["apps"], historical["apps_history"] = currentApps, history
	return historical, nil
}

func (d *DB) usageFilterValues() (map[string]any, error) {
	db, release := d.analyticsReader()
	defer release()
	col := func(field string) ([]string, error) {
		rows, err := db.Query("SELECT DISTINCT " + field + " FROM usage_logs WHERE " + field + " IS NOT NULL AND " + field + " != '' ORDER BY " + field)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, rows.Err()
	}
	out := map[string]any{}
	for _, item := range []struct{ key, column string }{{"protocols", "protocol"}, {"models", "model"}, {"apps_history", "app_name"}, {"statuses", "status"}} {
		values, err := col(item.column)
		if err != nil {
			return nil, err
		}
		out[item.key] = values
	}
	var unnamed bool
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM usage_logs WHERE app_name IS NULL OR app_name='')").Scan(&unnamed); err != nil {
		return nil, err
	}
	out["has_unnamed"] = unnamed
	return out, nil
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
	db, release := d.analyticsReader()
	defer release()
	rows, err := db.Query(query, args...)
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

// MaintainUsage performs bounded transactions. A canceled run can resume safely;
// clearing diagnostic text never changes usage, billing or performance counters.
func (d *DB) MaintainUsage(ctx context.Context, days, maxRows, contentDays int, contentBudget int64) (resultErr error) {
	started := time.Now()
	d.usageRuntime.maintenanceActive.Store(true)
	defer func() {
		d.usageRuntime.maintenanceMS.Store(uint64(time.Since(started).Milliseconds()))
		d.usageRuntime.maintenanceRuns.Add(1)
		if resultErr != nil && ctx.Err() == nil {
			d.usageRuntime.maintenanceErrors.Add(1)
		}
		d.usageRuntime.maintenanceActive.Store(false)
	}()
	const batch = 64
	run := func(query string, args ...any) (int64, error) {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		d.mu.Lock()
		res, err := d.db.ExecContext(ctx, query, args...)
		d.mu.Unlock()
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}
	loop := func(query string, args ...any) error {
		for {
			n, err := run(query, args...)
			if err != nil {
				return err
			}
			if n < batch {
				return nil
			}
		}
	}
	now := float64(time.Now().UnixNano()) / 1e9
	if days > 0 {
		if err := loop("DELETE FROM usage_logs WHERE id IN (SELECT id FROM usage_logs WHERE ts < ? ORDER BY ts LIMIT 64)", now-float64(days)*86400); err != nil {
			return err
		}
	}
	if maxRows > 0 {
		// Pin this maintenance round's cutoff once. New requests are retained
		// and considered by the next round, rather than repeating a large
		// OFFSET scan for every 64 deleted rows.
		var cutoff sql.NullInt64
		d.mu.Lock()
		err := d.db.QueryRowContext(ctx, "SELECT id FROM usage_logs ORDER BY id DESC LIMIT 1 OFFSET ?", maxRows).Scan(&cutoff)
		d.mu.Unlock()
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if cutoff.Valid {
			if err := loop(`DELETE FROM usage_logs WHERE id IN (SELECT id FROM usage_logs WHERE id <= ? ORDER BY id LIMIT 64)`, cutoff.Int64); err != nil {
				return err
			}
		}
	}
	const hasContent = "(content_bytes>0 OR (content_bytes IS NULL AND (COALESCE(input_content,'')<>'' OR COALESCE(output_content,'')<>'' OR COALESCE(reasoning_content,'')<>'')))"
	const clear = `UPDATE usage_logs SET input_content='',output_content='',reasoning_content='',content_bytes=0,
 diagnostics=json_set(CASE WHEN json_valid(diagnostics) THEN CASE WHEN json_type(diagnostics)='object' THEN diagnostics ELSE '{}' END ELSE '{}' END,'$.content_cleared',json('true')) WHERE id IN `
	if contentDays > 0 {
		if err := loop(clear+"(SELECT id FROM usage_logs WHERE ts < ? AND "+hasContent+" ORDER BY ts LIMIT 64)", now-float64(contentDays)*86400); err != nil {
			return err
		}
	}
	if days > 0 {
		if err := loop("DELETE FROM media_tasks WHERE id IN (SELECT id FROM media_tasks WHERE updated_at < ? ORDER BY updated_at LIMIT 64)", now-float64(days)*86400); err != nil {
			return err
		}
	}
	if contentDays > 0 {
		if err := loop(`UPDATE media_tasks SET response=json_object('id',id,'model',model,'status',status,'result_cleared',json('true')) WHERE id IN (SELECT id FROM media_tasks WHERE updated_at < ? AND status IN ('completed','failed','cancelled') AND COALESCE(json_extract(response,'$.result_cleared'),0)=0 ORDER BY updated_at LIMIT 64)`, now-float64(contentDays)*86400); err != nil {
			return err
		}
	}
	if contentBudget <= 0 {
		return nil
	}
	// Backfill historical sizes once, in resumable short batches. Aggregation
	// then reads the covering numeric index, not every saved prompt body.
	const size = "COALESCE(length(CAST(input_content AS BLOB)),0)+COALESCE(length(CAST(output_content AS BLOB)),0)+COALESCE(length(CAST(reasoning_content AS BLOB)),0)"
	if err := loop("UPDATE usage_logs SET content_bytes=" + size + " WHERE id IN (SELECT id FROM usage_logs WHERE content_bytes IS NULL LIMIT 64)"); err != nil {
		return err
	}
	d.mu.Lock()
	var total int64
	err := d.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(content_bytes),0) FROM usage_logs").Scan(&total)
	d.mu.Unlock()
	if err != nil {
		return err
	}
	for total > contentBudget {
		if err := ctx.Err(); err != nil {
			return err
		}
		d.mu.Lock()
		rows, err := d.db.QueryContext(ctx, "SELECT id,content_bytes FROM usage_logs WHERE content_bytes>0 ORDER BY ts,id LIMIT 64")
		if err != nil {
			d.mu.Unlock()
			return err
		}
		ids := []int64{}
		var released int64
		for rows.Next() {
			var id, n int64
			if err = rows.Scan(&id, &n); err != nil {
				break
			}
			ids = append(ids, id)
			released += n
			if total-released <= contentBudget {
				break
			}
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		d.mu.Unlock()
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		marks := make([]string, len(ids))
		args := make([]any, len(ids))
		for i, id := range ids {
			marks[i] = "?"
			args[i] = id
		}
		if _, err := run(clear+"("+strings.Join(marks, ",")+")", args...); err != nil {
			return err
		}
		total -= released
	}
	return nil
}

// Content-free, fixed-cardinality counters; these are observation, not limits.
type usageRuntime struct {
	writes, writeNS, writeErrors                      atomic.Uint64
	maintenanceRuns, maintenanceMS, maintenanceErrors atomic.Uint64
	maintenanceActive                                 atomic.Bool
}

func (d *DB) UsageRuntimeStats() map[string]any {
	calls := d.usageRuntime.writes.Load()
	var average float64
	if calls > 0 {
		average = float64(d.usageRuntime.writeNS.Load()) / float64(calls) / 1e6
	}
	return map[string]any{"write_attempts": calls, "write_errors": d.usageRuntime.writeErrors.Load(), "write_average_ms": average,
		"maintenance_runs": d.usageRuntime.maintenanceRuns.Load(), "maintenance_last_ms": d.usageRuntime.maintenanceMS.Load(), "maintenance_errors": d.usageRuntime.maintenanceErrors.Load(), "maintenance_running": d.usageRuntime.maintenanceActive.Load()}
}
