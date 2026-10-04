package qoder

import (
	"context"
	"strconv"
	"time"
	"work2api/internal/qoder/account"
	"work2api/internal/qoder/checkin"
)

// Maintain is serialized by the scheduler; no independent immortal timer exists.
func (r *Runtime) Maintain(ctx context.Context, settings map[string]string, now time.Time) {
	if !r.maintenanceMu.TryLock() {
		return
	}
	defer r.maintenanceMu.Unlock()
	if !r.Ready() || ctx.Err() != nil {
		return
	}
	if settings["qoder_auto_checkin"] == "1" {
		window := checkin.WindowDate(now)
		if r.autoWindow != window {
			r.autoWindow = window
			r.autoAttempts = 0
			r.autoRetry = time.Time{}
		}
		if r.autoAttempts < 3 && !now.Before(r.autoRetry) {
			r.autoAttempts++
			result, err := r.AdminCheckin(ctx)
			complete := err == nil
			if rows, ok := result["results"].([]map[string]any); ok {
				for _, row := range rows {
					if row["status"] == "error" || row["status"] == "no_campaign" {
						complete = false
					}
				}
			}
			if complete {
				r.autoAttempts = 3
			}
			r.autoRetry = now.Add(time.Hour)
		}
	}
	interval, err := strconv.Atoi(settings["credit_refresh_min"])
	if err != nil || interval < 1 {
		interval = 30
	}
	if settings["qoder_auto_quota"] == "1" && now.Sub(r.lastQuotaMaintenance) >= time.Duration(interval)*time.Minute {
		r.lastQuotaMaintenance = now
		if accounts, err := account.List(); err == nil {
			for _, a := range accounts {
				if ctx.Err() != nil {
					return
				}
				if !account.IsGatewayHidden(a.ID) {
					r.refreshQuotaContext(ctx, a.ID, "native", string(a.Region))
				}
			}
		}
		for _, c := range r.detectLocal() {
			if ctx.Err() != nil {
				return
			}
			r.refreshQuotaContext(ctx, "qoder-local-"+c.Region, "local", c.Region)
		}
	}
}
