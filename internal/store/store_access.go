package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Each legacy grant retains its exact account set. '*' denotes a new explicit
// administrator grant over the public pool, never an inferred legacy scope.
type ModelGrant struct {
	Model       string   `json:"model"`
	AccountUIDs []string `json:"account_uids"`
}

func migratePortalAccess(tx *sql.Tx, version int) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS user_model_grants (
 user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 model TEXT NOT NULL, account_uid TEXT NOT NULL,
 PRIMARY KEY(user_id,model,account_uid));`); err != nil {
		return err
	}
	if version >= 21 {
		return nil
	}
	if _, err := tx.Exec(`ALTER TABLE apps ADD COLUMN model_policy TEXT NOT NULL DEFAULT 'fixed' CHECK(model_policy IN ('fixed','follow'))`); err != nil && !strings.Contains(err.Error(), "duplicate column") {
		return err
	}
	// Capture only previously effective groups and eligible credentials. Empty
	// or malformed historical model lists never become unrestricted grants.
	_, err := tx.Exec(`INSERT OR IGNORE INTO user_model_grants(user_id,model,account_uid)
 SELECT u.id,j.value,ga.account_uid FROM users u CROSS JOIN resource_groups g
 JOIN group_accounts ga ON ga.group_id=g.id
 JOIN json_each(CASE WHEN json_valid(g.allowed_models) THEN g.allowed_models ELSE '[]' END) j
 WHERE g.enabled=1 AND g.provider IN ('workbuddy','trae') AND j.type='text'
 AND trim(j.value)<>'' AND j.value NOT IN ('auto','workbuddy/auto')
 AND EXISTS(SELECT 1 FROM contributions c WHERE c.user_id=u.id AND c.status='active')
 AND (EXISTS(SELECT 1 FROM user_group_grants x WHERE x.user_id=u.id AND x.group_id=g.id)
 OR (g.id=CAST((SELECT value FROM settings WHERE key='portal_default_group') AS INTEGER)
 AND (SELECT value FROM settings WHERE key='portal_default_auto_grant')='1'
 AND EXISTS(SELECT 1 FROM contributions c WHERE c.user_id=u.id AND c.provider=g.provider AND c.status='active')))
 AND NOT EXISTS(SELECT 1 FROM user_group_denials x WHERE x.user_id=u.id AND x.group_id=g.id)
 AND (EXISTS(SELECT 1 FROM contributions c WHERE c.account_uid=ga.account_uid AND c.status='active' AND c.provider=g.provider)
 OR (NOT EXISTS(SELECT 1 FROM contributions c WHERE c.account_uid=ga.account_uid)
 AND EXISTS(SELECT 1 FROM platform_account_sharing p JOIN accounts a ON a.uid=p.account_uid
 WHERE p.account_uid=ga.account_uid AND p.mode IN ('shared','both') AND a.provider=g.provider)))`)
	return err
}

func (d *DB) UserModelGrants(userID int64) ([]ModelGrant, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query(`SELECT model,account_uid FROM user_model_grants WHERE user_id=? ORDER BY model,account_uid`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ModelGrant{}
	for rows.Next() {
		var m, uid string
		if err := rows.Scan(&m, &uid); err != nil {
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].Model != m {
			out = append(out, ModelGrant{Model: m, AccountUIDs: []string{}})
		}
		out[len(out)-1].AccountUIDs = append(out[len(out)-1].AccountUIDs, uid)
	}
	return out, rows.Err()
}

// AllUserModelGrants supplies the administrator's all-user view without N+1 queries.
func (d *DB) AllUserModelGrants() (map[int64][]ModelGrant, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query(`SELECT user_id,model,account_uid FROM user_model_grants ORDER BY user_id,model,account_uid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]ModelGrant{}
	for rows.Next() {
		var userID int64
		var model, uid string
		if err := rows.Scan(&userID, &model, &uid); err != nil {
			return nil, err
		}
		grants := out[userID]
		if len(grants) == 0 || grants[len(grants)-1].Model != model {
			grants = append(grants, ModelGrant{Model: model, AccountUIDs: []string{}})
		}
		grants[len(grants)-1].AccountUIDs = append(grants[len(grants)-1].AccountUIDs, uid)
		out[userID] = grants
	}
	return out, rows.Err()
}

// Replace public grants while preserving the scoped legacy grants. A caller
// must explicitly delete a legacy grant by model rather than accidentally
// widening it when saving the ordinary model picker.
func (d *DB) SetUserPublicModels(userID int64, models, removeScoped []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	if err = tx.QueryRow("SELECT COUNT(*) FROM users WHERE id=?", userID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	if _, err = tx.Exec("DELETE FROM user_model_grants WHERE user_id=? AND account_uid='*'", userID); err != nil {
		return err
	}
	for _, m := range removeScoped {
		if _, err = tx.Exec("DELETE FROM user_model_grants WHERE user_id=? AND model=?", userID, m); err != nil {
			return err
		}
	}
	for _, m := range models {
		if strings.TrimSpace(m) == "" {
			return fmt.Errorf("empty model")
		}
		if _, err = tx.Exec("INSERT OR IGNORE INTO user_model_grants VALUES(?,?,'*')", userID, m); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type DefaultModelPolicy struct {
	Mode    string   `json:"mode"`
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

func DefaultPolicy(settings map[string]string) DefaultModelPolicy {
	p := DefaultModelPolicy{Mode: "auto", Include: []string{}, Exclude: []string{}}
	if settings["portal_model_policy"] != "" {
		if json.Unmarshal([]byte(settings["portal_model_policy"]), &p) != nil || (p.Mode != "auto" && p.Mode != "manual") {
			return DefaultModelPolicy{Mode: "manual", Include: []string{}, Exclude: []string{}}
		}
	}
	return p
}

func (d *DB) ActivateContribution(id, userID int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE contributions SET status='active',revoked_at=NULL,revoked_reason='' WHERE id=? AND user_id=? AND status='verifying' AND EXISTS(SELECT 1 FROM users WHERE id=? AND status='active' AND must_change_password=0)", id, userID, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrConflict
	}
	if _, err := tx.Exec("UPDATE accounts SET enabled=1,disabled_reason='' WHERE uid=(SELECT account_uid FROM contributions WHERE id=?)", id); err != nil {
		return err
	}
	return tx.Commit()
}
