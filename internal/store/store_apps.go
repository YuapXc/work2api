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
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	KeyPrefix string  `json:"key_prefix"`
	Note      string  `json:"note"`
	Enabled   bool    `json:"enabled"`
	CreatedAt float64 `json:"created_at"`
	Requests  int64   `json:"requests"`
	Tokens    int64   `json:"tokens"`
	Credits   float64 `json:"credits"`
	// AllowedModels is the per-key model allowlist (JSON array of model ids);
	// empty/nil means unrestricted. Parsed for display convenience.
	AllowedModels []string `json:"allowed_models"`
}

// ListApps returns applications with cumulative usage. No ciphertext is
// returned. (Single-user: no user join.)
func (d *DB) ListApps() ([]App, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query("SELECT id, name, key_prefix, note, enabled, created_at, allowed_models FROM apps ORDER BY id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	apps := []App{}
	for rows.Next() {
		var a App
		var note, allowed sql.NullString
		var enabled int
		if err := rows.Scan(&a.ID, &a.Name, &a.KeyPrefix, &note, &enabled, &a.CreatedAt, &allowed); err != nil {
			return nil, err
		}
		a.Note = note.String
		a.Enabled = enabled != 0
		a.AllowedModels = parseAllowedModels(allowed.String)
		apps = append(apps, a)
	}
	// per-app usage stats
	for i := range apps {
		var c, t int64
		var cr float64
		_ = d.db.QueryRow(
			"SELECT COUNT(l.id), COALESCE(SUM(l.total_tokens),0), COALESCE(SUM(l.credits),0) FROM usage_logs l WHERE l.app_name = ? AND l.ts >= ?",
			apps[i].Name, apps[i].CreatedAt).Scan(&c, &t, &cr)
		apps[i].Requests, apps[i].Tokens, apps[i].Credits = c, t, cr
	}
	return apps, nil
}

// CreateApp inserts a new application key and returns its id. allowedJSON is
// the raw JSON array string of allowed model ids ("" = unrestricted).
func (d *DB) CreateApp(name, keyHash, keyPrefix, note, keyEnc, allowedJSON string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.db.Exec(
		"INSERT INTO apps (name, key_hash, key_prefix, note, enabled, created_at, key_enc, user_id, allowed_models) VALUES (?,?,?,?,1,?,?,?,?)",
		name, keyHash, keyPrefix, note, float64(time.Now().UnixNano())/1e9, keyEnc, nil, allowedJSON)
	if err != nil {
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
