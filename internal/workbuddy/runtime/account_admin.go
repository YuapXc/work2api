package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"work2api/internal/store"
	"work2api/internal/workbuddy/billing"
	"work2api/internal/workbuddy/credentials"
)

var uidSafeRe = regexp.MustCompile(`[^A-Za-z0-9_\-]`)

func SafeUID(uid string) string {
	s := uidSafeRe.ReplaceAllString(uid, "")
	s = TrimDash(s)
	if s == "" {
		return "unknown"
	}
	return s
}

func TrimDash(s string) string {
	for len(s) > 0 && s[0] == '-' {
		s = s[1:]
	}
	for len(s) > 0 && s[len(s)-1] == '-' {
		s = s[:len(s)-1]
	}
	return s
}

// RegisterAuthUpload writes an uploaded auth JSON to the project auths/ dir and
// registers it into the pool + DB.
func (o *Runtime) RegisterAuthUpload(data []byte) (map[string]any, *apiError) {
	o.AccountMu.Lock()
	defer o.AccountMu.Unlock()
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, errBody(400, "auth 文件不是合法 JSON", "invalid_request_error")
	}
	account, _ := raw["account"].(map[string]any)
	uid := ""
	if account != nil {
		uid, _ = account["uid"].(string)
	}
	if uid == "" {
		return nil, errBody(400, "auth 文件中缺少 uid", "invalid_request_error")
	}
	previousAccount, err := o.db.GetAccount(uid)
	if err != nil {
		return nil, errBody(503, "账号登记查询失败", "server_error")
	}
	if previousAccount != nil && previousAccount["provider"] != "workbuddy" {
		return nil, errBody(409, "账号标识已被其他渠道使用", "conflict")
	}
	if previous := o.Manager(uid); previous != nil {
		if err := previous.Retire(false); err != nil {
			return nil, errBody(500, "停止旧账户凭据刷新失败", "server_error")
		}
	}
	if err := os.MkdirAll(o.ProjectAuths, 0o755); err != nil {
		return nil, errBody(500, "无法创建 auths 目录", "server_error")
	}
	path := filepath.Join(o.ProjectAuths, "workbuddy-"+SafeUID(uid)+".info")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, errBody(500, "写入 auth 文件失败", "server_error")
	}
	mgr := credentials.NewManager(path)
	if err := o.db.SetAccountHidden(uid, false); err != nil {
		return nil, errBody(500, "恢复账户注册失败", "server_error")
	}
	if _, err := PersistAccount(o.db, raw); err != nil {
		return nil, errBody(500, "账号登记失败", "server_error")
	}
	added := o.Pool.AddAccount(uid, mgr)
	o.SetManager(uid, mgr)
	return map[string]any{"uid": uid, "added": added}, nil
}

func HiddenAccountKey(uid string) string {
	return store.HiddenAccountKey(uid)
}

// Delete only gateway-owned files whose decoded UID matches this account.
// Desktop files are left intact; the persisted marker prevents rediscovery.
func (o *Runtime) DeleteAccount(uid string) error {
	o.AccountMu.Lock()
	defer o.AccountMu.Unlock()
	if err := o.db.SetAccountHidden(uid, true); err != nil {
		return err
	}
	o.Pool.RemoveAccount(uid)
	mgr := o.Manager(uid)
	o.SetManager(uid, nil)
	o.LimMu.Lock()
	delete(o.Limiters, uid)
	o.LimMu.Unlock()
	o.CooldownMu.Lock()
	for key := range o.Cooldowns {
		if strings.HasPrefix(key, uid+"|") {
			delete(o.Cooldowns, key)
		}
	}
	o.CooldownMu.Unlock()
	if o.Sessions != nil {
		o.Sessions.RemoveAccount(uid)
	}
	var failures []string
	if _, err := o.db.DeleteAccount(uid); err != nil {
		failures = append(failures, "数据库清理失败: "+err.Error())
	}
	if mgr != nil {
		if err := mgr.Retire(false); err != nil {
			failures = append(failures, err.Error())
		}
	}
	root, rootErr := filepath.EvalSymlinks(o.ProjectAuths)
	if rootErr == nil {
		for _, path := range credentials.FindAuthFiles(o.ProjectAuths, "") {
			resolved, err := filepath.EvalSymlinks(path)
			if err != nil {
				continue
			}
			rel, err := filepath.Rel(root, resolved)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
				continue
			}
			candidate := credentials.NewManager(path)
			if candidate.Summary()["uid"] != uid {
				continue
			}
			if err := candidate.Retire(true); err != nil {
				failures = append(failures, err.Error())
			}
		}
	} else if !os.IsNotExist(rootErr) {
		failures = append(failures, rootErr.Error())
	}
	if len(failures) > 0 {
		return fmt.Errorf("账号已隐藏并移出分发，但部分资源清理失败：%s", strings.Join(failures, "; "))
	}
	return nil
}

// RenameAccount changes only the gateway alias, never the upstream identity.
func (o *Runtime) RenameAccount(uid, name string) error {
	o.AccountMu.Lock()
	defer o.AccountMu.Unlock()
	row, err := o.db.GetAccount(uid)
	if err != nil {
		return err
	}
	if row == nil || row["provider"] != "workbuddy" {
		return fmt.Errorf("账号不存在")
	}
	name = strings.TrimSpace(name)
	if err := o.db.SetAccountState(uid, map[string]any{"alias": name}); err != nil {
		return err
	}
	o.Pool.SetAlias(uid, name)
	return nil
}

func BillingCheckin(ctx context.Context, mgr *credentials.Manager) (billing.CheckinResult, error) {
	return billing.DailyCheckin(ctx, mgr)
}

func (o *Runtime) SetAccountEnabled(uid string, enabled bool) error {
	o.AccountMu.Lock()
	defer o.AccountMu.Unlock()
	row, err := o.db.GetAccount(uid)
	if err != nil {
		return err
	}
	if row == nil || row["provider"] != "workbuddy" {
		return fmt.Errorf("账号不存在")
	}
	value, reason := 0, "手动停用"
	if enabled {
		value, reason = 1, ""
	}
	if err := o.db.SetAccountState(uid, map[string]any{"enabled": value, "disabled_reason": reason}); err != nil {
		return err
	}
	o.Pool.SetEnabled(uid, enabled, reason)
	return nil
}

func (o *Runtime) SetAccountPriority(uid string, priority int) error {
	o.AccountMu.Lock()
	defer o.AccountMu.Unlock()
	row, err := o.db.GetAccount(uid)
	if err != nil {
		return err
	}
	if row == nil || row["provider"] != "workbuddy" {
		return fmt.Errorf("账号不存在")
	}
	if err := o.db.SetAccountState(uid, map[string]any{"priority": priority}); err != nil {
		return err
	}
	o.Pool.SetPriority(uid, priority)
	return nil
}
