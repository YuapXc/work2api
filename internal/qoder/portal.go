package qoder

import (
	"context"
	"errors"
	"fmt"
	"work2api/internal/core/provider"
	"work2api/internal/qoder/account"
)

func PortalAccountUID(id string) string { return "qoder:" + id }
func (r *Runtime) SupportsPortal() bool { return true }

func (r *Runtime) PortalModels(_ context.Context, models map[string]bool, discoverFree bool, _ func(string) bool) []provider.PortalModel {
	var out []provider.PortalModel
	for _, m := range r.catalogModels(models) {
		if models != nil && !models[m.ID] {
			continue
		}
		uids, _ := m.Extra["account_uids"].([]string)
		factor, _ := m.Extra["price_factor"].(*float64)
		free := m.Extra["has_free_source"] == true || m.Extra["pricing_known"] == true && factor != nil && *factor == 0
		out = append(out, provider.NewPortalModel(m.ID, uids, discoverFree && free))
	}
	return out
}

func accountAllowed(ctx context.Context, id, model string) error {
	call := provider.CurrentInvocation(ctx)
	if call.Caller.UserID > 0 && !call.Caller.AccountScope[PortalAccountUID(id)] {
		return provider.NewAPIError(403, "账号不在当前调用范围", "account_not_allowed")
	}
	return provider.CheckDispatch(ctx, provider.AccountRef{Provider: runtimeName, LocalID: PortalAccountUID(id)}, model)
}

func (r *Runtime) pickServingAccount(ctx context.Context, model string) (*account.Account, string, error) {
	preferred, secret, e := r.pickAccount()
	if e == nil && accountAllowed(ctx, preferred.ID, model) == nil {
		return preferred, secret, nil
	}
	accounts, e := account.List()
	if e != nil {
		return nil, "", e
	}
	for i := range accounts {
		a := &accounts[i]
		if account.IsGatewayHidden(a.ID) {
			continue
		}
		if err := accountAllowed(ctx, a.ID, model); err != nil {
			var api *provider.APIError
			if errors.As(err, &api) && api.Status >= 500 {
				return nil, "", err
			}
			continue
		}
		s, err := account.GetSecret(a.ID)
		if err == nil && s != "" {
			return a, s, nil
		}
	}
	for _, local := range r.detectLocal() {
		a := &account.Account{ID: "qoder-local-" + local.Region, Region: account.NormalizeRegion(local.Region)}
		if !account.IsGatewayHidden(a.ID) && accountAllowed(ctx, a.ID, model) == nil {
			return a, local.DeviceToken, nil
		}
	}
	return nil, "", fmt.Errorf("没有符合当前权限的 Qoder 账号")
}

func (r *Runtime) RefreshOwnedAccount(ctx context.Context, id string) error {
	a, err := account.Get(id)
	if err != nil {
		return err
	}
	s, err := account.GetSecret(id)
	if err != nil {
		return err
	}
	return r.refreshAccountModels(ctx, a, s, modelCatalogKey(a, s))
}

// Restore every credential catalog before user model permissions are served.
func (r *Runtime) Warm(ctx context.Context) {
	if r.Ready() {
		_ = r.RefreshModels(ctx)
	}
}
