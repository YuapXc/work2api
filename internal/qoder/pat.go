package qoder

import (
	"context"
	"fmt"
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
	region := account.NormalizeRegion(str(opts["region"]))
	ep := account.GetEndpoints(region)
	// Ensure the install salt exists before the account is used for signing.
	if salt, err := account.EnsureMachineSalt(); err == nil {
		cosy.SetInstallSalt(salt)
		r.mu.Lock()
		r.saltSet = true
		r.mu.Unlock()
	}
	// 交换时尚无 uid：机器头按凭证稳定派生，避免每次导入生成新机器码（上游同款）。
	seed := cosy.FingerprintSeed("", token)
	jt, err := cosy.ExchangeJobTokenContext(ctx, token, cosy.DeriveMachineID(seed), cosy.DeriveMachineToken(seed), cosy.DeriveMachineType(seed), ep.JobTokenURL)
	if err != nil {
		return nil, fmt.Errorf("验证 PAT 失败: %w", err)
	}
	id := account.SanitizeID(bridge.StrVal(jt, "id") + bridge.StrVal(jt, "name"))
	if id == "" {
		return nil, fmt.Errorf("验证 PAT 失败：上游未返回账号标识")
	}
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
	if err := account.SaveSecret(acct.ID, token); err != nil {
		return nil, fmt.Errorf("保存凭据失败: %w", err)
	}
	if err := account.Save(acct); err != nil {
		_ = account.DeleteSecret(acct.ID)
		return nil, fmt.Errorf("保存账号失败: %w", err)
	}
	_ = account.SetActive(acct.ID)
	if err := account.SetGatewayHidden(acct.ID, false); err != nil {
		return nil, fmt.Errorf("恢复账户失败: %w", err)
	}
	r.mu.Lock()
	delete(r.bridges, acct.ID)
	r.bridgeGeneration[acct.ID]++
	r.mu.Unlock()
	return map[string]any{"id": acct.ID, "label": acct.Name, "region": string(acct.Region), "auth_mode": "pat"}, nil
}
