package qoder

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"
	"work2api/internal/core/provider"
	"work2api/internal/qoder/account"
)

func (r *Runtime) CheckAccountIDs() []string {
	ids := []string{}
	accounts, _ := account.List()
	for _, a := range accounts {
		if !account.IsGatewayHidden(a.ID) {
			ids = append(ids, a.ID)
		}
	}
	for _, c := range r.detectLocal() {
		id := "qoder-local-" + c.Region
		if !account.IsGatewayHidden(id) {
			ids = append(ids, id)
		}
	}
	return ids
}
func (r *Runtime) checkCredential(id string) (*account.Account, string) {
	if account.IsGatewayHidden(id) {
		return nil, ""
	}
	if strings.HasPrefix(id, "qoder-local-") {
		for _, c := range r.detectLocal() {
			if id == "qoder-local-"+c.Region {
				return &account.Account{ID: id, Region: account.NormalizeRegion(c.Region)}, c.DeviceToken
			}
		}
		return nil, ""
	}
	a, e := account.Get(id)
	if e != nil {
		return nil, ""
	}
	secret, _ := account.GetSecret(id)
	return a, secret
}
func (r *Runtime) CheckAccountIdentity(id string) string {
	a, s := r.checkCredential(id)
	if a == nil || s == "" {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(string(a.Region)+"\x00"+s)))
}
func (r *Runtime) CheckAccount(ctx context.Context, id string, force bool) provider.AccountCheck {
	a, s := r.checkCredential(id)
	if a == nil || s == "" {
		return provider.Checked("unknown", "账号", "未找到可用凭据")
	}
	// Signed model discovery verifies the same identity used for inference and
	// also supports PAT accounts; device-token check-in is not an auth verdict.
	key := modelCatalogKey(a, s)
	r.mu.Lock()
	cached := r.accountModels[key]
	r.mu.Unlock()
	if !force && cached.expires.After(time.Now()) && time.Since(cached.expires.Add(-5*time.Minute)) < provider.AccountCheckMaxAge(ctx) {
		result := provider.Checked("valid", "模型目录授权", "已读取该账号模型目录；未测试模型调用")
		result.CheckedAt = cached.expires.Add(-5 * time.Minute).Unix()
		return result
	}
	err := r.refreshAccountCatalog(ctx, a, s, key, true)
	if err != nil {
		return provider.Checked("warning", "模型目录授权", "目录验证未通过，请核实授权或网络；未判定整号失效")
	}
	return provider.Checked("valid", "模型目录授权", "已读取该账号模型目录；未测试模型调用")
}
