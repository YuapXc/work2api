package qoder

import (
	"context"
	"fmt"
	"strings"
	"time"

	"work2api/internal/core/provider"
	"work2api/internal/qoder/account"
	"work2api/internal/qoder/bridge"
	"work2api/internal/qoder/cosy"
)

// qoder can also add an account from a pasted Personal Access Token (upstream
// qoder2api service.AddAccountByPAT): no browser/scan flow needed, which is
// the only viable path on a headless server.
var _ provider.AccountImporter = (*Runtime)(nil)

// AddAccountByToken validates a Qoder PAT by exchanging it for a job token
// (same call the bridge itself makes), persists the account + secret, marks it
// active and unhidden, and invalidates any cached bridge for the id.
func (r *Runtime) AddAccountByToken(ctx context.Context, token string, opts map[string]any) (map[string]any, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("PAT 不能为空")
	}
	region := account.NormalizeRegion(str(opts["region"]))
	ep := account.GetEndpoints(region)
	// Ensure the install salt exists before the account is used for signing.
	salt, err := account.EnsureMachineSalt()
	if err != nil {
		return nil, fmt.Errorf("初始化设备身份失败: %w", err)
	}
	cosy.SetInstallSalt(salt)
	r.mu.Lock()
	r.saltSet = true
	r.mu.Unlock()
	// 交换时尚无 uid：机器头按凭证稳定派生，避免每次导入生成新机器码（上游同款）。
	seed := cosy.FingerprintSeed("", token)
	jt, err := cosy.ExchangeJobTokenContext(ctx, token, cosy.DeriveMachineID(seed), cosy.DeriveMachineToken(seed), cosy.DeriveMachineType(seed), ep.JobTokenURL)
	if err != nil {
		return nil, fmt.Errorf("验证 PAT 失败: %w", err)
	}
	if code, present := jt["code"]; present && code != nil {
		switch code := code.(type) {
		case float64:
			if code != 0 {
				return nil, fmt.Errorf("验证 PAT 失败：上游拒绝交换")
			}
		case string:
			if code != "0" {
				return nil, fmt.Errorf("验证 PAT 失败：上游拒绝交换")
			}
		default:
			return nil, fmt.Errorf("验证 PAT 失败：上游响应格式错误")
		}
	}
	rawID := strings.TrimSpace(bridge.StrVal(jt, "id"))
	sessionToken := strings.TrimSpace(bridge.StrVal(jt, "securityOauthToken"))
	if rawID == "" || sessionToken == "" {
		return nil, fmt.Errorf("验证 PAT 失败：上游未返回有效账号身份和会话凭证")
	}
	id := account.SanitizeID(rawID + bridge.StrVal(jt, "name"))
	acct := &account.Account{
		ID:        id,
		Name:      bridge.StrVal(jt, "name"),
		Email:     bridge.StrVal(jt, "email"),
		UserType:  bridge.StrValDefault(jt, "userType", "personal_standard"),
		Region:    region,
		AuthMode:  "pat",
		APIMode:   "openai",
		Tags:      []string{},
		CreatedAt: time.Now(),
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := account.ImportAndActivate(acct, token); err != nil {
		return nil, fmt.Errorf("保存及激活账号失败: %w", err)
	}
	delete(r.bridges, acct.ID)
	delete(r.bridgeKeys, acct.ID)
	r.bridgeGeneration[acct.ID]++
	return map[string]any{"id": acct.ID, "label": acct.Name, "region": string(acct.Region), "auth_mode": "pat"}, nil
}
