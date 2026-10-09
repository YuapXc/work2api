package store

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func IsAdminRole(role string) bool { return role == "admin" || role == "owner" }

// IdentityActor is produced by server authentication, never a request body.
// The current role and version are checked again inside each write transaction.
type IdentityActor struct {
	ID       int64
	Version  int64
	Recovery bool
	Method   string
}

func identityActor(tx *sql.Tx, actor IdentityActor) (string, error) {
	if actor.Recovery {
		return "owner", nil
	}
	var role, status string
	var version int64
	var temporary bool
	err := tx.QueryRow("SELECT role,status,auth_version,must_change_password FROM users WHERE id=?", actor.ID).Scan(&role, &status, &version, &temporary)
	if err != nil || status != "active" || !IsAdminRole(role) || version != actor.Version || temporary {
		return "", errors.New("管理身份已变更，请重新登录")
	}
	return role, nil
}

func identityAudit(tx *sql.Tx, actor IdentityActor, target int64, action, before, after string) error {
	_, err := tx.Exec("INSERT INTO identity_audit(ts,actor_id,auth_method,target_id,action,before_value,after_value) VALUES(?,?,?,?,?,?,?)", float64(time.Now().UnixNano())/1e9, actor.ID, actor.Method, target, action, before, after)
	return err
}

func invalidateIdentity(tx *sql.Tx, id int64) error {
	if _, err := tx.Exec("UPDATE users SET auth_version=auth_version+1,updated_at=? WHERE id=?", float64(time.Now().UnixNano())/1e9, id); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM user_sessions WHERE user_id=?", id)
	return err
}

func (d *DB) CreateManagedUser(actor IdentityActor, username, hash, role string) (int64, error) {
	if role != "user" && role != "admin" {
		return 0, errors.New("只能创建普通用户或管理员")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	actorRole, err := identityActor(tx, actor)
	if err != nil {
		return 0, err
	}
	if role == "admin" && actorRole != "owner" {
		return 0, errors.New("只有超级管理员可以创建管理员")
	}
	now := float64(time.Now().UnixNano()) / 1e9
	res, err := tx.Exec("INSERT INTO users(username,password_hash,role,status,created_at,updated_at,must_change_password) VALUES(?,?,?,'active',?,?,1)", username, hash, role, now, now)
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
	if err = identityAudit(tx, actor, id, "create", "", role); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// ManageIdentity centralizes every mutation of another user's credentials,
// role or status. Ordinary admins can only manage ordinary users.
func (d *DB) ManageIdentity(actor IdentityActor, id int64, action, value string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	actorRole, err := identityActor(tx, actor)
	if err != nil {
		return err
	}
	var role, status string
	if err = tx.QueryRow("SELECT role,status FROM users WHERE id=?", id).Scan(&role, &status); err != nil {
		return errors.New("用户不存在")
	}
	if role == "owner" && !(actor.Recovery && action == "password") {
		return errors.New("超级管理员需自行改密或使用身份交接，不能被重置、停用或降级")
	}
	if actorRole != "owner" && role != "user" {
		return errors.New("只有超级管理员可以管理管理员")
	}
	if actor.ID == id && !actor.Recovery {
		return errors.New("不能修改自己的身份或停用自己，请使用个人改密入口")
	}
	before, after := status, value
	switch action {
	case "role":
		if actorRole != "owner" || (value != "admin" && value != "user") {
			return errors.New("只有超级管理员可以调整 user/admin 身份")
		}
		if role == value {
			return nil
		}
		before = role
		_, err = tx.Exec("UPDATE users SET role=? WHERE id=?", value, id)
	case "status":
		if value != "active" && value != "disabled" {
			return errors.New("无效账号状态")
		}
		if status == value {
			return nil
		}
		_, err = tx.Exec("UPDATE users SET status=? WHERE id=?", value, id)
	case "password":
		if value == "" {
			return errors.New("密码不能为空")
		}
		before, after = "", "temporary_password"
		_, err = tx.Exec("UPDATE users SET password_hash=?,must_change_password=1 WHERE id=?", value, id)
	default:
		return errors.New("不支持的身份操作")
	}
	if err != nil {
		return err
	}
	if err = invalidateIdentity(tx, id); err != nil {
		return err
	}
	if err = identityAudit(tx, actor, id, action, before, after); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) TransferOwner(actor IdentityActor, target int64, claim bool) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	role, err := identityActor(tx, actor)
	if err != nil {
		return err
	}
	if role != "owner" {
		return errors.New("只有超级管理员可以交接")
	}
	var ownerID int64
	err = tx.QueryRow("SELECT id FROM users WHERE role='owner'").Scan(&ownerID)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if claim {
		if !actor.Recovery || ownerID != 0 {
			return errors.New("仅恢复入口可以为尚无超级管理员的系统指定归属")
		}
	} else if ownerID == 0 || (!actor.Recovery && ownerID != actor.ID) {
		return errors.New("超级管理员归属已变更")
	}
	if ownerID == target {
		return errors.New("目标已经是超级管理员")
	}
	var targetRole, status string
	var temporary bool
	if err = tx.QueryRow("SELECT role,status,must_change_password FROM users WHERE id=?", target).Scan(&targetRole, &status, &temporary); err != nil {
		return errors.New("目标不存在")
	}
	if targetRole != "admin" || status != "active" || temporary {
		return errors.New("目标需为已完成改密的有效管理员")
	}
	if ownerID != 0 {
		if _, err = tx.Exec("UPDATE users SET role='admin' WHERE id=?", ownerID); err != nil {
			return err
		}
		if err = invalidateIdentity(tx, ownerID); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("UPDATE users SET role='owner' WHERE id=?", target); err != nil {
		return err
	}
	if err = invalidateIdentity(tx, target); err != nil {
		return err
	}
	action := "transfer_owner"
	if claim {
		action = "claim_owner"
	}
	if err = identityAudit(tx, actor, target, action, fmt.Sprint(ownerID), fmt.Sprint(target)); err != nil {
		return err
	}
	return tx.Commit()
}

func (d *DB) HasOwner() (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var count int
	err := d.db.QueryRow("SELECT COUNT(*) FROM users WHERE role='owner'").Scan(&count)
	return count > 0, err
}

func (d *DB) IdentityAudits() ([]map[string]any, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rows, err := d.db.Query(`SELECT a.*,COALESCE(u.username,'恢复入口') AS actor_name,COALESCE(t.username,'已移除账号') AS target_name
 FROM identity_audit a LEFT JOIN users u ON u.id=a.actor_id LEFT JOIN users t ON t.id=a.target_id ORDER BY a.id DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRows(rows)
}
