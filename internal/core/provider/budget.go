package provider

import "math"

// BudgetPool describes one non-overlapping balance. Domains identify accounts
// whose actual consumption units and prices can be calibrated together.
// A coefficient is not a per-token price; only explicitly free models belong
// in FreeModels. Channels without trustworthy billing must give a Reason.
type BudgetPool struct {
	AccountID, Domain, Label, Unit string
	DomainLabel                    string
	ModelCosts                     map[string]float64 // catalog coefficients, not per-token prices
	Remaining                      *float64
	Models, FreeModels             []string
	Stale, Deferred                bool
	Excluded                       float64
	Reason                         string
	EstimateUnavailable            string
	ExpiresAt                      *float64
	Expiring                       float64
}

// CreditBudget builds a balance projection without changing dispatch policy.
// Temporary cooldowns preserve the budget but remain visible to the caller.
func CreditBudget(row map[string]any, domain, label, unit string, now float64) BudgetPool {
	id, _ := row["uid"].(string)
	if id == "" {
		id, _ = row["id"].(string)
	}
	p := BudgetPool{AccountID: id, Domain: domain, Label: label, Unit: unit,
		Remaining: resourceNumber(row["credits_remaining"]), Stale: row["quota_stale"] == true || row["quota_refresh_failed"] == true}
	switch {
	case row["enabled"] == false || row["has_secret"] == false || row["auth_invalid"] == true:
		p.Reason = "账号未启用或未授权"
	case row["quota_exceeded"] == true && (p.Remaining == nil || *p.Remaining != 0):
		p.Reason = "额度不可用"
	case row["healthy"] == false:
		if until := resourceNumber(row["cooldown_until"]); until != nil && *until > now {
			p.Deferred = true
		} else if p.Remaining == nil || *p.Remaining != 0 {
			p.Reason = "账号不可用"
		}
	}
	packages, _ := row["credit_packages"].([]map[string]any)
	if p.Remaining != nil && *p.Remaining < 0 {
		p.Remaining = nil
	}
	// Account summaries may show the earliest package expiry, not the expiry
	// of the entire balance. Prefer individual packages when available.
	if expiry := resourceNumber(row["credits_expire_at"]); len(packages) == 0 && expiry != nil && *expiry > 0 && *expiry <= now {
		if p.Remaining != nil {
			zero := float64(0)
			p.Excluded, p.Remaining = *p.Remaining, &zero
		}
	}
	if len(packages) > 0 && p.Remaining != nil {
		remaining := *p.Remaining
		for _, pkg := range packages {
			if pkg["display_only"] == true {
				continue
			}
			expiry, remain := resourceNumber(pkg["expire_at"]), resourceNumber(pkg["remain"])
			if remain == nil || *remain <= 0 {
				continue
			}
			switch {
			case pkg["budget_restricted"] == true || expiry != nil && *expiry > 0 && *expiry <= now:
				p.Excluded += *remain
			case expiry != nil && *expiry > now:
				if p.ExpiresAt == nil || *expiry < *p.ExpiresAt {
					p.ExpiresAt, p.Expiring = expiry, *remain
				} else if *expiry == *p.ExpiresAt {
					p.Expiring += *remain
				}
			}
		}
		remaining = math.Max(0, remaining-p.Excluded)
		p.Remaining = &remaining
		p.Expiring = math.Min(p.Expiring, remaining)
	} else if p.Remaining != nil && *p.Remaining > 0 {
		if expiry := resourceNumber(row["credits_expire_at"]); expiry != nil && *expiry > now {
			p.ExpiresAt, p.Expiring = expiry, *p.Remaining
		}
	}
	return p
}
