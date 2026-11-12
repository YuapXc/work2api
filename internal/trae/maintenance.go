package trae

import (
	"context"
	"errors"
	"time"

	"work2api/internal/core/provider"
	"work2api/internal/trae/upstream"
)

type quotaRefreshError struct{ cause error }

func (e *quotaRefreshError) Error() string { return "Trae 额度刷新失败" }
func (e *quotaRefreshError) Unwrap() error { return e.cause }

func (r *Runtime) AdminRefreshCredits(ctx context.Context) (map[string]any, error) {
	r.mu.Lock()
	ids := []string{}
	for id := range r.accounts {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	results := []map[string]any{}
	for _, id := range ids {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		result := map[string]any{"uid": id, "status": "error"}
		if r.refreshQuota(ctx, id, true) == nil {
			result["status"] = "ok"
		}
		results = append(results, result)
	}
	return map[string]any{"results": results}, nil
}

// One in-flight quota read per resource, even when several Agents discover an
// expired zero balance together. Callers can cancel without detached probes.
func (r *Runtime) refreshQuota(ctx context.Context, id string, force bool) error {
	r.mu.Lock()
	a := r.accounts[id]
	r.mu.Unlock()
	if a == nil {
		return errors.New("账号不存在")
	}
	if err := r.ensureMaintenance(ctx, a.Auth); err != nil {
		return err
	}
	probe := func(ctx context.Context) error { return r.refreshQuotaRaw(ctx, id, force) }
	ran, err := r.authGuard().Recover(ctx, id+":"+a.Auth.Variant+":quota", a.Auth.MaintenanceIdentity(), probe)
	if err != nil || ran {
		return err
	}
	return probe(ctx)
}
func (r *Runtime) refreshQuotaRaw(ctx context.Context, id string, force bool) (result error) {
	r.mu.Lock()
	a := r.accounts[id]
	if a == nil {
		r.mu.Unlock()
		return errors.New("账号不存在")
	}
	if done := a.QuotaProbeDone; done != nil {
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
		}
		r.mu.Lock()
		failed := r.accounts[id] != a || a.QuotaFailed
		r.mu.Unlock()
		if failed {
			return errors.New("Trae 额度刷新失败")
		}
		return nil
	}
	if !force && time.Since(a.QuotaProbeAt) < time.Minute {
		r.mu.Unlock()
		return nil
	}
	done := make(chan struct{})
	a.QuotaProbeDone, a.QuotaProbeAt = done, time.Now()
	identity := a.Auth
	r.mu.Unlock()
	fingerprint := identity.MaintenanceIdentity()
	defer func() { r.observeMaintenance(identity, "quota", fingerprint, result) }()
	err := r.ensure(ctx, identity)
	var usage *upstream.EntUsageResult
	var enterprise *upstream.EnterpriseQuota
	if err == nil {
		fingerprint = identity.MaintenanceIdentity()
		if identity.EnterpriseConsole {
			bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			ctx = bounded
			enterprise, err = r.client(ctx).EnterpriseUsage(ctx, identity)
			if err == nil && ctx.Err() == nil {
				usage, _ = r.client(ctx).UserEntUsage(identity)
			}
		} else {
			usage, err = r.client(ctx).UserEntUsage(identity)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	defer close(done)
	a.QuotaProbeDone = nil
	current, readable := identity.TryMaintenanceIdentity()
	if r.accounts[id] != a || !readable || current != fingerprint {
		if r.accounts[id] == a {
			a.QuotaFailed = true
		}
		return errors.New("Trae 账号已变更")
	}
	a.QuotaFailed = err != nil
	if err == nil && enterprise != nil {
		oldRemaining, oldTotal := a.Remaining, a.Total
		a.Remaining, a.Total, a.Expiry = nil, enterprise.Limit, nil
		if enterprise.Used == nil && a.QuotaUnit == "CNY" && enterprise.Limit != nil && oldTotal != nil && *oldTotal == *enterprise.Limit && enterprise.Reset != nil && a.QuotaReset != nil && *enterprise.Reset == *a.QuotaReset {
			a.Remaining = oldRemaining
		}
		a.QuotaUnit, a.QuotaReset, a.QuotaUnlimited = "CNY", enterprise.Reset, enterprise.Unlimited
		if enterprise.Limit != nil && enterprise.Used != nil {
			left := max(0.0, *enterprise.Limit-*enterprise.Used)
			a.Remaining = &left
		}
		pkg := map[string]any{"name": "企业席位额度", "unit": "CNY", "remain": a.Remaining, "total": a.Total, "used": enterprise.Used, "unlimited": enterprise.Unlimited}
		if enterprise.Reset != nil {
			pkg["reset_time"] = time.Unix(int64(*enterprise.Reset), 0).UTC().Format(time.RFC3339)
		}
		a.CreditPackages = []map[string]any{pkg}
		if usage != nil {
			for _, pack := range usage.UserEntitlementPackList {
				if pack.Excluded() {
					continue
				}
				q := pack.EffectiveQuota()
				if q.CreditsLimit == nil {
					continue
				}
				item := map[string]any{"name": pack.DisplayDesc, "unit": "credits", "total": nil, "remain": nil, "used": pack.Usage.CreditsAmount, "display_only": true}
				if *q.CreditsLimit >= 0 {
					item["total"] = *q.CreditsLimit
					if pack.Usage.CreditsAmount != nil {
						item["remain"] = max(0.0, float64(*q.CreditsLimit)-*pack.Usage.CreditsAmount)
					}
				}
				a.CreditPackages = append(a.CreditPackages, item)
			}
		}
		a.QuotaAt = time.Now()
		a.QuotaFailed = enterprise.Partial
		if a.CooldownKind == upstream.ErrPlanLimit && !enterprise.Partial && (enterprise.Unlimited || a.Remaining != nil && *a.Remaining > 0) {
			a.Cooldown, a.CooldownKind = time.Time{}, upstream.ErrNone
		}
	} else if err == nil {
		a.QuotaUnit, a.QuotaReset, a.QuotaUnlimited = "credits", nil, false
		pool := upstream.CreditsPoolUsage(usage.UserEntitlementPackList, usage.IsCreditsBilling)
		remaining := 0.0
		total := 0.0
		known := false
		var expiry *float64
		packages := []map[string]any{}
		for _, pack := range usage.UserEntitlementPackList {
			if pack.Excluded() {
				continue
			}
			q := pack.EffectiveQuota()
			var creditRemain *float64
			if q.CreditsLimit != nil && *q.CreditsLimit >= 0 {
				used := 0.0 // Same absent-usage convention as the upstream credits pool.
				if pack.Usage.CreditsAmount != nil {
					used = *pack.Usage.CreditsAmount
				}
				left := max(0.0, float64(*q.CreditsLimit)-used)
				creditRemain = &left
				remaining += left
			}
			if q.CreditsLimit != nil {
				name := pack.DisplayDesc
				if name == "" {
					name = "积分资源包"
				}
				item := map[string]any{"name": name, "remain": nil, "used": pack.Usage.CreditsAmount, "total": nil, "expire_at": nil, "unlimited": *q.CreditsLimit == -1}
				if *q.CreditsLimit >= 0 {
					item["total"] = *q.CreditsLimit
					item["remain"] = creditRemain
				}
				end := pack.EntitlementBaseInfo.EndTime
				if end > 1e12 {
					end /= 1000
				}
				if end > 0 && end < 4102444800 {
					item["expire_at"] = end
				}
				packages = append(packages, item)
			}
			if q.CreditsLimit != nil && *q.CreditsLimit >= 0 {
				total += float64(*q.CreditsLimit)
				known = true
			}
			end := float64(pack.EntitlementBaseInfo.EndTime)
			if end > 1e12 {
				end /= 1000
			}
			if creditRemain != nil && *creditRemain > 0 && end > 0 && end < 4102444800 && (expiry == nil || end < *expiry) {
				v := end
				expiry = &v
			}
		}
		if pool.Known && !pool.Unlimited {
			// Preserve fractional credits so a usable balance is not rounded to zero.
			a.Remaining = &remaining
		} else {
			a.Remaining = nil
		}
		if known && !pool.Unlimited {
			a.Total = &total
		} else {
			a.Total = nil
		}
		a.Expiry = expiry
		a.QuotaAt = time.Now()
		a.CreditPackages = packages
		if a.CooldownKind == upstream.ErrPlanLimit && (pool.Unlimited || pool.Known && remaining > 0) {
			// This allows a bounded chat recheck, not a promise that positive
			// credits imply remaining model quota. A rejection re-freezes it.
			a.Cooldown, a.CooldownKind = time.Time{}, upstream.ErrNone
		}
	}
	if err != nil {
		return &quotaRefreshError{cause: err}
	}
	if enterprise != nil && enterprise.Partial {
		return &quotaRefreshError{cause: errors.New("企业额度明细读取不完整")}
	}
	return nil
}

func (r *Runtime) refreshBlockedQuotas(ctx context.Context, req provider.ServeRequest, model string, image bool) {
	excluded := map[string]bool{}
	if req.Caller.UserID <= 0 {
		var err error
		excluded, err = r.db.PrivatePoolExcludedUIDs()
		if err != nil {
			return
		}
	}
	r.mu.Lock()
	ids := []string{}
	for id, a := range r.accounts {
		if !a.Enabled || req.Caller.UserID > 0 && !req.Caller.AccountScope[id] || req.Caller.UserID <= 0 && excluded[id] {
			continue
		}
		if identity, _ := a.modelIdentityFor(model, image, r.identityAvailableLocked); identity == nil {
			continue
		}
		// Healthy resources should serve immediately; recovery reads must not
		// delay a working pool because a different resource is frozen.
		if !time.Now().Before(a.Cooldown) && (a.Remaining == nil || *a.Remaining > 0) {
			r.mu.Unlock()
			return
		}
		zero := a.Remaining != nil && *a.Remaining == 0 && time.Since(a.QuotaAt) >= time.Minute
		frozen := a.CooldownKind == upstream.ErrPlanLimit && time.Since(a.QuotaProbeAt) >= 5*time.Minute
		if zero || frozen {
			ids = append(ids, id)
		}
	}
	r.mu.Unlock()
	// Cap the recovery round: startup/large pools must not add N network reads
	// before a request. Subsequent calls can probe the remaining stale resources.
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	for i, id := range ids {
		if i == 3 || probeCtx.Err() != nil {
			break
		}
		if provider.CheckDispatch(probeCtx, provider.AccountRef{Provider: "trae", LocalID: id}, "trae/"+model) == nil {
			_ = r.refreshQuota(probeCtx, id, false)
		}
	}
}
func (r *Runtime) AdminCheckin(ctx context.Context) (map[string]any, error) {
	// Serialize manual/scheduled claim batches without blocking inference or
	// trapping a canceled waiter behind the maintenance network operation.
	r.mu.Lock()
	if r.checkinPermit == nil {
		r.checkinPermit = make(chan struct{}, 1)
	}
	permit := r.checkinPermit
	r.mu.Unlock()
	select {
	case permit <- struct{}{}:
		defer func() { <-permit }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	r.mu.Lock()
	ids := []string{}
	for id := range r.accounts {
		ids = append(ids, id)
	}
	r.mu.Unlock()
	results := []map[string]any{}
	today := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600)).Format("2006-01-02")
	for _, id := range ids {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		r.mu.Lock()
		a := r.accounts[id]
		already := a != nil && a.CheckinDate == today
		r.mu.Unlock()
		if a == nil {
			continue
		}
		if already {
			results = append(results, map[string]any{"uid": id, "status": "already"})
			continue
		}
		result := map[string]any{"uid": id, "status": "error"}
		fingerprint := a.Auth.MaintenanceIdentity()
		err := r.ensureMaintenance(ctx, a.Auth)
		if err == nil {
			fingerprint = a.Auth.MaintenanceIdentity()
			c := r.client(ctx)
			var status *upstream.CheckinStatusResult
			probe := func(ctx context.Context) error {
				fingerprint := a.Auth.MaintenanceIdentity()
				var e error
				status, e = c.CheckinStatus(a.Auth)
				r.observeMaintenance(a.Auth, "checkin", fingerprint, e)
				return e
			}
			ran, e := r.authGuard().Recover(ctx, id+":"+a.Auth.Variant+":checkin", a.Auth.MaintenanceIdentity(), probe)
			if e == nil && (!ran || status == nil) {
				e = probe(ctx)
			}
			err = e
			if err == nil {
				if !status.Enable {
					result["status"] = "no_campaign"
				} else if status.CheckedIn {
					result["status"] = "already"
				} else if status.DidCheckedIn {
					result["status"] = "device_checked"
					result["message"] = "设备已参与，未确认该账号签到成功"
				} else {
					sent, markerErr := r.checkinSent(id, today)
					if markerErr != nil {
						err = markerErr
					} else if sent {
						result["status"] = "pending"
						result["message"] = "今日已发起领取，上游尚未确认签到；仅刷新状态"
					} else if markerErr = r.markCheckinSent(id, today); markerErr != nil {
						err = markerErr
					} else {
						r.mu.Lock()
						if r.accounts[id] == a {
							a.CheckinPending = today
						}
						r.mu.Unlock()
						fingerprint := a.Auth.MaintenanceIdentity()
						_, err = c.CheckinClaim(a.Auth)
						r.observeMaintenance(a.Auth, "checkin", fingerprint, err)
						if err != nil {
							var rejection *upstream.Error
							if errors.As(err, &rejection) && (rejection.BizCode != 0 || rejection.Status == 401 || rejection.Status == 403) {
								if clearErr := r.clearCheckinSent(id); clearErr != nil {
									result["message"] = "领取被拒绝，领取状态记录清理失败"
								} else {
									r.mu.Lock()
									if r.accounts[id] == a {
										a.CheckinPending = ""
									}
									r.mu.Unlock()
								}
							} else {
								result["status"] = "pending"
							}
						} else {
							// code:0 acknowledges the claim, not the day's check-in status.
							result["status"] = "pending"
							result["message"] = "领取已受理，等待上游确认签到"
							after, readErr := c.CheckinStatus(a.Auth)
							r.observeMaintenance(a.Auth, "checkin", fingerprint, readErr)
							if readErr == nil && after.CheckedIn {
								result["status"] = "ok"
								delete(result, "message")
							}
						}
					}

				}
			}
		}
		if err != nil {
			result["message"] = "上游未确认签到成功，请刷新状态核实"
			if errors.Is(err, provider.ErrMaintenanceLogin) {
				result["status"] = "reauthorize"
				result["message"] = err.Error()
			} else if errors.Is(err, provider.ErrMaintenancePaused) {
				result["status"] = "paused"
				result["message"] = err.Error()
			}
			var rejection *upstream.Error
			if errors.As(err, &rejection) {
				result["code"] = rejection.BizCode
			}
		}
		r.mu.Lock()
		current, readable := a.Auth.TryMaintenanceIdentity()
		valid := r.accounts[id] == a && readable && current == fingerprint
		if valid && result["status"] == "pending" {
			a.CheckinPending = today
		}
		r.mu.Unlock()
		if !valid {
			result["status"] = "paused"
			result["message"] = "账号授权已变化，等待新凭据维护"
		}
		if result["status"] == "ok" || result["status"] == "already" {
			if err := r.db.RecordCheckin(id, today, "trae"); err != nil {
				return nil, err
			}
			r.mu.Lock()
			current, readable := a.Auth.TryMaintenanceIdentity()
			if r.accounts[id] == a && readable && current == fingerprint {
				a.CheckinDate = today
			}
			r.mu.Unlock()
		}
		results = append(results, result)
	}
	return map[string]any{"results": results}, nil
}
func (r *Runtime) CheckinHistory(ctx context.Context, days int) (provider.CheckinCalendar, error) {
	if days < 1 || days > 366 {
		days = 30
	}
	now := time.Now().In(time.FixedZone("Asia/Shanghai", 8*3600))
	history, err := r.db.CheckinHistory(now.AddDate(0, 0, -days).Format("2006-01-02"))
	if err != nil {
		return provider.CheckinCalendar{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	filtered := map[string][]string{}
	for id, dates := range history {
		if r.accounts[id] != nil {
			filtered[id] = dates
		}
	}
	today := now.Format("2006-01-02")
	return provider.CheckinCalendar{History: filtered, Today: today, WindowDate: today, Timezone: "Asia/Shanghai"}, nil
}
