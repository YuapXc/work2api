package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

// App is a returned application-key row with usage stats.
type App struct {
	ID           int64   `json:"id"`
	Name         string  `json:"name"`
	KeyPrefix    string  `json:"key_prefix"`
	Note         string  `json:"note"`
	Enabled      bool    `json:"enabled"`
	CreatedAt    float64 `json:"created_at"`
	Requests     int64   `json:"requests"`
	Tokens       int64   `json:"tokens"`
	Credits      float64 `json:"credits"`
	TokensKnown  bool    `json:"tokens_known"`
	CreditsKnown bool    `json:"credits_known"`
	// UserID 0 = private/admin key (pre-portal keys stay private — HANDOFF §4).
	UserID int64 `json:"user_id"`
	// AllowedModels is the per-key model allowlist (JSON array of model ids);
	// empty/nil means unrestricted. Parsed for display convenience.
	AllowedModels []string `json:"allowed_models"`
}

func (d *DB) HasEncryptedAppKeys() (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var count int
	err := d.db.QueryRow("SELECT COUNT(*) FROM apps WHERE key_enc IS NOT NULL AND key_enc != ''").Scan(&count)
	return count > 0, err
}

// ListApps returns applications with cumulative usage. No ciphertext is
// returned. (Single-user: no user join.)
func (d *DB) ListApps() ([]App, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query("SELECT id, name, key_prefix, note, enabled, created_at, allowed_models, COALESCE(user_id,0) FROM apps ORDER BY id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	apps := []App{}
	for rows.Next() {
		var a App
		var note, allowed sql.NullString
		var enabled int
		if err := rows.Scan(&a.ID, &a.Name, &a.KeyPrefix, &note, &enabled, &a.CreatedAt, &allowed, &a.UserID); err != nil {
			return nil, err
		}
		a.Note = note.String
		a.Enabled = enabled != 0
		a.AllowedModels = parseAllowedModels(allowed.String)
		apps = append(apps, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// per-app usage stats
	for i := range apps {
		var c, t int64
		var cr float64
		if err := d.db.QueryRow(
			"SELECT COUNT(l.id), COALESCE(SUM(l.total_tokens),0), COALESCE(SUM(l.credits),0), COUNT(CASE WHEN l.tokens_known=0 THEN 1 END)=0, COUNT(CASE WHEN l.credits IS NULL THEN 1 END)=0 FROM usage_logs l WHERE l.app_id = ? AND COALESCE(l.user_id,0) = ?",
			apps[i].ID, apps[i].UserID).Scan(&c, &t, &cr, &apps[i].TokensKnown, &apps[i].CreditsKnown); err != nil {
			return nil, err
		}
		apps[i].Requests, apps[i].Tokens, apps[i].Credits = c, t, cr
	}
	return apps, nil
}

// CreateApp inserts a new application key and returns its id. allowedJSON is
// the raw JSON array string of allowed model ids ("" = unrestricted). userID
// 0 = admin/private key (portal keys carry the owner's user id). Name
// uniqueness is per-user (UNIQUE(user_id, name)); a same-name key owned by
// another user is fine — HANDOFF §5.
func (d *DB) CreateApp(name, keyHash, keyPrefix, note, keyEnc, allowedJSON string, userID int64) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var uid any
	if userID != 0 {
		uid = userID
	}
	res, err := d.db.Exec(
		"INSERT INTO apps (name, key_hash, key_prefix, note, enabled, created_at, key_enc, user_id, allowed_models) VALUES (?,?,?,?,1,?,?,?,?)",
		name, keyHash, keyPrefix, note, float64(time.Now().UnixNano())/1e9, keyEnc, uid, allowedJSON)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, ErrConflict
		}
		return 0, err
	}
	return res.LastInsertId()
}

// FindAppByKey returns an enabled app matching key_hash, or nil.
func (d *DB) FindAppByKey(keyHash string) (map[string]any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query("SELECT * FROM apps WHERE key_hash = ? AND enabled = 1", keyHash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := scanRows(rows)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return list[0], nil
}

// AllowedModelsOf returns the parsed allowlist for the app id, or nil for
// unrestricted / unknown app. Used on the per-request auth path.
func (d *DB) AllowedModelsOf(appID int64) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var allowed sql.NullString
	err := d.db.QueryRow("SELECT allowed_models FROM apps WHERE id = ?", appID).Scan(&allowed)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return parseAllowedModels(allowed.String), nil
}

// SetAppModels replaces an app's model allowlist. An empty list clears the
// restriction (all models allowed).
func (d *DB) SetAppModels(appID int64, allowedJSON string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.db.Exec("UPDATE apps SET allowed_models = ? WHERE id = ?", allowedJSON, appID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("应用不存在：%d", appID)
	}
	return nil
}

// parseAllowedModels decodes the stored JSON array, treating garbage or an
// empty string as unrestricted (nil). Never errors: a malformed value must not
// break auth.
func parseAllowedModels(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	clean := make([]string, 0, len(out))
	for _, m := range out {
		if m = strings.TrimSpace(m); m != "" {
			clean = append(clean, m)
		}
	}
	if len(clean) == 0 {
		return nil
	}
	return clean
}

// GetAppKeyEnc returns the encrypted key token (may be empty for legacy apps).
func (d *DB) GetAppKeyHash(appID int64) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var hash string
	err := d.db.QueryRow("SELECT key_hash FROM apps WHERE id = ?", appID).Scan(&hash)
	return hash, err
}

func (d *DB) GetAppKeyEnc(appID int64) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var enc sql.NullString
	err := d.db.QueryRow("SELECT key_enc FROM apps WHERE id = ?", appID).Scan(&enc)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return enc.String, err
}

// ToggleApp flips an app's enabled flag and returns the new state.
func (d *DB) ToggleApp(appID int64) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var enabled int
	if err := d.db.QueryRow("SELECT enabled FROM apps WHERE id = ?", appID).Scan(&enabled); err != nil {
		return false, err
	}
	newVal := 0
	if enabled == 0 {
		newVal = 1
	}
	_, err := d.db.Exec("UPDATE apps SET enabled = ? WHERE id = ?", newVal, appID)
	return newVal != 0, err
}

// DeleteApp removes an app by id.
func (d *DB) DeleteApp(appID int64) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.db.Exec("DELETE FROM apps WHERE id = ?", appID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// --- portal-side per-user key operations (ownership is part of every query) ---

// UserApps lists one user's keys with usage stats.
func (d *DB) UserApps(userID int64) ([]App, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query("SELECT id, name, key_prefix, note, enabled, created_at, allowed_models, COALESCE(user_id,0) FROM apps WHERE user_id=? ORDER BY id ASC", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	apps := []App{}
	for rows.Next() {
		var a App
		var note, allowed sql.NullString
		var enabled int
		if err := rows.Scan(&a.ID, &a.Name, &a.KeyPrefix, &note, &enabled, &a.CreatedAt, &allowed, &a.UserID); err != nil {
			return nil, err
		}
		a.Note = note.String
		a.Enabled = enabled != 0
		a.AllowedModels = parseAllowedModels(allowed.String)
		apps = append(apps, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range apps {
		var c, t int64
		var cr float64
		if err := d.db.QueryRow(
			"SELECT COUNT(l.id), COALESCE(SUM(l.total_tokens),0), COALESCE(SUM(l.credits),0), COUNT(CASE WHEN l.tokens_known=0 THEN 1 END)=0, COUNT(CASE WHEN l.credits IS NULL THEN 1 END)=0 FROM usage_logs l WHERE l.user_id = ? AND l.app_id = ?",
			userID, apps[i].ID).Scan(&c, &t, &cr, &apps[i].TokensKnown, &apps[i].CreditsKnown); err != nil {
			return nil, err
		}
		apps[i].Requests, apps[i].Tokens, apps[i].Credits = c, t, cr
	}
	return apps, nil
}

// UserAppOwned returns the app row only when it belongs to userID; nil when
// missing OR foreign (the handler reports the same 404 for both — HANDOFF
// ownership matrix: no existence oracle for other users' keys).
func (d *DB) UserAppOwned(appID, userID int64) (map[string]any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query("SELECT * FROM apps WHERE id=? AND user_id=?", appID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := scanRows(rows)
	if err != nil || len(list) == 0 {
		return nil, err
	}
	return list[0], nil
}

// ToggleAppOwned flips a user's own key; owned=false means not found/foreign.
func (d *DB) ToggleAppOwned(appID, userID int64) (enabled, owned bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var cur int
	if err := d.db.QueryRow("SELECT enabled FROM apps WHERE id=? AND user_id=?", appID, userID).Scan(&cur); err != nil {
		if err == sql.ErrNoRows {
			return false, false, nil
		}
		return false, false, err
	}
	newVal := 0
	if cur == 0 {
		newVal = 1
	}
	if _, err := d.db.Exec("UPDATE apps SET enabled=? WHERE id=? AND user_id=?", newVal, appID, userID); err != nil {
		return false, false, err
	}
	return newVal != 0, true, nil
}

// DeleteAppOwned removes a user's own key; returns owned=false when missing/foreign.
func (d *DB) DeleteAppOwned(appID, userID int64) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.db.Exec("DELETE FROM apps WHERE id=? AND user_id=?", appID, userID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetAppModelsOwned rebinds a user's own key allowlist (users may only narrow
// their own keys; the caller validates models against eligibility).
func (d *DB) SetAppModelsOwned(appID, userID int64, allowedJSON string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.db.Exec("UPDATE apps SET allowed_models=? WHERE id=? AND user_id=?", allowedJSON, appID, userID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// CountUserApps returns how many keys a user owns (portal cap check).
func (d *DB) CountUserApps(userID int64) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var c int
	err := d.db.QueryRow("SELECT COUNT(*) FROM apps WHERE user_id=?", userID).Scan(&c)
	return c, err
}

// UserDailyUsage sums today's request count and tokens across all of a user's
// apps (multi-key merged budget — HANDOFF §8). dayStart is unix seconds of
// local midnight, passed in so cross-day handling stays with the caller.
func (d *DB) UserDailyUsage(userID int64, dayStart float64) (requests, inputTokens, outputTokens int64, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	err = d.db.QueryRow(
		"SELECT COUNT(*), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0) FROM usage_logs WHERE user_id=? AND ts>=?",
		userID, dayStart).Scan(&requests, &inputTokens, &outputTokens)
	return requests, inputTokens, outputTokens, err
}

// --- shared helpers ---

// scanRows scans all rows into []map[string]any with driver-native types,
// mirroring Python's sqlite3.Row → dict.
func scanRows(rows *sql.Rows) ([]map[string]any, error) {
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []map[string]any{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			v := vals[i]
			// normalize []byte (TEXT) to string
			if b, ok := v.([]byte); ok {
				m[c] = string(b)
			} else {
				m[c] = v
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case []byte:
		return string(x)
	case float64:
		if x == math.Trunc(x) {
			return i64(int64(x))
		}
	case int64:
		return i64(x)
	}
	return ""
}

func round2(f float64) float64 {
	return math.Round(f*100) / 100
}

func localMidnight() int64 {
	n := time.Now()
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, n.Location()).Unix()
}

func i64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// CreateAppForUserLimited checks the owner cap and creates the key under one
// database transaction. options are allowed-model JSON and display prefix.
func (d *DB) CreateAppForUserLimited(userID int64, name, keyHash, keyEnc string, max int, options ...string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRow("SELECT COUNT(*) FROM apps WHERE user_id=?", userID).Scan(&count); err != nil {
		return 0, err
	}
	if max > 0 && count >= max {
		return 0, ErrKeyLimit
	}
	allowed, prefix := "", ""
	if len(options) > 0 {
		allowed = options[0]
	}
	if len(options) > 1 {
		prefix = options[1]
	}
	res, err := tx.Exec("INSERT INTO apps(name,key_hash,key_prefix,note,enabled,created_at,key_enc,user_id,allowed_models) VALUES(?,?,?,'门户 Key',1,?,?,?,?)", name, keyHash, prefix, float64(time.Now().UnixNano())/1e9, keyEnc, userID, allowed)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, ErrConflict
		}
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// UserDailyUsageKnown reports whether every logged token value is known.
// Legacy NULL markers retain their old semantics; explicit false is unknown.
func (d *DB) UserDailyUsageKnown(userID int64, since float64) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var known bool
	err := d.db.QueryRow("SELECT NOT EXISTS(SELECT 1 FROM usage_logs WHERE user_id=? AND ts>=? AND tokens_known=0)", userID, since).Scan(&known)
	return known, err
}
