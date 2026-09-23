// Package store is the SQLite persistence layer (accounts, usage logs,
// settings, application keys, model cooldowns). Ported from workbuddy_one/db.py
// for the single-user, all-in-one deployment (multi-user portal deferred).
//
// The Go store creates the current schema directly and stamps user_version.
// It does not run the Python app's incremental migration chain; pointing it at
// an existing Python-created DB works because the column layout matches.
package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"work2api/internal/workbuddy/siterouting"
)

// SchemaVersion matches the Python app's final schema (v12).
const SchemaVersion = 12

// DB wraps the SQLite connection.
type DB struct {
	db *sql.DB
	mu sync.Mutex
}

// DefaultSettings mirror db.py DEFAULT_SETTINGS.
var DefaultSettings = map[string]string{
	"checkin_hours":           "9,21",
	"credit_refresh_min":      "30",
	"model_refresh_hour":      "6",
	"model_ttl_min":           "60",
	"aa_refresh_hour":         "7",
	"keepalive_hour":          "22",
	"aa_api_key":              "",
	"model_aliases":           "",
	"alert_enabled":           "0",
	"alert_webhook":           "",
	"alert_threshold_percent": "20",
	"alert_expiry_days":       "7",
	"qoder_machine_salt":      "",
}

// New opens (creating if needed) the SQLite database at path.
func New(path string) (*DB, error) {
	// MaxOpenConns(1): serialize like the Python single-connection + lock model,
	// and guarantee the PRAGMA ordering below (auto_vacuum must precede WAL and
	// table creation, or it is silently ignored).
	sqldb, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	sqldb.SetMaxOpenConns(1)
	for _, p := range []string{
		"PRAGMA auto_vacuum=FULL",
		"PRAGMA journal_mode=WAL",
		"PRAGMA journal_size_limit=67108864",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := sqldb.Exec(p); err != nil {
			return nil, fmt.Errorf("pragma %q: %w", p, err)
		}
	}
	d := &DB{db: sqldb}
	if err := d.initSchema(); err != nil {
		return nil, err
	}
	return d, nil
}

// Close closes the underlying database.
func (d *DB) Close() error { return d.db.Close() }

func (d *DB) initSchema() error {
	const schema = `
CREATE TABLE IF NOT EXISTS accounts (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    uid TEXT UNIQUE, nickname TEXT, enterprise_id TEXT, domain TEXT,
    auth_json TEXT, enabled INTEGER DEFAULT 1, priority INTEGER DEFAULT 0,
    credits_remaining REAL, credits_total REAL, credits_expire_at TEXT,
    last_used_at REAL, last_checkin_date TEXT, profile TEXT,
    provider TEXT DEFAULT 'workbuddy', disabled_reason TEXT DEFAULT '',
    alias TEXT DEFAULT '', failure_count INTEGER DEFAULT 0,
    cooldown_until REAL DEFAULT 0, created_at REAL, updated_at REAL
);
CREATE TABLE IF NOT EXISTS usage_logs (
    id INTEGER PRIMARY KEY AUTOINCREMENT, ts REAL, model TEXT, protocol TEXT,
    account_uid TEXT, input_tokens INTEGER, output_tokens INTEGER,
    total_tokens INTEGER, latency_ms REAL, status TEXT, error TEXT,
    input_content TEXT, output_content TEXT, reasoning_content TEXT,
    credits REAL, app_name TEXT, user_id INTEGER, reasoning_effort TEXT
);
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE IF NOT EXISTS apps (
    id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT UNIQUE, key_hash TEXT UNIQUE,
    key_prefix TEXT, note TEXT, enabled INTEGER DEFAULT 1, created_at REAL,
    key_enc TEXT, user_id INTEGER
);
CREATE TABLE IF NOT EXISTS model_cooldowns (
    account_uid TEXT, model TEXT, cooldown_until REAL, reason TEXT,
    created_at REAL, PRIMARY KEY (account_uid, model)
);
CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage_logs(ts);
CREATE INDEX IF NOT EXISTS idx_usage_account ON usage_logs(account_uid);
CREATE INDEX IF NOT EXISTS idx_usage_model ON usage_logs(model);
CREATE INDEX IF NOT EXISTS idx_usage_protocol ON usage_logs(protocol);
`
	if _, err := d.db.Exec(schema); err != nil {
		return err
	}
	if _, err := d.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		return err
	}
	return nil
}

// --- accounts ---

// UpsertAccount writes a workbuddy auth file's content. auth is the full
// {auth:{...}, account:{...}} blob.
func (d *DB) UpsertAccount(auth map[string]any) (string, error) {
	account, _ := auth["account"].(map[string]any)
	authData, _ := auth["auth"].(map[string]any)
	if authData == nil {
		authData = auth
	}
	uid := str(account["uid"])
	nickname := str(account["nickname"])
	ent := str(account["enterpriseId"])
	domain := str(authData["domain"])
	profile, err := siterouting.ProfileForAuth(authData)
	if err != nil {
		profile = siterouting.DefaultProfile
	}
	blob, _ := json.Marshal(auth)
	now := float64(time.Now().UnixNano()) / 1e9
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err = d.db.Exec(
		`INSERT INTO accounts (uid, nickname, enterprise_id, domain, auth_json, profile, provider, created_at, updated_at)
		 VALUES (?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(uid) DO UPDATE SET nickname=excluded.nickname, enterprise_id=excluded.enterprise_id,
		   domain=excluded.domain, auth_json=excluded.auth_json, profile=excluded.profile,
		   provider=excluded.provider, updated_at=excluded.updated_at`,
		uid, nickname, ent, domain, string(blob), profile, "workbuddy", now, now)
	return uid, err
}

// ListAccounts returns all accounts ordered by priority desc, id asc.
func (d *DB) ListAccounts() ([]map[string]any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query("SELECT * FROM accounts ORDER BY priority DESC, id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRows(rows)
}

// GetAccount returns one account by uid, or nil.
func (d *DB) GetAccount(uid string) (map[string]any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query("SELECT * FROM accounts WHERE uid=?", uid)
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

// DeleteAccount removes an account and its model cooldowns.
func (d *DB) DeleteAccount(uid string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.db.Exec("DELETE FROM accounts WHERE uid=?", uid)
	if err != nil {
		return false, err
	}
	_, _ = d.db.Exec("DELETE FROM model_cooldowns WHERE account_uid=?", uid)
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetAccountState updates whitelisted account fields.
func (d *DB) SetAccountState(uid string, fields map[string]any) error {
	allowed := map[string]struct{}{
		"enabled": {}, "priority": {}, "credits_remaining": {}, "credits_total": {},
		"credits_expire_at": {}, "last_used_at": {}, "failure_count": {}, "cooldown_until": {},
		"disabled_reason": {}, "alias": {}, "profile": {}, "provider": {},
	}
	var sets []string
	var vals []any
	for k, v := range fields {
		if _, ok := allowed[k]; ok {
			sets = append(sets, k+"=?")
			vals = append(vals, v)
		}
	}
	if len(sets) == 0 {
		return nil
	}
	vals = append(vals, uid)
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec("UPDATE accounts SET "+strings.Join(sets, ", ")+" WHERE uid=?", vals...)
	return err
}

// SetCheckinDate records an account's last check-in date (YYYY-MM-DD).
func (d *DB) SetCheckinDate(uid, date string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec("UPDATE accounts SET last_checkin_date=?, updated_at=? WHERE uid=?",
		date, float64(time.Now().UnixNano())/1e9, uid)
	return err
}

// CheckinDates returns {uid: last_checkin_date}.
func (d *DB) CheckinDates() (map[string]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query("SELECT uid, last_checkin_date FROM accounts")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var uid, date sql.NullString
		if err := rows.Scan(&uid, &date); err != nil {
			return nil, err
		}
		if date.Valid && date.String != "" {
			out[uid.String] = date.String
		}
	}
	return out, nil
}

// SetModelCooldown records a per-account per-model cooldown deadline.
func (d *DB) SetModelCooldown(accountUID, model string, until float64, reason string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec(
		`INSERT INTO model_cooldowns (account_uid, model, cooldown_until, reason, created_at)
		 VALUES (?,?,?,?,?)
		 ON CONFLICT(account_uid, model) DO UPDATE SET cooldown_until=excluded.cooldown_until,
		   reason=excluded.reason, created_at=excluded.created_at`,
		accountUID, model, until, reason, float64(time.Now().UnixNano())/1e9)
	return err
}

// ActiveModelCooldowns purges expired rows and returns those still in effect.
func (d *DB) ActiveModelCooldowns(now float64) ([]map[string]any, error) {
	if now == 0 {
		now = float64(time.Now().UnixNano()) / 1e9
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, _ = d.db.Exec("DELETE FROM model_cooldowns WHERE cooldown_until <= ?", now)
	rows, err := d.db.Query("SELECT account_uid, model, cooldown_until, reason FROM model_cooldowns WHERE cooldown_until > ?", now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRows(rows)
}
