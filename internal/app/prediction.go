package app

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"

	"work2api/internal/core/provider"
	"work2api/internal/store"
)

type budgetEvidence struct {
	credits, tokens    float64
	calls, legacyCalls int
}

func (e *budgetEvidence) add(s store.BudgetSample) {
	e.credits += s.Credits
	e.tokens += s.Tokens
	e.calls += s.Calls
	e.legacyCalls += s.LegacyCalls
}

func (e budgetEvidence) good() bool {
	return e.calls >= 5 && e.tokens >= 1000 && finitePositive(e.credits) && finitePositive(e.tokens)
}
func finitePositive(n float64) bool { return n > 0 && !math.IsNaN(n) && !math.IsInf(n, 0) }

func budgetModel(providerName, model string) (string, bool) {
	if name, id, ok := strings.Cut(model, "/"); ok {
		return id, name == providerName
	}
	return model, providerName == "workbuddy"
}

func (s *Server) creditPrediction(data map[string]provider.AdminData, now float64) map[string]any {
	samples, err := s.o.db.UsageBudgetSamples(now)
	if err != nil {
		return map[string]any{"predicted_tokens": nil, "providers": []any{}, "error": "用量统计暂不可用"}
	}
	return estimateBudgets(data, samples)
}

type budgetGroup struct {
	domain, label, unit string
	paid                map[string]bool
	costs               map[string]float64
	accounts            []provider.BudgetPool
	unavailable         string
}

// Only balances with identical paid billing rules and paid model applicability
// are pooled. Extra free models do not consume this balance or split the pool.
// Catalog coefficients identify tariffs; they are never treated as token prices.
func groupedBudgets(name string, pools []provider.BudgetPool) []*budgetGroup {
	groups := map[string]*budgetGroup{}
	seen := map[string]bool{}
	for _, p := range pools {
		if p.AccountID == "" || seen[p.AccountID] {
			continue
		}
		seen[p.AccountID] = true
		models, free, costs := map[string]bool{}, map[string]bool{}, map[string]float64{}
		for _, model := range p.Models {
			if id, ok := budgetModel(name, model); ok {
				models[id] = true
			}
		}
		for _, model := range p.FreeModels {
			if id, ok := budgetModel(name, model); ok && models[id] {
				free[id] = true
			}
		}
		for model, cost := range p.ModelCosts {
			if id, ok := budgetModel(name, model); ok && models[id] && cost >= 0 && !math.IsNaN(cost) && !math.IsInf(cost, 0) {
				costs[id] = cost
			}
		}
		paidModels, paidCosts := map[string]bool{}, map[string]float64{}
		for model := range models {
			if !free[model] {
				paidModels[model] = true
				if cost, ok := costs[model]; ok {
					paidCosts[model] = cost
				}
			}
		}
		signature, _ := json.Marshal([]any{p.Domain, p.Unit, paidModels, paidCosts, p.EstimateUnavailable})
		key := string(signature)
		if p.Domain == "" {
			key += "\x00" + p.AccountID
		}
		g := groups[key]
		if g == nil {
			label := p.DomainLabel
			if label == "" {
				label = p.Domain
			}
			if label == "" {
				label = p.Label
			}
			g = &budgetGroup{domain: p.Domain, label: label, unit: p.Unit, paid: paidModels, costs: paidCosts, unavailable: p.EstimateUnavailable}
			groups[key] = g
		}
		g.accounts = append(g.accounts, p)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	labels := map[string]int{}
	for _, key := range keys {
		labels[groups[key].label]++
	}
	indexes := map[string]int{}
	out := make([]*budgetGroup, 0, len(keys))
	for _, key := range keys {
		g := groups[key]
		if labels[g.label] > 1 {
			indexes[g.label]++
			g.label += " · 计费组 " + strconv.Itoa(indexes[g.label])
		}
		out = append(out, g)
	}
	return out
}

type budgetRate struct {
	domain, unit, model string
	coefficient         float64
}

type budgetRateEvidence struct{ recent, total budgetEvidence }

// Share only stamped prices within the same billing domain and unit. This
// widens the sample set without letting a restricted balance fund other models.
func budgetRates(name string, groups []*budgetGroup, samples map[string][]store.BudgetSample) map[budgetRate]budgetRateEvidence {
	rates := map[budgetRate]budgetRateEvidence{}
	for _, group := range groups {
		if group.domain == "" || group.unavailable != "" {
			continue
		}
		for _, p := range group.accounts {
			if p.Reason != "" {
				continue
			}
			for _, s := range samples[p.AccountID] {
				id, ok := budgetModel(name, s.Model)
				cost, known := group.costs[id]
				if !ok || !known || s.Domain != group.domain || s.Coefficient == nil || *s.Coefficient != cost ||
					!finitePositive(s.Credits) || !finitePositive(s.Tokens) {
					continue
				}
				key := budgetRate{group.domain, group.unit, id, cost}
				e := rates[key]
				e.total.add(s)
				if s.Recent {
					e.recent.add(s)
				}
				rates[key] = e
			}
		}
	}
	return rates
}

func estimateBudgetGroup(name string, g *budgetGroup, samples map[string][]store.BudgetSample, rates map[budgetRate]budgetRateEvidence) (map[string]any, []string) {
	row := map[string]any{"label": g.label, "unit": g.unit, "remaining": nil, "predicted_tokens": nil, "status": "unknown", "reason": "", "stale": false, "deferred": false, "excluded_credits": float64(0), "window_days": 7, "samples": 0, "legacy_samples": 0, "scope": "billing_domain", "account_count": len(g.accounts)}
	accounts := []map[string]any{}
	var balance, excluded float64
	knownBalances, eligibleAccounts := 0, 0
	freeSet := map[string]bool{}
	byModel := map[string][]store.BudgetSample{}
	var recentDemand, allDemand float64
	for _, p := range g.accounts {
		eligible := p.Reason == ""
		knownBalance := p.Remaining != nil && *p.Remaining >= 0 && !math.IsNaN(*p.Remaining) && !math.IsInf(*p.Remaining, 0)
		accounts = append(accounts, map[string]any{"label": p.Label, "remaining": p.Remaining, "reason": p.Reason, "stale": p.Stale, "deferred": p.Deferred, "included": eligible && knownBalance, "expires_at": p.ExpiresAt, "expiring": p.Expiring})
		if !eligible {
			continue
		}
		eligibleAccounts++
		row["deferred"] = row["deferred"].(bool) || p.Deferred
		if finitePositive(p.Excluded) {
			excluded += p.Excluded
		}
		if !p.Deferred {
			for _, model := range p.FreeModels {
				if id, ok := budgetModel(name, model); ok {
					freeSet[id] = true
				}
			}
		}
		if knownBalance {
			balance += *p.Remaining
			knownBalances++
			row["stale"] = row["stale"].(bool) || p.Stale
		}
		for _, s := range samples[p.AccountID] {
			id, ok := budgetModel(name, s.Model)
			demand := s.Tokens + s.UnknownTokens
			if !ok || !g.paid[id] || s.Domain != "" && s.Domain != g.domain || !finitePositive(demand) {
				continue
			}
			byModel[id] = append(byModel[id], s)
			allDemand += demand
			if s.Recent {
				recentDemand += demand
			}
		}
	}
	row["accounts"], row["known_balances"], row["eligible_accounts"], row["excluded_credits"] = accounts, knownBalances, eligibleAccounts, excluded
	if knownBalances > 0 && (finitePositive(balance) || balance == 0) {
		row["remaining"] = balance
	}
	recentMix := recentDemand > 0
	totalDemand := recentDemand
	if !recentMix {
		totalDemand = allDemand
		row["window_days"] = 30
	}
	modelIDs := make([]string, 0, len(byModel))
	for id := range byModel {
		modelIDs = append(modelIDs, id)
	}
	sort.Strings(modelIDs)
	details := []map[string]any{}
	missing := []string{}
	aggregate := budgetEvidence{}
	var weightedCost, weakCost, weakShare float64
	for _, id := range modelIDs {
		items := byModel[id]
		current, hasPrice := g.costs[id]
		changed := false
		currentEvidence := budgetEvidence{}
		for _, s := range items {
			if hasPrice && s.Coefficient != nil && *s.Coefficient != current {
				changed = true
			}
			if hasPrice && s.Coefficient != nil && *s.Coefficient == current && finitePositive(s.Credits) && finitePositive(s.Tokens) {
				currentEvidence.add(s)
			}
		}
		// Do not erase a useful historical estimate after the first stamped
		// request. Replace unversioned evidence once the new evidence is sound,
		// or immediately when an actual conflicting tariff is known.
		stamped := currentEvidence.good()
		evidence := func(recent bool) budgetEvidence {
			e := budgetEvidence{}
			for _, s := range items {
				if recent && !s.Recent || !finitePositive(s.Credits) || !finitePositive(s.Tokens) {
					continue
				}
				if s.Coefficient != nil && (!hasPrice || *s.Coefficient != current) || s.Coefficient == nil && (changed || stamped) {
					continue
				}
				e.add(s)
			}
			return e
		}
		var demand float64
		for _, s := range items {
			if !recentMix || s.Recent {
				demand += s.Tokens + s.UnknownTokens
			}
		}
		if demand <= 0 {
			continue
		}
		share := demand / totalDemand
		e := evidence(true)
		days := 7
		if !e.good() {
			e = evidence(false)
			days = 30
		}
		if !e.good() && hasPrice {
			peer := rates[budgetRate{g.domain, g.unit, id, current}]
			if peer.recent.good() {
				e, days = peer.recent, 7
			} else if peer.total.good() {
				e, days = peer.total, 30
			}
		}
		detail := map[string]any{"model": id, "token_share": share, "credits_per_million_tokens": nil, "samples": e.calls, "window_days": days}
		if finitePositive(e.credits) && finitePositive(e.tokens) {
			cost := e.credits / e.tokens
			weightedCost += share * cost
			if !e.good() {
				weakCost += share * cost
				weakShare += share
			}
			detail["credits_per_million_tokens"] = cost * 1e6
			aggregate.credits += e.credits
			aggregate.tokens += e.tokens
			aggregate.calls += e.calls
			aggregate.legacyCalls += e.legacyCalls
			if days == 30 {
				row["window_days"] = 30
			}
		} else {
			missing = append(missing, id)
		}
		details = append(details, detail)
	}
	row["models"], row["missing_models"], row["samples"], row["legacy_samples"] = details, missing, aggregate.calls, aggregate.legacyCalls
	switch {
	case eligibleAccounts == 0:
		row["status"] = "unavailable"
		row["reason"] = g.accounts[0].Reason
	case len(g.paid) == 0 && g.unavailable == "" && len(freeSet) > 0:
		row["status"] = "free"
		row["reason"] = "仅有免费模型"
	case knownBalances == 0 && g.unavailable != "":
		row["reason"] = g.unavailable
	case knownBalances == 0:
		row["reason"] = "余额未知"
	case !finitePositive(balance) && balance != 0:
		row["reason"] = "余额超出可估算范围"
	case balance == 0 && knownBalances == eligibleAccounts:
		row["status"] = "empty"
		row["predicted_tokens"] = int64(0)
		row["reason"] = "没有可估算的付费余额"
	case balance == 0:
		row["reason"] = "部分账号余额未知"
	case g.unavailable != "":
		row["reason"] = g.unavailable
	case len(missing) > 0:
		row["reason"] = "部分近期调用模型缺少当前费率的消费依据"
	case !aggregate.good() || !finitePositive(weightedCost):
		row["reason"] = "有效付费样本不足（至少5次、1000 Tokens）"
	case weakShare >= .5 || weakCost >= weightedCost*.5:
		row["reason"] = "主要模型的有效付费样本不足"
	default:
		value := balance / weightedCost
		if !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value < float64(math.MaxInt64) {
			row["predicted_tokens"] = int64(value)
			row["status"] = "estimated"
			row["tokens_per_unit"] = 1 / weightedCost
		} else {
			row["reason"] = "超出可估算范围"
		}
	}
	free := []string{}
	for id := range freeSet {
		free = append(free, id)
	}
	sort.Strings(free)
	return row, free
}

// Sum token estimates, never nominal credits across channels or distinct tariffs.
func estimateBudgets(data map[string]provider.AdminData, samples []store.BudgetSample) map[string]any {
	providers := []map[string]any{}
	byAccount := make(map[string][]store.BudgetSample)
	for _, sample := range samples {
		byAccount[sample.AccountUID] = append(byAccount[sample.AccountUID], sample)
	}
	names := make([]string, 0, len(data))
	for name := range data {
		names = append(names, name)
	}
	sort.Strings(names)
	var total int64
	estimated, poolCount := 0, 0
	hasEmpty, incomplete := false, false
	for _, name := range names {
		d := data[name]
		pools := []map[string]any{}
		freeSet := map[string]bool{}
		var subtotal int64
		covered := 0
		channelEmpty, channelIncomplete := false, false
		groups := groupedBudgets(name, d.Budgets)
		rates := budgetRates(name, groups, byAccount)
		for _, g := range groups {
			poolCount++
			row, free := estimateBudgetGroup(name, g, byAccount, rates)
			for _, id := range free {
				freeSet[id] = true
			}
			switch row["status"] {
			case "estimated":
				value := row["predicted_tokens"].(int64)
				if value <= math.MaxInt64-total && value <= math.MaxInt64-subtotal {
					total += value
					subtotal += value
					covered++
					estimated++
					channelIncomplete = channelIncomplete || row["known_balances"] != row["eligible_accounts"]
				} else {
					row["predicted_tokens"] = nil
					row["status"] = "unknown"
					row["reason"] = "超出可估算范围"
					channelIncomplete = true
				}
			case "empty":
				channelEmpty = true
			case "unknown":
				channelIncomplete = true
			}
			pools = append(pools, row)
		}
		free := []string{}
		for id := range freeSet {
			free = append(free, id)
		}
		sort.Strings(free)
		row := map[string]any{"provider": name, "label": d.DisplayName, "predicted_tokens": nil, "pools": pools, "free_models": free, "estimated_pools": covered}
		if len(pools) == 0 {
			row["reason"] = "余额或计费依据未知"
			channelIncomplete = d.Ready
		}
		if covered > 0 || channelEmpty && !channelIncomplete {
			row["predicted_tokens"] = subtotal
		}
		row["partial"] = covered > 0 && channelIncomplete
		hasEmpty = hasEmpty || channelEmpty
		incomplete = incomplete || channelIncomplete
		providers = append(providers, row)
	}
	out := map[string]any{"predicted_tokens": nil, "providers": providers, "estimated_pools": estimated, "total_pools": poolCount, "partial": estimated > 0 && incomplete}
	if estimated > 0 || hasEmpty && !incomplete {
		out["predicted_tokens"] = total
	}
	return out
}
