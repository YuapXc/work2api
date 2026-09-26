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

const expiryPriorityDays = 7.0

// Pick selects the next healthy account by weighted random. Phases (in order):
// soon-to-expire accounts win outright (use-it-or-lose-it credits); otherwise,
// when costByUID is provided, restrict to the cheapest cost group (unknown cost
// ranks last); then weighted random. costByUID nil = cost-blind (legacy).
// Falls back to the account whose cooldown expires soonest when none healthy.
func (p *Pool) Pick(allowed map[string]bool, costByUID map[string]float64) *Account {
	p.mu.Lock()
	defer p.mu.Unlock()
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
	var urgent []*Account
	for _, a := range candidates {
		if daysToExpiry(a, now) <= expiryPriorityDays {
			urgent = append(urgent, a)
		}
	}
	poolToPick := candidates
	applyIdle := true
	if len(urgent) > 0 {
		// 到期紧迫硬优先：先烧快过期的额度，此阶段不看成本（用完即废更急）。
		poolToPick = urgent
		applyIdle = false
	} else if costByUID != nil {
		// 无快到期账号时才做成本优先：精确分组取最低成本组，未知成本垫底。
		poolToPick = cheapestGroup(candidates, costByUID)
	}
	var totalW float64
	weights := make([]float64, len(poolToPick))
	for i, a := range poolToPick {
		w := weight(a, now, applyIdle)
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

func daysToExpiry(a *Account, now float64) float64 {
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
