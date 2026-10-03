// Package store is the SQLite persistence layer (accounts, usage logs,
// settings, application keys, model cooldowns). Ported from workbuddy_one/db.py
// for the single-user, all-in-one deployment (multi-user portal deferred).
//
// The Go store creates the current schema directly and stamps user_version.
// It does not run the Python app's incremental migration chain; pointing it at
// an existing Python-created DB works because the column layout matches.
package store

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"work2api/internal/workbuddy/siterouting"
)

// SchemaVersion is the Go store's schema version. v12 matches the Python
// app's final schema; v13 adds the Go-side apps.allowed_models column
// (per-key model allowlist, JSON array, empty = unrestricted); v14 adds
// usage_logs.cached_tokens (upstream prompt-cache hit tokens, NULL = unknown);
// v15 adds the user-portal tables (users, user_sessions, invite_codes,
// contributions, resource_groups, group_accounts, user_group_grants) and
// rebuilds apps to drop the global-unique name constraint (per-user naming).
// v16 adds stable usage app IDs and independent daily execution quotas.
const SchemaVersion = 17

// DB wraps the SQLite connection.
type DB struct {
	db *sql.DB
	mu sync.Mutex

	// settingsCache memoizes the merged settings map. GetSettings is on the
	// per-request hot path (model alias resolve + cost-aware routing both read
	// it), so serving from memory avoids a SQLite round-trip per request. It is
	// invalidated on SaveSettings; nil means "not populated yet".
	settingsCache map[string]string
}

// DefaultSettings mirror db.py DEFAULT_SETTINGS.
var DefaultSettings = map[string]string{
	"checkin_hours":           "9,21",
	"credit_refresh_min":      "30",
	"model_refresh_hour":      "6",
	"model_ttl_min":           "60",
	"aa_refresh_hour":         "7",
	"keepalive_hour":          "22",
	"keepalive_enabled":       "1",
	"cost_aware_routing":      "1",
	"expiry_priority_days":    "7",
	"aa_api_key":              "",
	"model_aliases":           "",
	"alert_enabled":           "0",
	"alert_webhook":           "",
	"alert_threshold_percent": "20",
	"alert_expiry_days":       "7",
	"qoder_machine_salt":      "",
	// 用户门户（阶段 A）：管理员可配置的共享模型禁用列表（JSON 数组），从共享
	// 权限集中扣除。注册方式等部署级开关走环境变量，不进 settings。
	"portal_disabled_models":    "",
	"portal_public_url":         "",
	"portal_default_group":      "0",
	"portal_default_auto_grant": "0",
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
			sqldb.Close()
			return nil, fmt.Errorf("pragma %q: %w", p, err)
		}
	}
	d := &DB{db: sqldb}
	if err := d.initSchema(); err != nil {
		sqldb.Close()
		return nil, err
	}
	return d, nil
}

// Close closes the underlying database.
func (d *DB) Close() error { return d.db.Close() }

func HiddenAccountKey(uid string) string {
	return fmt.Sprintf("hidden_workbuddy_%x", sha256.Sum256([]byte(uid)))
}

// SetAccountHidden persists an internal discovery marker, separate from the
// user-editable settings whitelist.
func (d *DB) SetAccountHidden(uid string, hidden bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	value := "0"
	if hidden {
		value = "1"
	}
	_, err := d.db.Exec("INSERT INTO settings (key,value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", HiddenAccountKey(uid), value)
	d.settingsCache = nil
	return err
}

func (d *DB) initSchema() error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
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
CREATE TABLE IF NOT EXISTS model_cooldowns (
    account_uid TEXT, model TEXT, cooldown_until REAL, reason TEXT,
    created_at REAL, PRIMARY KEY (account_uid, model)
);
CREATE TABLE IF NOT EXISTS checkin_history (
    account_uid TEXT, date TEXT, source TEXT DEFAULT 'auto',
    created_at REAL, PRIMARY KEY (account_uid, date)
);
CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage_logs(ts);
CREATE INDEX IF NOT EXISTS idx_usage_account ON usage_logs(account_uid);
CREATE INDEX IF NOT EXISTS idx_usage_model ON usage_logs(model);
CREATE INDEX IF NOT EXISTS idx_usage_protocol ON usage_logs(protocol);
`
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	// v14: 上游 prompt-cache 命中 tokens（上游未报/未知为 NULL，便于区分 0 命中与不可知）。
	if _, err := tx.Exec("ALTER TABLE usage_logs ADD COLUMN cached_tokens INTEGER"); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return fmt.Errorf("migrate usage_logs.cached_tokens: %w", err)
	}
	// v13-era: LogUsage writes tokens_known; older DBs lack the column.
	if _, err := tx.Exec("ALTER TABLE usage_logs ADD COLUMN tokens_known INTEGER"); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return fmt.Errorf("migrate usage_logs.tokens_known: %w", err)
	}
	// Legacy apps migration runs once and commits atomically with the rest
	// of the schema. The old table remains authoritative after an interrupted
	// pre-transaction migration that left a partial apps_v2 copy.
	var hasApps, hasCopy int
	if err := tx.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='apps'").Scan(&hasApps); err != nil {
		return err
	}
	if err := tx.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='apps_v2'").Scan(&hasCopy); err != nil {
		return err
	}
	const appColumns = `(
 id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, key_hash TEXT UNIQUE,
 key_prefix TEXT, note TEXT, enabled INTEGER DEFAULT 1, created_at REAL,
 key_enc TEXT, user_id INTEGER, allowed_models TEXT DEFAULT ''
 )`
	if hasApps == 0 {
		if hasCopy > 0 {
			if _, err := tx.Exec("ALTER TABLE apps_v2 RENAME TO apps"); err != nil {
				return err
			}
		} else if _, err := tx.Exec("CREATE TABLE apps " + appColumns); err != nil {
			return err
		}
	} else if version < 15 {
		if _, err := tx.Exec("ALTER TABLE apps ADD COLUMN allowed_models TEXT DEFAULT ''"); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return err
		}
		var sequence int64
		if err := tx.QueryRow("SELECT COALESCE(MAX(seq),0) FROM sqlite_sequence WHERE name='apps'").Scan(&sequence); err != nil {
			return err
		}
		if _, err := tx.Exec("DROP TABLE IF EXISTS apps_v2"); err != nil {
			return err
		}
		if _, err := tx.Exec("CREATE TABLE apps_v2 " + appColumns); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO apps_v2 (id,name,key_hash,key_prefix,note,enabled,created_at,key_enc,user_id,allowed_models)
   SELECT id,name,key_hash,key_prefix,note,enabled,created_at,key_enc,user_id,allowed_models FROM apps`); err != nil {
			return fmt.Errorf("migrate apps rows: %w", err)
		}
		if _, err := tx.Exec("DROP TABLE apps"); err != nil {
			return err
		}
		if _, err := tx.Exec("ALTER TABLE apps_v2 RENAME TO apps"); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE sqlite_sequence SET seq=MAX(seq,?) WHERE name='apps'", sequence); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO sqlite_sequence(name,seq) SELECT 'apps',? WHERE NOT EXISTS (SELECT 1 FROM sqlite_sequence WHERE name='apps')", sequence); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_apps_user_name ON apps(user_id,name)"); err != nil {
		return err
	}
	if _, err := tx.Exec("ALTER TABLE usage_logs ADD COLUMN app_id INTEGER"); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return err
	}
	if _, err := tx.Exec("CREATE INDEX IF NOT EXISTS idx_usage_app ON usage_logs(app_id)"); err != nil {
		return err
	}
	// Historical name-based attribution is safe only for a unique current owner.
	// Missing or ambiguous historical records stay unattributed.
	if version < 16 {
		if _, err := tx.Exec(`UPDATE usage_logs SET app_id=(SELECT a.id FROM apps a
  WHERE a.name=usage_logs.app_name AND COALESCE(a.user_id,0)=COALESCE(usage_logs.user_id,0)
  AND usage_logs.ts>=a.created_at)
  WHERE app_id IS NULL AND (SELECT COUNT(*) FROM apps a WHERE a.name=usage_logs.app_name
  AND COALESCE(a.user_id,0)=COALESCE(usage_logs.user_id,0) AND usage_logs.ts>=a.created_at)=1`); err != nil {
			return err
		}
	}
	// Portal tables (see store_portal.go for the status enums).
	const portalSchema = `
CREATE TABLE IF NOT EXISTS users (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    username TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role TEXT NOT NULL DEFAULT 'user',
    status TEXT NOT NULL DEFAULT 'active',
    created_at REAL, updated_at REAL
);
CREATE TABLE IF NOT EXISTS user_sessions (
    token_hash TEXT PRIMARY KEY,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    expires_at REAL NOT NULL,
    created_at REAL
);
CREATE TABLE IF NOT EXISTS invite_codes (
    code TEXT PRIMARY KEY,
    created_by TEXT DEFAULT '',
    used_by INTEGER,
    used_at REAL,
    expires_at REAL DEFAULT 0,
    created_at REAL
);
CREATE TABLE IF NOT EXISTS contributions (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    account_uid TEXT NOT NULL UNIQUE,
    provider TEXT NOT NULL DEFAULT 'workbuddy',
    site TEXT DEFAULT '',
    status TEXT NOT NULL DEFAULT 'verifying',
    created_at REAL,
    revoked_at REAL,
    revoked_reason TEXT DEFAULT ''
);
CREATE TABLE IF NOT EXISTS resource_groups (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE,
    provider TEXT NOT NULL DEFAULT 'workbuddy',
    allowed_models TEXT DEFAULT '',
    enabled INTEGER DEFAULT 0,
    created_at REAL
);
CREATE TABLE IF NOT EXISTS group_accounts (
    group_id INTEGER NOT NULL REFERENCES resource_groups(id) ON DELETE CASCADE,
    account_uid TEXT NOT NULL,
    PRIMARY KEY (group_id, account_uid)
);
CREATE TABLE IF NOT EXISTS platform_account_sharing (
    account_uid TEXT PRIMARY KEY REFERENCES accounts(uid) ON DELETE CASCADE,
    mode TEXT NOT NULL CHECK(mode IN ('private','shared','both'))
);
CREATE TABLE IF NOT EXISTS user_group_grants (
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    group_id INTEGER NOT NULL REFERENCES resource_groups(id) ON DELETE CASCADE,
    granted_at REAL,
    PRIMARY KEY (user_id, group_id)
);
CREATE TABLE IF NOT EXISTS user_group_denials (
    user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    group_id INTEGER NOT NULL REFERENCES resource_groups(id) ON DELETE CASCADE,
    PRIMARY KEY(user_id,group_id)
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON user_sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expiry ON user_sessions(expires_at);
CREATE TABLE IF NOT EXISTS user_daily_quota (
 user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 day REAL NOT NULL, requests INTEGER NOT NULL DEFAULT 0,
 input_tokens INTEGER NOT NULL DEFAULT 0, output_tokens INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(user_id,day)
);
CREATE INDEX IF NOT EXISTS idx_contrib_user ON contributions(user_id);
`
	if _, err := tx.Exec(portalSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		return err
	}
	return tx.Commit()
}

// hasTable checks sqlite_master for a table (portal migration branching).
func (d *DB) hasTable(name string) (bool, error) {
	var c int
	err := d.db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&c)
	return c > 0, err
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

// RecordCheckin appends one day to the check-in history (idempotent per uid+date)
// and updates the account's last_checkin_date. source: auto (scheduler) / manual.
func (d *DB) RecordCheckin(uid, date, source string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.db.Exec(
		"INSERT OR IGNORE INTO checkin_history (account_uid, date, source, created_at) VALUES (?,?,?,?)",
		uid, date, source, float64(time.Now().UnixNano())/1e9); err != nil {
		return err
	}
	_, err := d.db.Exec("UPDATE accounts SET last_checkin_date=?, updated_at=? WHERE uid=?",
		date, float64(time.Now().UnixNano())/1e9, uid)
	return err
}

// CheckinHistory returns the check-in history (YYYY-MM-DD list) for every
// account since the given date (inclusive), grouped by uid. Empty date = all.
func (d *DB) CheckinHistory(since string) (map[string][]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	q := "SELECT account_uid, date FROM checkin_history"
	args := []any{}
	if since != "" {
		q += " WHERE date >= ?"
		args = append(args, since)
	}
	q += " ORDER BY date"
	rows, err := d.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var uid, date string
		if err := rows.Scan(&uid, &date); err != nil {
			return nil, err
		}
		out[uid] = append(out[uid], date)
	}
	return out, rows.Err()
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
