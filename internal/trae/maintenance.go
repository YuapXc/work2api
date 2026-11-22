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
func (r *Runtime) refreshQuota(ctx context.Context, id string, force bool, expiryRecovery ...bool) error {
	r.mu.Lock()
	a := r.accounts[id]
	r.mu.Unlock()
	if a == nil {
		return errors.New("账号不存在")
	}
	if err := r.ensureMaintenance(ctx, a.Auth); err != nil {
		return err
	}
	probe := func(ctx context.Context) error { return r.refreshQuotaRaw(ctx, id, force, expiryRecovery...) }
	ran, err := r.authGuard().Recover(ctx, id+":"+a.Auth.Variant+":quota", a.Auth.MaintenanceIdentity(), probe)
	if err != nil || ran {
		return err
	}
	return probe(ctx)
}
func (r *Runtime) refreshQuotaRaw(ctx context.Context, id string, force bool, expiryRecovery ...bool) (result error) {
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
	if !force && len(expiryRecovery) > 0 && expiryRecovery[0] {
		budget := a.creditBudget(nowSec())
		force = budget.Remaining != nil && *budget.Remaining <= 0 && budget.ExpiredAt != nil && float64(a.QuotaProbeAt.UnixNano())/1e9 < *budget.ExpiredAt
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
	enterprisePrimary := identity.UsesEnterpriseConsole()
	if err == nil {
		fingerprint = identity.MaintenanceIdentity()
		bounded, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		ctx = bounded
		if enterprisePrimary {
			enterprise, err = r.client(ctx).EnterpriseUsage(ctx, identity)
			usage = &upstream.EntUsageResult{}
			if err == nil && force {
				if reference, referenceErr := r.client(ctx).UserEntUsage(identity); referenceErr == nil {
					usage = reference
				}
			}
		} else {
			usage, err = r.client(ctx).UserEntUsage(identity)
			if identity.EnterpriseConsole && err == nil && force {
				enterprise, _ = r.client(ctx).EnterpriseUsage(ctx, identity)
			}
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	defer close(done)
	a.QuotaProbeDone = nil
	current, readable := identity.TryMaintenanceIdentity()
	currentRoute, routeReadable := identity.TryUsesEnterpriseConsole()
	if r.accounts[id] != a || !readable || !routeReadable || current != fingerprint || currentRoute != enterprisePrimary {
		if r.accounts[id] == a {
			a.QuotaFailed = true
		}
		return errors.New("Trae 账号已变更")
	}
	if a.QuotaUnit == "CNY" && !enterprisePrimary {
		a.Remaining, a.Total, a.Expiry = nil, nil, nil
		a.QuotaUnit, a.QuotaReset, a.QuotaUnlimited = "credits", nil, false
		for _, pkg := range a.CreditPackages {
			pkg["display_only"] = true
		}
	}
	a.QuotaFailed = err != nil
	oldRemain, oldTotal, oldReset, oldUnit := a.Remaining, a.Total, a.QuotaReset, a.QuotaUnit
	if err == nil {
		a.QuotaUnit, a.QuotaReset, a.QuotaUnlimited = "credits", nil, false
		now := float64(time.Now().Unix())
		live := make([]upstream.EntitlementPack, 0, len(usage.UserEntitlementPackList))
		creditsKnown := usage.IsCreditsBilling
		for _, pack := range usage.UserEntitlementPackList {
			if !pack.Excluded() && pack.EffectiveQuota().CreditsLimit != nil {
				creditsKnown = true
			}
			end := float64(pack.EntitlementBaseInfo.EndTime)
			if end > 1e12 {
				end /= 1000
			}
			if end > 0 && end <= now {
				continue
			}
			live = append(live, pack)
		}
		pool := upstream.CreditsPoolUsage(live, creditsKnown)
		remaining := 0.0
		total := 0.0
		known := false
		var expiry *float64
		packages := []map[string]any{}
		for _, pack := range live {
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
			if creditRemain != nil && *creditRemain > 0 && end > now && end < 4102444800 && (expiry == nil || end < *expiry) {
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
		if enterprisePrimary {
			for _, pkg := range packages {
				pkg["unit"], pkg["display_only"] = "credits", true
			}
		}
		if enterprise != nil {
			reference := map[string]any{"name": "企业席位额度（非当前调用余额）", "unit": "CNY", "total": enterprise.Limit, "used": enterprise.Used, "remain": nil, "display_only": true, "unlimited": enterprise.Unlimited}
			if enterprise.Limit != nil && enterprise.Used != nil {
				reference["remain"] = max(0.0, *enterprise.Limit-*enterprise.Used)
			}
			if enterprise.Reset != nil {
				reference["reset_time"] = time.Unix(int64(*enterprise.Reset), 0).UTC().Format(time.RFC3339)
			}
			packages = append(packages, reference)
		} else if identity.EnterpriseConsole {
			reference := map[string]any{"name": "企业席位额度（参考信息待刷新）", "unit": "CNY", "display_only": true}
			for _, old := range a.CreditPackages {
				if old["unit"] == "CNY" && old["display_only"] == true {
					for k, v := range old {
						if k != "name" {
							reference[k] = v
						}
					}
					break
				}
			}
			packages = append(packages, reference)
		}
		if enterprisePrimary {
			a.QuotaUnit, a.QuotaReset, a.QuotaUnlimited = "CNY", enterprise.Reset, enterprise.Unlimited
			a.Total, a.Remaining, a.Expiry = enterprise.Limit, nil, nil
			if enterprise.Limit != nil && enterprise.Used != nil && !enterprise.Unlimited {
				left := max(0.0, *enterprise.Limit-*enterprise.Used)
				a.Remaining = &left
			}
			if enterprise.Used == nil && oldUnit == "CNY" && oldRemain != nil && oldTotal != nil && enterprise.Limit != nil && *oldTotal == *enterprise.Limit && oldReset != nil && enterprise.Reset != nil && *oldReset == *enterprise.Reset {
				a.Remaining = oldRemain
			}
			primary := packages[len(packages)-1]
			primary["name"], primary["display_only"], primary["remain"] = "企业席位额度", false, a.Remaining
			a.QuotaFailed = enterprise.Partial
		}
		a.CreditPackages = packages
		if a.CooldownKind == upstream.ErrPlanLimit && (!enterprisePrimary && (pool.Unlimited || pool.Known && remaining > 0) || enterprisePrimary && !enterprise.Partial && (enterprise.Unlimited || a.Remaining != nil && *a.Remaining > 0)) {
			// This allows a bounded chat recheck, not a promise that positive
			// credits imply remaining model quota. A rejection re-freezes it.
			a.Cooldown, a.CooldownKind = time.Time{}, upstream.ErrNone
		}
	}
	if err != nil {
		return &quotaRefreshError{cause: err}
	}
	if enterprisePrimary && enterprise.Partial {
		return &quotaRefreshError{cause: errors.New("企业席位消费或重置时间暂不可用")}
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
	now := time.Now()
	nowFloat := float64(now.UnixNano()) / 1e9
	for id, a := range r.accounts {
		if !a.Enabled || req.Caller.UserID > 0 && !req.Caller.AccountScope[id] || req.Caller.UserID <= 0 && excluded[id] {
			continue
		}
		if identity, _ := a.modelIdentityFor(model, image, r.identityAvailableLocked); identity == nil {
			continue
		}
		// Healthy resources should serve immediately; recovery reads must not
		// delay a working pool because a different resource is frozen.
		budget := a.creditBudget(nowFloat)
		empty := budget.Remaining != nil && *budget.Remaining <= 0
		if !now.Before(a.Cooldown) && !empty {
			r.mu.Unlock()
			return
		}
		zero := empty && now.Sub(a.QuotaAt) >= time.Minute
		crossed := empty && budget.ExpiredAt != nil && float64(a.QuotaProbeAt.UnixNano())/1e9 < *budget.ExpiredAt
		frozen := a.CooldownKind == upstream.ErrPlanLimit && time.Since(a.QuotaProbeAt) >= 5*time.Minute
		if zero || frozen || crossed {
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
			_ = r.refreshQuota(probeCtx, id, false, true)
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
						var claim *upstream.CheckinClaimResult
						claim, err = c.CheckinClaim(a.Auth)
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
								if claim != nil && claim.ClaimCredits != nil {
									result["credit"] = *claim.ClaimCredits
								} else if after.RewardCredits != nil {
									result["credit"] = *after.RewardCredits
								}
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
		if result["status"] == "ok" || result["status"] == "already" {
			result["ok"] = true
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
