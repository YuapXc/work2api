package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Portal tables: users, sessions, invite codes, contributions, resource
// groups and grants. All methods are safe for concurrent use (the DB wrapper
// serializes writes with MaxOpenConns(1) + mu).
//
// Status enums are plain strings for readability in SQL and admin tooling:
//   - users.status: active / disabled
//   - contributions.status: verifying / active / private / unavailable / invalid / revoked
//   - invite_codes: single-use; used_at IS NULL means unconsumed.

var ErrConflict = errors.New("资源冲突")
var ErrKeyLimit = errors.New("Key 数量已达上限")
var ErrDailyQuota = errors.New("每日用量已达上限")
var ErrInvalidInvite = errors.New("邀请码无效、已使用或已过期")

const MaxUserSessions = 32

// --- users ---

type User struct {
	ID           int64   `json:"id"`
	Username     string  `json:"username"`
	Role         string  `json:"role"`
	Status       string  `json:"status"`
	CreatedAt    float64 `json:"created_at"`
	PasswordHash string  `json:"-"` // never serialized
}

// CreateUser inserts a user; username uniqueness is enforced by a UNIQUE index.
func (d *DB) CreateUser(username, passwordHash, role string) (int64, error) {
	now := float64(time.Now().UnixNano()) / 1e9
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.db.Exec(
		"INSERT INTO users (username, password_hash, role, status, created_at, updated_at) VALUES (?,?,?,?,?,?)",
		username, passwordHash, role, "active", now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, ErrConflict
		}
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) GetUserByUsername(username string) (*User, error) {
	return d.getUserBy("username", username)
}

func (d *DB) GetUser(id int64) (*User, error) {
	return d.getUserBy("id", id)
}

func (d *DB) getUserBy(col string, v any) (*User, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	row := d.db.QueryRow(
		"SELECT id, username, password_hash, role, status, created_at FROM users WHERE "+col+" = ?", v)
	var u User
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Status, &u.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &u, nil
}

// SetUserStatus flips active/disabled. Disabled users lose eligibility on the
// next eligibility check without needing to touch sessions here (the per-request
// user lookup is authoritative).
func (d *DB) SetUserStatus(id int64, status string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.db.Exec("UPDATE users SET status=?, updated_at=? WHERE id=?", status, float64(time.Now().UnixNano())/1e9, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("用户不存在：%d", id)
	}
	return nil
}

// UpdateUserPassword replaces the hash and revokes sessions atomically. An
// expected hash prevents self-service changes overwriting a concurrent reset.
func (d *DB) UpdateUserPassword(id int64, passwordHash string, expectedHash ...string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	query := "UPDATE users SET password_hash=?,updated_at=? WHERE id=?"
	args := []any{passwordHash, float64(time.Now().UnixNano()) / 1e9, id}
	if len(expectedHash) > 0 {
		query += " AND password_hash=? AND status='active'"
		args = append(args, expectedHash[0])
	}
	res, err := tx.Exec(query, args...)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		if len(expectedHash) > 0 {
			return ErrConflict
		}
		return fmt.Errorf("用户不存在：%d", id)
	}
	if _, err := tx.Exec("DELETE FROM user_sessions WHERE user_id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}

// HasAnyUser reports whether any user row exists (admin bootstrap guard).
func (d *DB) HasAnyUser() (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var c int
	err := d.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&c)
	return c > 0, err
}

// ListUsers lists all users (admin view; password hashes are not serialized).
func (d *DB) ListUsers() ([]User, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query("SELECT id, username, password_hash, role, status, created_at FROM users ORDER BY id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &u.Status, &u.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// DeleteUserHard removes a user row (invite-code compensation when the atomic
// consume races). Sessions/contributions cascade via FK.
func (d *DB) DeleteUserHard(id int64) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.db.Exec("DELETE FROM users WHERE id=?", id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// --- user sessions (persistent, revocable; distinct from in-memory admin
// sessions — see HANDOFF §4) ---

type UserSession struct {
	TokenHash string
	UserID    int64
	ExpiresAt float64
}

// CreateUserSession persists a session; expiresAt is unix seconds.
func (d *DB) CreateUserSession(tokenHash string, userID int64, expiresAt float64) error {
	return d.createUserSession(tokenHash, userID, expiresAt, "")
}

// CreateUserSessionForPassword refuses issuance when credentials or status
// changed after bcrypt verification, closing concurrent reset/disable races.
func (d *DB) CreateUserSessionForPassword(tokenHash string, userID int64, expiresAt float64, expectedHash string) error {
	return d.createUserSession(tokenHash, userID, expiresAt, expectedHash)
}
func (d *DB) createUserSession(tokenHash string, userID int64, expiresAt float64, expectedHash string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := float64(time.Now().UnixNano()) / 1e9
	if _, err := tx.Exec("DELETE FROM user_sessions WHERE expires_at<=?", now); err != nil {
		return err
	}
	res, err := tx.Exec(`INSERT INTO user_sessions(token_hash,user_id,expires_at,created_at)
 SELECT ?,id,?,? FROM users WHERE id=? AND status='active' AND (?='' OR password_hash=?)`, tokenHash, expiresAt, now, userID, expectedHash, expectedHash)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	if _, err := tx.Exec(`DELETE FROM user_sessions WHERE user_id=? AND token_hash IN
  (SELECT token_hash FROM user_sessions WHERE user_id=? ORDER BY created_at DESC, rowid DESC LIMIT -1 OFFSET ?)`, userID, userID, MaxUserSessions); err != nil {
		return err
	}
	return tx.Commit()
}

// GetUserSession returns the session row if live (sliding expiry NOT applied
// here; the caller decides whether to extend).
func (d *DB) GetUserSession(tokenHash string) (*UserSession, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	row := d.db.QueryRow("SELECT token_hash, user_id, expires_at FROM user_sessions WHERE token_hash = ?", tokenHash)
	var s UserSession
	if err := row.Scan(&s.TokenHash, &s.UserID, &s.ExpiresAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	if s.ExpiresAt <= float64(time.Now().Unix()) {
		_, _ = d.db.Exec("DELETE FROM user_sessions WHERE token_hash = ?", tokenHash)
		return nil, nil
	}
	return &s, nil
}

// ExtendUserSession slides the expiry forward.
func (d *DB) ExtendUserSession(tokenHash string, expiresAt float64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec("UPDATE user_sessions SET expires_at=? WHERE token_hash=?", expiresAt, tokenHash)
	return err
}

// DeleteUserSession revokes one session (logout).
func (d *DB) DeleteUserSession(tokenHash string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec("DELETE FROM user_sessions WHERE token_hash=?", tokenHash)
	return err
}

// DeleteUserSessions revokes every session of a user (password change /
// disable / reset). Returns how many were removed.
func (d *DB) DeleteUserSessions(userID int64) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.db.Exec("DELETE FROM user_sessions WHERE user_id=?", userID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// --- invite codes ---

type InviteCode struct {
	Code      string   `json:"code"`
	UsedBy    *int64   `json:"used_by"`
	UsedAt    *float64 `json:"used_at"`
	ExpiresAt float64  `json:"expires_at"` // 0 = never expires
	CreatedAt float64  `json:"created_at"`
}

func (d *DB) CreateInviteCode(code string, expiresAt float64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec("INSERT INTO invite_codes (code, expires_at, created_at) VALUES (?,?,?)",
		code, expiresAt, float64(time.Now().UnixNano())/1e9)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrConflict
	}
	return err
}

// ConsumeInviteCode atomically marks an unconsumed, unexpired code as used by
// userID. Returns false when the code is unknown/used/expired. The UPDATE's
// WHERE clause makes concurrent registrations consume each code exactly once.
func (d *DB) ConsumeInviteCode(code string, userID int64) (bool, error) {
	now := float64(time.Now().UnixNano()) / 1e9
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.db.Exec(
		"UPDATE invite_codes SET used_by=?, used_at=? WHERE code=? AND used_by IS NULL AND (expires_at=0 OR expires_at>?)",
		userID, now, code, now)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ListInviteCodes returns codes newest first (admin view).
func (d *DB) ListInviteCodes() ([]map[string]any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query("SELECT code, used_by, used_at, expires_at, created_at FROM invite_codes ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRows(rows)
}

// --- contributions ---

// Contribution links a user to one upstream account they contributed. The
// account_uid is the provider's stable identity (workbuddy UID here); UNIQUE
// across the whole table enforces "one account may only ever belong to one
// contributor", including admin-owned accounts registered before the portal
// (those simply have no row and cannot gain one while present in the pool).
type Contribution struct {
	ID            int64    `json:"id"`
	UserID        int64    `json:"user_id"`
	AccountUID    string   `json:"account_uid"`
	Provider      string   `json:"provider"`
	Site          string   `json:"site"`
	Status        string   `json:"status"`
	CreatedAt     float64  `json:"created_at"`
	RevokedAt     *float64 `json:"revoked_at"`
	RevokedReason string   `json:"revoked_reason"`
}

func (d *DB) CreateContribution(userID int64, accountUID, provider, site, status string) (int64, error) {
	now := float64(time.Now().UnixNano()) / 1e9
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.db.Exec(
		"INSERT INTO contributions (user_id, account_uid, provider, site, status, created_at) VALUES (?,?,?,?,?,?)",
		userID, accountUID, provider, site, status, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, ErrConflict
		}
		return 0, err
	}
	return res.LastInsertId()
}

// ContributionByAccount returns the (unique) contribution owning an account, or nil.
func (d *DB) ContributionByAccount(accountUID string) (*Contribution, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	row := d.db.QueryRow(
		"SELECT id, user_id, account_uid, provider, site, status, created_at, revoked_at, revoked_reason FROM contributions WHERE account_uid=?", accountUID)
	var c Contribution
	if err := row.Scan(&c.ID, &c.UserID, &c.AccountUID, &c.Provider, &c.Site, &c.Status, &c.CreatedAt, &c.RevokedAt, &c.RevokedReason); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}

// UserContributions lists one user's contributions, newest first. Account
// identities are masked by the handler before reaching the client.
func (d *DB) UserContributions(userID int64) ([]Contribution, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query(
		"SELECT id, user_id, account_uid, provider, site, status, created_at, revoked_at, revoked_reason FROM contributions WHERE user_id=? ORDER BY id DESC", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Contribution{}
	for rows.Next() {
		var c Contribution
		if err := rows.Scan(&c.ID, &c.UserID, &c.AccountUID, &c.Provider, &c.Site, &c.Status, &c.CreatedAt, &c.RevokedAt, &c.RevokedReason); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ActiveContributionUIDs returns the account UIDs a user currently holds an
// active contribution for. This is the eligibility check's source of truth.
func (d *DB) ActiveContributionUIDs(userID int64) (map[string]bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query("SELECT account_uid FROM contributions WHERE user_id=? AND status='active'", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		out[uid] = true
	}
	return out, rows.Err()
}

// HasActiveContribution reports whether any active contribution grants this
// user portal model access (used to reject when eligibility is empty).
func (d *DB) HasActiveContribution(userID int64) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var c int
	err := d.db.QueryRow("SELECT COUNT(*) FROM contributions WHERE user_id=? AND status='active'", userID).Scan(&c)
	return c > 0, err
}

// OwnedAccountUIDs includes verified accounts retained for personal use after
// withdrawal. Historical revoked/invalid/verifying accounts stay unavailable.
func (d *DB) OwnedAccountUIDs(userID int64) (map[string]bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query("SELECT account_uid FROM contributions WHERE user_id=? AND provider='workbuddy' AND status IN ('active','private')", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		out[uid] = true
	}
	return out, rows.Err()
}

// PrivatePoolExcludedUIDs prevents historical platform keys from selecting
// user-owned credentials or platform accounts configured for shared use only.
func (d *DB) PrivatePoolExcludedUIDs() (map[string]bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query("SELECT account_uid FROM contributions UNION SELECT account_uid FROM platform_account_sharing WHERE mode='shared'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		out[uid] = true
	}
	return out, rows.Err()
}

// WithdrawContribution atomically stops sharing while preserving ownership.
func (d *DB) WithdrawContribution(id, userID int64, reason string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE contributions SET status='private',revoked_at=?,revoked_reason=? WHERE id=? AND user_id=? AND status='active'", float64(time.Now().UnixNano())/1e9, reason, id, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if _, err := tx.Exec("DELETE FROM group_accounts WHERE account_uid=(SELECT account_uid FROM contributions WHERE id=?)", id); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// RestoreContributionSharing preserves verified ownership and account state.
// Only the owner may opt back in; invalid/historical revoked accounts need OAuth.
func (d *DB) RestoreContributionSharing(id, userID int64) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE contributions SET status='active',revoked_at=NULL,revoked_reason=''
 WHERE id=? AND user_id=? AND status='private' AND EXISTS(SELECT 1 FROM users WHERE id=? AND status='active')`, id, userID, userID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO group_accounts(group_id,account_uid) SELECT rg.id,c.account_uid FROM resource_groups rg JOIN contributions c ON c.id=? AND c.user_id=? WHERE rg.provider=c.provider AND rg.id=CAST((SELECT value FROM settings WHERE key='portal_default_group') AS INTEGER)`, id, userID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// SetContributionStatus transitions one of the user's own contributions.
// updateOnly keeps the transition explicit: callers pass the expected current
// status so a revoke racing a re-verify cannot clobber unexpectedly.
func (d *DB) SetContributionStatus(id int64, userID int64, from, to, reason string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	set := "status=?, revoked_reason=?"
	args := []any{to, reason}
	if to == "revoked" {
		set += ", revoked_at=?"
		args = append(args, float64(time.Now().UnixNano())/1e9)
	}
	q := "UPDATE contributions SET " + set + " WHERE id=? AND user_id=?"
	args = append(args, id, userID)
	if from != "" {
		q += " AND status=?"
		args = append(args, from)
	}
	res, err := d.db.Exec(q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ListContributions lists every contribution (admin overview).
func (d *DB) ListContributions() ([]Contribution, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query(
		"SELECT id, user_id, account_uid, provider, site, status, created_at, revoked_at, revoked_reason FROM contributions ORDER BY id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Contribution{}
	for rows.Next() {
		var c Contribution
		if err := rows.Scan(&c.ID, &c.UserID, &c.AccountUID, &c.Provider, &c.Site, &c.Status, &c.CreatedAt, &c.RevokedAt, &c.RevokedReason); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// --- resource groups (shared pool partitioning) ---

type ResourceGroup struct {
	ID            int64    `json:"id"`
	Name          string   `json:"name"`
	Provider      string   `json:"provider"`
	AllowedModels string   `json:"allowed_models"` // JSON array; "" = group must be configured before serving
	Enabled       bool     `json:"enabled"`
	CreatedAt     float64  `json:"created_at"`
	Accounts      []string `json:"account_uids"`
}

func (d *DB) CreateResourceGroup(name, provider, allowedModels string) (int64, error) {
	now := float64(time.Now().UnixNano()) / 1e9
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.db.Exec(
		"INSERT INTO resource_groups (name, provider, allowed_models, enabled, created_at) VALUES (?,?,?,0,?)",
		name, provider, allowedModels, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, ErrConflict
		}
		return 0, err
	}
	return res.LastInsertId()
}

func (d *DB) SetGroupModels(id int64, allowedModels string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.db.Exec("UPDATE resource_groups SET allowed_models=? WHERE id=?", allowedModels, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("分组不存在：%d", id)
	}
	return nil
}

func (d *DB) SetGroupEnabled(id int64, enabled bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	v := 0
	if enabled {
		v = 1
	}
	res, err := d.db.Exec("UPDATE resource_groups SET enabled=? WHERE id=?", v, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("分组不存在：%d", id)
	}
	return nil
}

func (d *DB) DeleteResourceGroup(id int64) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.Exec("DELETE FROM resource_groups WHERE id=?", id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	for _, table := range []string{"group_accounts", "user_group_grants", "user_group_denials"} {
		if _, err := tx.Exec("DELETE FROM "+table+" WHERE group_id=?", id); err != nil {
			return false, err
		}
	}
	var isDefault bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM settings WHERE key='portal_default_group' AND value=?)", strconv.FormatInt(id, 10)).Scan(&isDefault); err != nil {
		return false, err
	}
	if isDefault {
		if _, err := tx.Exec("UPDATE settings SET value='0' WHERE key IN ('portal_default_group','portal_default_auto_grant')"); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	d.settingsCache = nil
	return true, nil
}

// AddGroupAccount links an account into a group, idempotent.
func (d *DB) AddGroupAccount(groupID int64, accountUID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec("INSERT OR IGNORE INTO group_accounts (group_id, account_uid) VALUES (?,?)", groupID, accountUID)
	return err
}

// RemoveGroupAccount unlinks an account from a group.
func (d *DB) RemoveGroupAccount(groupID int64, accountUID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.db.Exec("DELETE FROM group_accounts WHERE group_id=? AND account_uid=?", groupID, accountUID)
	return err
}

// GrantGroup grants a user access to a group, idempotent.
func (d *DB) GrantGroup(userID, groupID int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec("INSERT OR IGNORE INTO user_group_grants (user_id, group_id, granted_at) VALUES (?,?,?)",
		userID, groupID, float64(time.Now().UnixNano())/1e9)
	if err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM user_group_denials WHERE user_id=? AND group_id=?", userID, groupID); err != nil {
		return err
	}
	return tx.Commit()
}

// RevokeGroup removes a user's group grant.
func (d *DB) RevokeGroup(userID, groupID int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM user_group_grants WHERE user_id=? AND group_id=?", userID, groupID); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT OR IGNORE INTO user_group_denials(user_id,group_id) VALUES(?,?)", userID, groupID); err != nil {
		return err
	}
	return tx.Commit()
}

// GroupAccountsOf returns the union of account UIDs across all enabled groups
// the user holds grants for. This is the shared-pool scheduling scope for one
// request: the caller intersects it with per-model account candidates.
func (d *DB) GroupAccountsOf(userID int64) (map[string]bool, error) {
	groups, err := d.GrantedGroups(userID)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, g := range groups {
		if !g.Enabled {
			continue
		}
		uids, err := d.GroupAccountUIDsWithActiveContribution(g.ID)
		if err != nil {
			return nil, err
		}
		for _, uid := range uids {
			out[uid] = true
		}
	}
	return out, nil
}

// GrantedGroups lists the groups a user has grants for (enabled and disabled
// both, so the portal can show what exists; scheduling only uses enabled ones
// via GroupAccountsOf).
func (d *DB) GrantedGroups(userID int64) ([]ResourceGroup, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query(
		`SELECT rg.id,rg.name,rg.provider,rg.allowed_models,rg.enabled,rg.created_at FROM resource_groups rg
 WHERE (EXISTS(SELECT 1 FROM user_group_grants g WHERE g.group_id=rg.id AND g.user_id=?)
 OR (rg.provider='workbuddy' AND rg.id=CAST((SELECT value FROM settings WHERE key='portal_default_group') AS INTEGER)
 AND (SELECT value FROM settings WHERE key='portal_default_auto_grant')='1'
 AND EXISTS(SELECT 1 FROM contributions WHERE user_id=? AND provider='workbuddy' AND status='active')))
 AND NOT EXISTS(SELECT 1 FROM user_group_denials deny WHERE deny.user_id=? AND deny.group_id=rg.id) ORDER BY rg.id`, userID, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ResourceGroup{}
	for rows.Next() {
		var g ResourceGroup
		var enabled int
		if err := rows.Scan(&g.ID, &g.Name, &g.Provider, &g.AllowedModels, &enabled, &g.CreatedAt); err != nil {
			return nil, err
		}
		g.Enabled = enabled != 0
		out = append(out, g)
	}
	return out, rows.Err()
}

// GroupAllowedModels returns the group's parsed allowlist for permission
// computation ("" = not configured → portal calls must be closed; nil result
// with ok=false distinguishes that from an empty-but-configured list).
func (d *DB) GroupAllowedModels(groupID int64) (models []string, configured bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var raw sql.NullString
	err = d.db.QueryRow("SELECT allowed_models FROM resource_groups WHERE id=? AND enabled=1", groupID).Scan(&raw)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, false, nil
		}
		return nil, false, err
	}
	return parseAllowedModels(raw.String), true, nil
}

// ListResourceGroups lists all groups (admin view; account_uids left empty —
// the admin UI fetches group membership on demand).
func (d *DB) ListResourceGroups() ([]ResourceGroup, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query("SELECT id, name, provider, allowed_models, enabled, created_at FROM resource_groups ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ResourceGroup{}
	for rows.Next() {
		var g ResourceGroup
		var enabled int
		if err := rows.Scan(&g.ID, &g.Name, &g.Provider, &g.AllowedModels, &enabled, &g.CreatedAt); err != nil {
			return nil, err
		}
		g.Enabled = enabled != 0
		out = append(out, g)
	}
	return out, rows.Err()
}

// CreateInitialAdmin atomically creates the first administrator. Ordinary
// registrations never close this private bootstrap path.
func (d *DB) CreateInitialAdmin(username, passwordHash string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := float64(time.Now().UnixNano()) / 1e9
	res, err := d.db.Exec(`INSERT INTO users(username,password_hash,role,status,created_at,updated_at)
 SELECT ?,?,'admin','active',?,? WHERE NOT EXISTS(SELECT 1 FROM users WHERE role='admin')`, username, passwordHash, now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, ErrConflict
		}
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, ErrConflict
	}
	return res.LastInsertId()
}
func (d *DB) HasAdminUser() (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var n int
	err := d.db.QueryRow("SELECT COUNT(*) FROM users WHERE role='admin'").Scan(&n)
	return n > 0, err
}
func (d *DB) GroupUserIDs(groupID int64) ([]int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query(`SELECT u.id FROM users u WHERE
 (EXISTS(SELECT 1 FROM user_group_grants g WHERE g.group_id=? AND g.user_id=u.id)
 OR (u.status='active' AND ?=CAST((SELECT value FROM settings WHERE key='portal_default_group') AS INTEGER)
 AND (SELECT value FROM settings WHERE key='portal_default_auto_grant')='1'
 AND EXISTS(SELECT 1 FROM contributions c WHERE c.user_id=u.id AND c.provider='workbuddy' AND c.status='active')))
 AND NOT EXISTS(SELECT 1 FROM user_group_denials deny WHERE deny.group_id=? AND deny.user_id=u.id) ORDER BY u.id`, groupID, groupID, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
func (d *DB) GroupAccountUIDs(groupID int64) ([]string, error) {
	return d.groupAccountUIDs(groupID, false)
}
func (d *DB) GroupAccountUIDsWithActiveContribution(groupID int64) ([]string, error) {
	return d.groupAccountUIDs(groupID, true)
}
func (d *DB) groupAccountUIDs(groupID int64, active bool) ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	q := "SELECT ga.account_uid FROM group_accounts ga WHERE ga.group_id=? ORDER BY ga.account_uid"
	if active {
		q = `SELECT ga.account_uid FROM group_accounts ga JOIN resource_groups rg ON rg.id=ga.group_id
 WHERE ga.group_id=? AND rg.enabled=1 AND
 (EXISTS(SELECT 1 FROM contributions c WHERE c.account_uid=ga.account_uid AND c.status='active' AND c.provider=rg.provider)
 OR (rg.provider='workbuddy' AND NOT EXISTS(SELECT 1 FROM contributions c WHERE c.account_uid=ga.account_uid)
 AND EXISTS(SELECT 1 FROM platform_account_sharing ps JOIN accounts a ON a.uid=ps.account_uid WHERE ps.account_uid=ga.account_uid AND ps.mode IN ('shared','both') AND a.provider='workbuddy')))
 ORDER BY ga.account_uid`
	}
	rows, err := d.db.Query(q, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var uid string
		if err := rows.Scan(&uid); err != nil {
			return nil, err
		}
		out = append(out, uid)
	}
	return out, rows.Err()
}
func (d *DB) RevokeInviteCode(code string) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.db.Exec("DELETE FROM invite_codes WHERE code=? AND used_by IS NULL", code)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ReserveUserDailyQuota charges the prospective execution once, independently
// of retry/error log rows. Zero limits mean unlimited; a reservation survives
// a process crash conservatively rather than restoring spent allowance.
func (d *DB) ReserveUserDailyQuota(userID int64, day float64, requests, input, output, limitRequests, limitInput, limitOutput int64) error {
	if requests < 0 || input < 0 || output < 0 {
		return errors.New("negative quota reservation")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("INSERT OR IGNORE INTO user_daily_quota(user_id,day) VALUES(?,?)", userID, day); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE user_daily_quota SET requests=requests+?,input_tokens=input_tokens+?,output_tokens=output_tokens+?
 WHERE user_id=? AND day=? AND (?<=0 OR requests<=?-?) AND (?<=0 OR input_tokens<=?-?) AND (?<=0 OR output_tokens<=?-?)`, requests, input, output, userID, day, limitRequests, limitRequests, requests, limitInput, limitInput, input, limitOutput, limitOutput, output)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrDailyQuota
	}
	return tx.Commit()
}

// SettleUserDailyQuota replaces this execution's reservation with actual use.
// Call exactly once; use actual zero for work cancelled before it started.
func (d *DB) SettleUserDailyQuota(userID int64, day float64, reservedRequests, reservedInput, reservedOutput, actualRequests, actualInput, actualOutput int64) error {
	if reservedRequests < 0 || reservedInput < 0 || reservedOutput < 0 || actualRequests < 0 || actualInput < 0 || actualOutput < 0 {
		return errors.New("negative quota settlement")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	res, err := d.db.Exec(`UPDATE user_daily_quota SET requests=requests-?+?,input_tokens=input_tokens-?+?,output_tokens=output_tokens-?+?
 WHERE user_id=? AND day=? AND requests>=? AND input_tokens>=? AND output_tokens>=?`, reservedRequests, actualRequests, reservedInput, actualInput, reservedOutput, actualOutput, userID, day, reservedRequests, reservedInput, reservedOutput)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return errors.New("quota reservation missing")
	}
	return nil
}
func (d *DB) UserDailyQuota(userID int64, day float64) (requests, input, output int64, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	err = d.db.QueryRow("SELECT requests,input_tokens,output_tokens FROM user_daily_quota WHERE user_id=? AND day=?", userID, day).Scan(&requests, &input, &output)
	if err == sql.ErrNoRows {
		err = nil
	}
	return
}

// ActivateContributionWithGroups commits qualification and membership together.
// Only a pending verification may activate; a concurrent revoke wins.
func (d *DB) ActivateContributionWithGroups(id, userID int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE contributions SET status='active',revoked_at=NULL,revoked_reason='' WHERE id=? AND user_id=? AND status='verifying'", id, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	res, err = tx.Exec(`UPDATE accounts SET enabled=1,disabled_reason='',updated_at=? WHERE uid=(SELECT account_uid FROM contributions WHERE id=? AND user_id=?)`, float64(time.Now().UnixNano())/1e9, id, userID)
	if err != nil {
		return err
	}
	n, err = res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrConflict
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO group_accounts(group_id,account_uid)
 SELECT rg.id,c.account_uid FROM resource_groups rg JOIN contributions c ON c.id=? AND c.user_id=?
 WHERE rg.provider=c.provider AND rg.id=CAST((SELECT value FROM settings WHERE key='portal_default_group') AS INTEGER)`, id, userID); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateInvitedUser commits a new ordinary identity and one invite consumption
// together, so authentication can never observe a registration that will roll back.
func (d *DB) PlatformSharingModes() (map[string]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query("SELECT account_uid,mode FROM platform_account_sharing")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var uid, mode string
		if err := rows.Scan(&uid, &mode); err != nil {
			return nil, err
		}
		out[uid] = mode
	}
	return out, rows.Err()
}

func (d *DB) SetPlatformAccountSharing(uid, mode string, groupIDs []int64) error {
	if mode != "private" && mode != "shared" && mode != "both" {
		return errors.New("使用范围无效")
	}
	if mode != "private" && len(groupIDs) == 0 {
		return errors.New("请选择至少一个共享池")
	}
	if mode == "private" && len(groupIDs) > 0 {
		return errors.New("私人账号不能加入共享池")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var valid bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM accounts a WHERE a.uid=? AND a.provider='workbuddy' AND NOT EXISTS(SELECT 1 FROM contributions c WHERE c.account_uid=a.uid))`, uid).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return errors.New("仅可转换平台所属的 WorkBuddy 账号，不能更改用户账号归属")
	}
	for _, id := range groupIDs {
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM resource_groups WHERE id=? AND provider='workbuddy')", id).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return errors.New("共享池不存在或渠道不匹配")
		}
	}
	if _, err := tx.Exec("INSERT INTO platform_account_sharing(account_uid,mode) VALUES(?,?) ON CONFLICT(account_uid) DO UPDATE SET mode=excluded.mode", uid, mode); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM group_accounts WHERE account_uid=?", uid); err != nil {
		return err
	}
	for _, id := range groupIDs {
		if _, err := tx.Exec("INSERT OR IGNORE INTO group_accounts(group_id,account_uid) VALUES(?,?)", id, uid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) SetDefaultPortalGroup(id int64, autoGrant bool) error {
	if id < 0 {
		return errors.New("默认共享池无效")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if id > 0 {
		var valid bool
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM resource_groups WHERE id=? AND provider='workbuddy')", id).Scan(&valid); err != nil {
			return err
		}
		if !valid {
			return errors.New("默认共享池不存在")
		}
	}
	flag := "0"
	if id > 0 && autoGrant {
		flag = "1"
	}
	for key, value := range map[string]string{"portal_default_group": strconv.FormatInt(id, 10), "portal_default_auto_grant": flag} {
		if _, err := tx.Exec("INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value); err != nil {
			return err
		}
	}
	if id > 0 {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO group_accounts(group_id,account_uid) SELECT ?,account_uid FROM contributions WHERE status='active' AND provider='workbuddy'`, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	d.settingsCache = nil
	return nil
}

func (d *DB) CreateInvitedUser(username, passwordHash, code string) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := float64(time.Now().UnixNano()) / 1e9
	var valid bool
	if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM invite_codes WHERE code=? AND used_by IS NULL AND (expires_at=0 OR expires_at>?))", code, now).Scan(&valid); err != nil {
		return 0, err
	}
	if !valid {
		return 0, ErrInvalidInvite
	}
	res, err := tx.Exec("INSERT INTO users(username,password_hash,role,status,created_at,updated_at) VALUES(?,?,'user','active',?,?)", username, passwordHash, now, now)
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
	res, err = tx.Exec("UPDATE invite_codes SET used_by=?,used_at=? WHERE code=? AND used_by IS NULL AND (expires_at=0 OR expires_at>?)", id, now, code, now)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n != 1 {
		return 0, ErrInvalidInvite
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

// FindInviteCode uses the primary-key index instead of loading all invitations.
func (d *DB) FindInviteCode(code string) (map[string]any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query("SELECT code,used_by,used_at,expires_at,created_at FROM invite_codes WHERE code=?", code)
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
