package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"

	"work2api/internal/workbuddy/billing"
	"work2api/internal/workbuddy/credentials"
)

var uidSafeRe = regexp.MustCompile(`[^A-Za-z0-9_\-]`)

func safeUID(uid string) string {
	s := uidSafeRe.ReplaceAllString(uid, "")
	s = trimDash(s)
	if s == "" {
		return "unknown"
	}
	return s
}

func trimDash(s string) string {
	for len(s) > 0 && s[0] == '-' {
		s = s[1:]
	}
	for len(s) > 0 && s[len(s)-1] == '-' {
		s = s[:len(s)-1]
	}
	return s
}

// registerAuthUpload writes an uploaded auth JSON to the project auths/ dir and
// registers it into the pool + DB.
func (o *Orchestrator) registerAuthUpload(data []byte) (map[string]any, *apiError) {
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
	if err := os.MkdirAll(o.projectAuths, 0o755); err != nil {
		return nil, errBody(500, "无法创建 auths 目录", "server_error")
	}
	path := filepath.Join(o.projectAuths, "workbuddy-"+safeUID(uid)+".info")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return nil, errBody(500, "写入 auth 文件失败", "server_error")
	}
	mgr := credentials.NewManager(path)
	added := o.pool.AddAccount(uid, mgr)
	o.managers[uid] = mgr
	if raw2, err := mgr.RawSession(); err == nil {
		_, _ = o.db.UpsertAccount(map[string]any{"auth": raw2.Auth, "account": raw2.Account})
	}
	return map[string]any{"uid": uid, "added": added}, nil
}

func billingCheckin(ctx context.Context, mgr *credentials.Manager) (billing.CheckinResult, error) {
	return billing.DailyCheckin(ctx, mgr)
}
