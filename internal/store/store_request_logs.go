package store

import (
	"context"
	"database/sql"
	"strings"
)

// Group only server-generated request IDs, scoped by user and key. Legacy rows
// without correlation remain separate; billing and quota rows are never changed.
const requestGroupExpression = `CASE
 WHEN json_valid(diagnostics) AND length(json_extract(CASE WHEN json_valid(diagnostics) THEN diagnostics ELSE '{}' END,'$.performance.request_id'))=32
 THEN 'request:' || json_extract(CASE WHEN json_valid(diagnostics) THEN diagnostics ELSE '{}' END,'$.performance.request_id') || ':' || COALESCE(user_id,0) || ':' || COALESCE(app_id,0)
 ELSE 'row:' || id END`

const requestLogCTE = `WITH scoped AS (SELECT id, ` + requestGroupExpression + ` AS request_group FROM usage_logs %s),
 grouped AS (SELECT MAX(id) AS final_id,COUNT(*) AS logged_attempts FROM scoped GROUP BY request_group),
 final AS (SELECT usage_logs.*,grouped.logged_attempts FROM usage_logs JOIN grouped ON usage_logs.id=grouped.final_id) `

func (d *DB) UsageRequests(userID *int64, limit, offset int, protocol, model string, appName *string, status, search string) ([]map[string]any, int, error) {
	return d.UsageRequestsContext(context.Background(), userID, limit, offset, protocol, model, appName, status, search)
}

func (d *DB) UsageRequestsContext(ctx context.Context, userID *int64, limit, offset int, protocol, model string, appName *string, status, search string) ([]map[string]any, int, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	cte, where, args := usageRequestQuery(userID, protocol, model, appName, status, search)
	db, release := d.analyticsReader()
	defer release()
	params := append(append([]any{}, args...), limit, offset)
	rows, err := db.QueryContext(ctx, cte+"SELECT "+usageLightCols+",logged_attempts,COUNT(*) OVER() AS filtered_total FROM final "+where+" ORDER BY id DESC LIMIT ? OFFSET ?", params...)
	if err != nil {
		return nil, 0, err
	}
	result, err := scanRows(rows)
	rows.Close()
	var total int
	// Empty pages beyond the end still report their true filtered total.
	if err == nil && len(result) == 0 && offset > 0 {
		err = db.QueryRowContext(ctx, cte+"SELECT COUNT(*) FROM final "+where, args...).Scan(&total)
	}
	for _, row := range result {
		if count, ok := row["filtered_total"].(int64); ok {
			total = int(count)
		}
		delete(row, "filtered_total")
		decodeUsageDiagnostics(row)
		row["credit_known"] = row["credits"] != nil
	}
	return result, total, err
}

func usageRequestQuery(userID *int64, protocol, model string, appName *string, status, search string) (string, string, []any) {
	scope, args := "", []any{}
	if userID != nil {
		scope = "WHERE user_id=?"
		args = append(args, *userID)
	}
	cte := strings.Replace(requestLogCTE, "%s", scope, 1)
	where, filters := usageWhere(protocol, "", appName, status, search)
	if model != "" {
		condition := `(model=? OR json_extract(CASE WHEN json_valid(diagnostics) THEN diagnostics ELSE '{}' END,'$.requested_model')=? OR json_extract(CASE WHEN json_valid(diagnostics) THEN diagnostics ELSE '{}' END,'$.physical_model')=?)`
		if where == "" {
			where = "WHERE " + condition
		} else {
			where += " AND " + condition
		}
		filters = append(filters, model, model, model)
	}
	return cte, where, append(args, filters...)
}

// VisitUsageExport reads the grouped snapshot once, using a private reader for file databases.
func (d *DB) VisitUsageExport(ctx context.Context, path, protocol, model string, appName *string, status, search string, visit func(map[string]any) error) error {
	db, err := newReadOnlyAnalytics(path)
	if err != nil {
		return err
	}
	if db == nil {
		db = d.db
	} else {
		defer db.Close()
		if _, err := db.ExecContext(ctx, "PRAGMA temp_store=FILE"); err != nil {
			return err
		}
	}
	cte, where, args := usageRequestQuery(nil, protocol, model, appName, status, search)
	const columns = "ts,model,protocol,account_uid,input_tokens,output_tokens,total_tokens,latency_ms,status,credits,app_name,tokens_known,cached_tokens,diagnostics"
	rows, err := db.QueryContext(ctx, cte+"SELECT "+columns+" FROM final "+where+" ORDER BY id DESC", args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	if err := visitRows(rows, func(row map[string]any) error {
		decodeUsageDiagnostics(row)
		return visit(row)
	}); err != nil {
		return err
	}
	return rows.Err()
}

// Ownership is checked by SQL before any data or retry history is returned.
func (d *DB) UsageRequest(id int64, userID *int64) (map[string]any, error) {
	db, release := d.analyticsReader()
	defer release()
	scope, args := "id=?", []any{id}
	if userID != nil {
		scope += " AND user_id=?"
		args = append(args, *userID)
	}
	rows, err := db.Query("SELECT * FROM usage_logs WHERE "+scope, args...)
	if err != nil {
		return nil, err
	}
	result, err := scanRows(rows)
	rows.Close()
	if err != nil || len(result) == 0 {
		return nil, err
	}
	row := result[0]
	decodeUsageDiagnostics(row)
	row["credit_known"] = row["credits"] != nil
	diag, _ := row["diagnostics"].(UsageDiagnostics)
	if diag.Performance == nil || len(diag.Performance.RequestID) != 32 {
		return row, nil
	}
	var user, app sql.NullInt64
	// Preserve NULL equality for legacy/admin entries.
	if err = db.QueryRow("SELECT user_id,app_id FROM usage_logs WHERE id=?", id).Scan(&user, &app); err != nil {
		return nil, err
	}
	attempts, err := db.Query(`SELECT `+usageLightCols+` FROM usage_logs WHERE user_id IS ? AND app_id IS ?
 AND json_valid(diagnostics) AND json_extract(CASE WHEN json_valid(diagnostics) THEN diagnostics ELSE '{}' END,'$.performance.request_id')=? ORDER BY id ASC LIMIT 16`, user, app, diag.Performance.RequestID)
	if err != nil {
		return nil, err
	}
	defer attempts.Close()
	history, err := scanRows(attempts)
	for _, item := range history {
		decodeUsageDiagnostics(item)
		item["credit_known"] = item["credits"] != nil
	}
	row["attempt_records"] = history
	return row, err
}
