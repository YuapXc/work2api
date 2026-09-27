// Package pool is workbuddy's multi-account pool: weighted rotation + cooldown
// + credit awareness. Ported from workbuddy_one/pool.py. Under the shared-core
// design this is workbuddy's own account-selection strategy; cooldown/failure
// bookkeeping are the shared primitives it builds on.
package pool

import (
	"math"
	"math/rand"
	"strings"
	"sync"
	"time"

	"work2api/internal/workbuddy/siterouting"
)

// Credential is the minimal interface the pool needs from an account's
// credential manager.
type Credential interface {
	Profile() string
	Summary() map[string]any
	Path() string
}

// Account is one pooled account.
type Account struct {
	UID      string
	Mgr      Credential
	Provider string
	Enabled  bool
	Profile  string

	DisabledReason string
	Alias          string
	Nickname       string

	CooldownUntil   float64
	FailureCount    int
	CreditsRemain   *float64
	CreditsTotal    *float64
	CreditsExpireAt *float64
	CreditPackages  []map[string]any
	Priority        int
	LastUsed        float64
}

func safeProfile(mgr Credential) string {
	if mgr == nil {
		return siterouting.DefaultProfile
	}
	p := mgr.Profile()
	if p == "" {
		return siterouting.DefaultProfile
	}
	return p
}

func newAccount(uid string, mgr Credential) *Account {
	return &Account{
		UID:      uid,
		Mgr:      mgr,
		Provider: "workbuddy",
		Enabled:  true,
		Profile:  safeProfile(mgr),
	}
}

// Healthy reports whether the account can serve a request now.
func (a *Account) Healthy(now float64) bool {
	if !a.Enabled {
		return false
	}
	if a.CooldownUntil > now {
		return false
	}
	if a.CreditsRemain != nil && *a.CreditsRemain <= 0 {
		return false
	}
	return true
}

func (a *Account) markFailure(cooldownSeconds float64) {
	a.FailureCount++
	a.CooldownUntil = float64(time.Now().UnixNano())/1e9 + cooldownSeconds
}

func (a *Account) markSuccess() {
	a.FailureCount = 0
	a.LastUsed = float64(time.Now().UnixNano()) / 1e9
}

// Pool holds the accounts.
type Pool struct {
	mu           sync.Mutex
	accounts     []*Account
	projectAuths string
}

// New builds a pool from uid→credential managers.
func New(managers map[string]Credential, projectAuths string) *Pool {
	p := &Pool{projectAuths: projectAuths}
	for uid, mgr := range managers {
		p.accounts = append(p.accounts, newAccount(uid, mgr))
	}
	return p
}

// Count returns the number of accounts.
func (p *Pool) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.accounts)
}

// HealthyCount returns the number of healthy accounts (optionally filtered).
func (p *Pool) HealthyCount(allowed map[string]bool) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := nowSec()
	n := 0
	for _, a := range p.accounts {
		if (allowed == nil || allowed[a.UID]) && a.Healthy(now) {
			n++
		}
	}
	return n
}

// DefaultExpiryWindowDays is the look-ahead window for "soon-to-expire" credit
// prioritization when a caller passes 0 (cost-blind / non-routing picks).
const DefaultExpiryWindowDays = 7.0

// expiryAmountBias controls how strongly the soon-to-expire group prefers the
// account with the larger absolute in-window expiring balance: the biggest pile
// gets weight ×(1+bias), scaling linearly to ×1 for an empty pile. Kept moderate
// so it steers burn order without fully overriding priority/failure weighting.
const expiryAmountBias = 2.0

// Pick selects the next healthy account by weighted random. Phases (in order):
// when costByUID is provided, first restrict to the cheapest cost group (cost
// is the top priority — a cheaper account is chosen even over a soon-to-expire
// costlier one, so the latter's expiring credits may go unused; unknown cost
// ranks last). Within that group, accounts with credits expiring inside
// expiryWindowDays win (burn expiring credits among same-cost peers), and among
// those the larger absolute in-window expiring balance is preferred (Option A
// soft bias); then weighted random. costByUID nil = cost-blind (legacy).
// expiryWindowDays <= 0 falls back to DefaultExpiryWindowDays. Falls back to the
// soonest-cooldown account when none healthy.
func (p *Pool) Pick(allowed map[string]bool, costByUID map[string]float64, expiryWindowDays float64) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	if expiryWindowDays <= 0 {
		expiryWindowDays = DefaultExpiryWindowDays
	}
	now := nowSec()
	var candidates []*Account
	for _, a := range p.accounts {
		if (allowed == nil || allowed[a.UID]) && a.Healthy(now) {
			candidates = append(candidates, a)
		}
	}
	if len(candidates) == 0 {
		var best *Account
		bestExpiry := math.Inf(1)
		for _, a := range p.accounts {
			if (allowed == nil || allowed[a.UID]) && a.Enabled && a.CooldownUntil < bestExpiry {
				best = a
				bestExpiry = a.CooldownUntil
			}
		}
		if best != nil {
			best.LastUsed = now
		}
		return best
	}
	// 成本绝对优先：先把候选收敛到最低成本组（未知成本垫底），到期紧迫只在组内起作用。
	if costByUID != nil {
		candidates = cheapestGroup(candidates, costByUID)
	}
	var urgent []*Account
	for _, a := range candidates {
		if daysToExpiry(a, now) <= expiryWindowDays {
			urgent = append(urgent, a)
		}
	}
	poolToPick := candidates
	applyIdle := true
	if len(urgent) > 0 {
		// 同成本组内先烧快过期的额度（此阶段不叠加闲置补偿，避免抵消优先意图）。
		poolToPick = urgent
		applyIdle = false
	}
	// Option A：紧迫组内按「窗口内绝对到期余额」偏置，优先烧最大的一堆；用组内最大值
	// 归一化，避免绝对量级压垮其它权重因子。非紧迫组不参与（maxExpiring 保持 0）。
	var maxExpiring float64
	if !applyIdle {
		for _, a := range poolToPick {
			if e := a.expiringWithin(now, expiryWindowDays); e > maxExpiring {
				maxExpiring = e
			}
		}
	}
	var totalW float64
	weights := make([]float64, len(poolToPick))
	for i, a := range poolToPick {
		w := weight(a, now, applyIdle)
		if maxExpiring > 0 {
			w *= 1 + expiryAmountBias*(a.expiringWithin(now, expiryWindowDays)/maxExpiring)
		}
		weights[i] = w
		totalW += w
	}
	pick := rand.Float64() * totalW
	acc := poolToPick[len(poolToPick)-1]
	for i, a := range poolToPick {
		pick -= weights[i]
		if pick <= 0 {
			acc = a
			break
		}
	}
	acc.LastUsed = now
	return acc
}

// cheapestGroup returns the candidates sharing the lowest known per-model cost.
// Accounts with unknown cost (not in costByUID) rank last: they are excluded
// whenever at least one candidate has a known cost. If no candidate has a known
// cost, the full set is returned unfiltered (soft fallback, never empties).
func cheapestGroup(cands []*Account, cost map[string]float64) []*Account {
	const eps = 1e-9
	minCost := math.Inf(1)
	for _, a := range cands {
		if c, ok := cost[a.UID]; ok && c < minCost {
			minCost = c
		}
	}
	if math.IsInf(minCost, 1) {
		return cands // 全部未知成本 → 不过滤
	}
	var group []*Account
	for _, a := range cands {
		if c, ok := cost[a.UID]; ok && c <= minCost+eps {
			group = append(group, a)
		}
	}
	if len(group) == 0 {
		return cands
	}
	return group
}

// pkgFloat reads a numeric field from a credit-package map, tolerating the
// float64/*float64/int shapes the billing layer and JSON round-trips produce.
func pkgFloat(p map[string]any, key string) (float64, bool) {
	switch v := p[key].(type) {
	case float64:
		return v, true
	case *float64:
		if v != nil {
			return *v, true
		}
	case int:
		return float64(v), true
	}
	return 0, false
}

// nearestExpiry returns the soonest real per-package expiry among packages that
// still hold a positive balance, or nil when package data is absent/none apply.
// This is the truthful "credits will actually vanish" time. The account-level
// CreditsExpireAt is now derived from the same per-package data (see billing
// earliestPackageExpiry), so daysToExpiry's fallback to it stays consistent;
// nearestExpiry is still preferred because it needs no stored round-trip and
// pairs with expiringWithin for the in-window amount.
func (a *Account) nearestExpiry() *float64 {
	var best *float64
	for _, p := range a.CreditPackages {
		if rem, ok := pkgFloat(p, "remain"); !ok || rem <= 0 {
			continue
		}
		exp, ok := pkgFloat(p, "expire_at")
		if !ok || exp <= 0 {
			continue
		}
		if best == nil || exp < *best {
			e := exp
			best = &e
		}
	}
	return best
}

// expiringWithin sums the balance of packages expiring within windowDays from
// now — the absolute amount at risk of going unused, driving burn priority.
func (a *Account) expiringWithin(now, windowDays float64) float64 {
	if len(a.CreditPackages) == 0 || windowDays <= 0 {
		return 0
	}
	limit := now + windowDays*86400
	var sum float64
	for _, p := range a.CreditPackages {
		exp, ok := pkgFloat(p, "expire_at")
		if !ok || exp <= 0 || exp > limit {
			continue
		}
		if rem, ok := pkgFloat(p, "remain"); ok && rem > 0 {
			sum += rem
		}
	}
	return sum
}

// daysToExpiry prefers the truthful per-package nearest expiry; only when no
// package data is available yet (e.g. before the first credit refresh) does it
// fall back to the account-level cycle boundary.
func daysToExpiry(a *Account, now float64) float64 {
	if e := a.nearestExpiry(); e != nil {
		return (*e - now) / 86400.0
	}
	if a.CreditsExpireAt == nil || *a.CreditsExpireAt == 0 {
		return math.Inf(1)
	}
	return (*a.CreditsExpireAt - now) / 86400.0
}

func weight(a *Account, now float64, applyIdle bool) float64 {
	p := float64(1 + max0(a.Priority))
	c := 1.0
	if a.CreditsTotal != nil && *a.CreditsTotal != 0 {
		remain := 0.0
		if a.CreditsRemain != nil && *a.CreditsRemain > 0 {
			remain = *a.CreditsRemain
		}
		ratio := remain / *a.CreditsTotal
		c = 0.5 + 0.5*ratio
	}
	e := 1.0
	days := daysToExpiry(a, now)
	switch {
	case days <= 0:
		e = 8.0
	case days <= 1:
		e = 8.0
	case days <= 3:
		e = 6.0
	case days <= 7:
		e = 4.0
	case days <= 30:
		e = 2.0
	}
	s := 1.0 / (1.0 + float64(a.FailureCount))
	i := 1.0
	if applyIdle {
		idleH := 0.0
		if a.LastUsed > 0 {
			idleH = (now - a.LastUsed) / 3600.0
		}
		i = math.Min(1.0+idleH*0.3, 3.0)
	}
	return p * c * e * s * i
}

// ChatURLFor returns the chat entry URL for an account.
func (p *Pool) ChatURLFor(uid string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.UID == uid {
			url, _ := siterouting.ChatURLForProfile(a.Profile)
			return url
		}
	}
	url, _ := siterouting.ChatURLForProfile(siterouting.DefaultProfile)
	return url
}

// SetEnabled toggles an account, recording the disable reason.
func (p *Pool) SetEnabled(uid string, enabled bool, reason string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.UID == uid {
			a.Enabled = enabled
			if enabled {
				a.DisabledReason = ""
			} else if reason != "" {
				a.DisabledReason = reason
			} else {
				a.DisabledReason = "已禁用"
			}
			return
		}
	}
}

// SetAlias sets a display alias.
func (p *Pool) SetAlias(uid, alias string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.UID == uid {
			a.Alias = strings.TrimSpace(alias)
			return
		}
	}
}

// SetCredits updates an account's credit snapshot.
func (p *Pool) SetCredits(uid string, remain, total, expireAt *float64, packages []map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.UID == uid {
			a.CreditsRemain = remain
			a.CreditsTotal = total
			a.CreditsExpireAt = expireAt
			if packages != nil {
				a.CreditPackages = packages
			}
			return
		}
	}
}

// SetPriority sets an account's priority.
func (p *Pool) SetPriority(uid string, priority int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.UID == uid {
			a.Priority = priority
			return
		}
	}
}

// ClearCooldown clears cooldown/failure for an account (never revives a
// manually disabled one).
func (p *Pool) ClearCooldown(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.UID == uid {
			a.CooldownUntil = 0
			a.FailureCount = 0
			return
		}
	}
}

// AddAccount registers an account, hot-updating credentials if it exists.
// Returns true if newly added.
func (p *Pool) AddAccount(uid string, mgr Credential) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.UID == uid {
			a.Mgr = mgr
			a.Profile = safeProfile(mgr)
			if s := mgr.Summary(); s != nil {
				if nick, ok := s["nickname"].(string); ok {
					a.Nickname = nick
				}
			}
			return false
		}
	}
	p.accounts = append(p.accounts, newAccount(uid, mgr))
	return true
}

// RemoveAccount removes an account.
func (p *Pool) RemoveAccount(uid string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, a := range p.accounts {
		if a.UID == uid {
			p.accounts = append(p.accounts[:i], p.accounts[i+1:]...)
			return true
		}
	}
	return false
}

// OnSuccess marks an account's request as successful.
func (p *Pool) OnSuccess(uid string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.UID == uid {
			a.markSuccess()
			return
		}
	}
}

// OnFailure marks an account's request as failed and cools it down.
func (p *Pool) OnFailure(uid string, cooldownSeconds float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.UID == uid {
			a.markFailure(cooldownSeconds)
			return
		}
	}
}

// AllAccounts returns display snapshots of all accounts.
func (p *Pool) AllAccounts() []map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := nowSec()
	out := make([]map[string]any, 0, len(p.accounts))
	for _, a := range p.accounts {
		out = append(out, map[string]any{
			"uid":               a.UID,
			"enabled":           a.Enabled,
			"disabled_reason":   a.DisabledReason,
			"alias":             a.Alias,
			"nickname":          a.Nickname,
			"healthy":           a.Healthy(now),
			"profile":           a.Profile,
			"provider":          a.Provider,
			"site":              siterouting.ProfileSite(a.Profile),
			"cooldown_until":    a.CooldownUntil,
			"failure_count":     a.FailureCount,
			"credits_remaining": deref(a.CreditsRemain),
			"credits_total":     deref(a.CreditsTotal),
			"credits_expire_at": deref(a.CreditsExpireAt),
			"credit_packages":   a.CreditPackages,
			"priority":          a.Priority,
			"weight":            round3(weight(a, now, true)),
			"source":            p.accountSource(a),
		})
	}
	return out
}

func (p *Pool) accountSource(a *Account) string {
	if a.Provider == "qoder" {
		return "qoder"
	}
	if a.Mgr == nil {
		return "unknown"
	}
	if p.projectAuths != "" && strings.HasPrefix(a.Mgr.Path(), p.projectAuths) {
		return "project"
	}
	return "local"
}

// Accounts returns the raw account list (caller must not mutate).
func (p *Pool) Accounts() []*Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*Account, len(p.accounts))
	copy(out, p.accounts)
	return out
}

// Get returns the account with the given UID, or nil. Avoids copying the whole
// slice when the caller only needs one account (e.g. session-sticky lookup).
func (p *Pool) Get(uid string) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.UID == uid {
			return a
		}
	}
	return nil
}

func nowSec() float64 { return float64(time.Now().UnixNano()) / 1e9 }
func max0(n int) int {
	if n > 0 {
		return n
	}
	return 0
}
func round3(f float64) float64 { return math.Round(f*1000) / 1000 }
func deref(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}
