package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"work2api/internal/core/provider"
	"work2api/internal/workbuddy/billing"
	"work2api/internal/workbuddy/credentials"
	"work2api/internal/workbuddy/pool"
)

func (r *Runtime) billingGuard() *provider.MaintenanceGuard {
	r.creditMu.Lock()
	defer r.creditMu.Unlock()
	if r.maintenanceGuard == nil {
		path := ""
		if r.cfg != nil && r.cfg.DataDir != "" {
			path = filepath.Join(r.cfg.DataDir, ".workbuddy-maintenance.json")
		}
		r.maintenanceGuard = provider.NewMaintenanceGuard(path)
	}
	return r.maintenanceGuard
}
func (r *Runtime) observeBilling(uid string, mgr *credentials.Manager, err error) {
	if r.Manager(uid) != mgr || errors.Is(err, context.Canceled) {
		return
	}
	identity, e := mgr.MaintenanceIdentity()
	if e != nil {
		return
	}
	g := r.billingGuard()
	g.State(uid, identity)
	kind := ""
	hard := errors.Is(err, credentials.ErrLoginRequired)
	if err != nil {
		kind = "temporary"
		var q *billing.QueryError
		if errors.As(err, &q) {
			kind = q.Kind
		}
		if hard {
			kind = "authorization_expired"
		}
	}
	g.Observe(uid, identity, kind, hard)
}
func (r *Runtime) prepareBillingMaintenance(ctx context.Context, acc *pool.Account) error {
	mgr := r.Manager(acc.UID)
	if mgr == nil {
		return errors.New("账号不存在")
	}
	identity, err := mgr.MaintenanceIdentity()
	if err != nil {
		return err
	}
	_, err = r.billingGuard().Recover(ctx, acc.UID, identity, func(ctx context.Context) error { return r.refreshCreditsFor(ctx, r.db, acc, mgr) })
	return err
}
func (r *Runtime) checkinAccount(ctx context.Context, acc *pool.Account) (billing.CheckinResult, error) {
	if err := r.prepareBillingMaintenance(ctx, acc); err != nil {
		return billing.CheckinResult{}, err
	}
	mgr := r.Manager(acc.UID)
	if mgr == nil {
		return billing.CheckinResult{}, errors.New("账号不存在")
	}
	if _, err := mgr.GetHeadersContext(ctx); err != nil {
		r.observeBilling(acc.UID, mgr, err)
		return billing.CheckinResult{}, err
	}
	identity, _ := mgr.MaintenanceIdentity()
	res, err := BillingCheckin(ctx, mgr)
	current, _ := mgr.MaintenanceIdentity()
	if current != identity || r.Manager(acc.UID) != mgr {
		return billing.CheckinResult{}, provider.ErrMaintenancePaused
	}
	r.observeBilling(acc.UID, mgr, err)
	if err == nil && res.OK {
		r.RefreshCreditsFor(ctx, r.db, acc)
	}
	return res, err
}
func maintenancePaused(err error) bool {
	var query *billing.QueryError
	authorization := errors.As(err, &query) && (query.Kind == "unauthorized" || query.Kind == "login_expired" || query.Kind == "credential")
	return authorization || errors.Is(err, provider.ErrMaintenancePaused) || errors.Is(err, provider.ErrMaintenanceLogin) || errors.Is(err, credentials.ErrLoginRequired)
}
