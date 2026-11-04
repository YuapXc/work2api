package runtime

import (
	"context"
	"errors"
	"time"
	"work2api/internal/core/provider"
)

func (r *Runtime) CheckAccountIDs() []string {
	ids := []string{}
	for _, a := range r.Pool.Accounts() {
		ids = append(ids, a.UID)
	}
	return ids
}
func (r *Runtime) CheckAccountIdentity(id string) string {
	if m := r.Manager(id); m != nil {
		fp, _ := m.MaintenanceIdentity()
		return fp
	}
	return ""
}
func (r *Runtime) CheckAccount(ctx context.Context, id string, force bool) provider.AccountCheck {
	a := r.Pool.Get(id)
	m := r.Manager(id)
	if a == nil || m == nil {
		return provider.Checked("unknown", "账号", "账号已移除")
	}
	if m.AuthorizationInvalid() {
		return provider.Checked("invalid", "登录授权", "已确认失效，请重新授权")
	}
	r.creditMu.Lock()
	state := r.creditStates[id]
	r.creditMu.Unlock()
	identity := r.CheckAccountIdentity(id)
	if force || state.Identity != identity || state.Failed || time.Since(time.Unix(int64(state.Updated), 0)) > provider.AccountCheckMaxAge(ctx) {
		r.RefreshCreditsFor(ctx, r.db, a)
	}
	if m.AuthorizationInvalid() {
		return provider.Checked("invalid", "登录授权", "已确认失效，请重新授权")
	}
	if ctx.Err() != nil {
		return provider.Checked("warning", "额度授权", "检查超时或取消，未判定登录失效")
	}
	pending := r.billingGuard().State(id, r.CheckAccountIdentity(id))
	if pending.Kind != "" {
		return provider.Checked("warning", "额度授权", "相关维护已暂停，等待授权恢复验证；模型调用授权单独判断")
	}
	r.creditMu.Lock()
	state = r.creditStates[id]
	r.creditMu.Unlock()
	if state.Failed || state.Updated == 0 {
		return provider.Checked("warning", "额度授权", "未能验证额度接口，保留现有调用判断")
	}
	result := provider.Checked("valid", "额度授权", "额度接口验证通过；未测试模型调用")
	result.CheckedAt = int64(state.Updated)
	return result
}

func (r *Runtime) AccountIdentityPreview(id string) (map[string]any, error) {
	m := r.Manager(id)
	if m == nil || r.Pool.Get(id) == nil {
		return nil, errors.New("账号已移除")
	}
	return m.ChatIdentityPreview()
}
