package qoder

import (
	"context"
	"crypto/sha256"
	"errors"
	"net"
	"strings"
	"time"

	"work2api/internal/core/provider"
	"work2api/internal/qoder/account"
	"work2api/internal/qoder/bridge"
)

type accountModelSnapshot struct {
	models  map[string]bool
	expires time.Time
}
type servingAccount struct {
	account *account.Account
	secret  string
	bridge  *bridge.Bridge
}

// A request/permission/content error cannot be repaired by consuming another
// account. Local DNS and client cancellation must not cool healthy accounts.
func canFailover(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return false
	}
	var upstream *bridge.UpstreamError
	if errors.As(err, &upstream) {
		if bridge.IsContentPolicy(upstream.Detail) {
			return false
		}
		return upstream.Status == 429 || bridge.IsTransientUpstream(upstream.Status, upstream.Detail)
	}
	return bridge.IsTransientTransport(err)
}

type accountAffinity struct {
	id                  string
	preferred, identity [32]byte
	expires             time.Time
}

func requestAffinityKey(req provider.ServeRequest) ([32]byte, bool) {
	session := ""
	for _, name := range []string{"X-Session-ID", "Session-ID", "X-Conversation-ID", "Conversation-ID"} {
		if value := strings.TrimSpace(req.Headers.Get(name)); value != "" {
			session = value
			break
		}
	}
	if session == "" {
		return [32]byte{}, false
	}
	// Scope untrusted session names to their authenticated key and model.
	scope := req.Headers.Get("Authorization") + "\x00" + req.Headers.Get("x-api-key") + "\x00" + req.AppName
	return sha256.Sum256([]byte(scope + "\x00" + str(req.Payload["model"]) + "\x00" + session)), true
}
func (r *Runtime) rememberAffinity(key [32]byte, enabled bool, preferred [32]byte, acct *account.Account, secret string) {
	if !enabled {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.accountAffinities == nil {
		r.accountAffinities = make(map[[32]byte]accountAffinity)
	}
	now := time.Now()
	for id, value := range r.accountAffinities {
		if !value.expires.After(now) {
			delete(r.accountAffinities, id)
		}
	}
	if _, exists := r.accountAffinities[key]; !exists && len(r.accountAffinities) >= 256 {
		var oldest [32]byte
		var expires time.Time
		for id, value := range r.accountAffinities {
			if expires.IsZero() || value.expires.Before(expires) {
				oldest, expires = id, value.expires
			}
		}
		delete(r.accountAffinities, oldest)
	}
	r.accountAffinities[key] = accountAffinity{acct.ID, preferred, modelCatalogKey(acct, secret), now.Add(30 * time.Minute)}
}
func (r *Runtime) affinityAccount(key [32]byte, enabled bool, preferred [32]byte) (*account.Account, string) {
	if !enabled {
		return nil, ""
	}
	r.mu.Lock()
	entry := r.accountAffinities[key]
	r.mu.Unlock()
	if entry.preferred != preferred || !entry.expires.After(time.Now()) || account.IsGatewayHidden(entry.id) {
		return nil, ""
	}
	if strings.HasPrefix(entry.id, "qoder-local-") {
		for _, local := range r.detectLocal() {
			acct := &account.Account{ID: "qoder-local-" + local.Region, Region: account.NormalizeRegion(local.Region)}
			if acct.ID == entry.id && modelCatalogKey(acct, local.DeviceToken) == entry.identity {
				return acct, local.DeviceToken
			}
		}
		return nil, ""
	}
	acct, e := account.Get(entry.id)
	secret, se := account.GetSecret(entry.id)
	if e != nil || se != nil || acct == nil || modelCatalogKey(acct, secret) != entry.identity {
		return nil, ""
	}
	return acct, secret
}

func (r *Runtime) coolAccount(acct *account.Account, secret string, cause error) {
	duration := 15 * time.Second
	var upstream *bridge.UpstreamError
	if errors.As(cause, &upstream) && upstream.Status == 429 {
		duration = time.Minute
		if upstream.RetryAfter > 0 {
			duration = upstream.RetryAfter
		}
	}
	key := modelCatalogKey(acct, secret)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.accountCooldowns == nil {
		r.accountCooldowns = make(map[[32]byte]time.Time)
	}
	now := time.Now()
	for id, until := range r.accountCooldowns {
		if !until.After(now) {
			delete(r.accountCooldowns, id)
		}
	}
	until := now.Add(duration)
	if until.After(r.accountCooldowns[key]) {
		r.accountCooldowns[key] = until
	}
}

// Failover never changes the active flag or region, and only selects a model
// explicitly advertised by the candidate account. Unknown catalogs are skipped.
func (r *Runtime) nextAccountBridge(ctx context.Context, region account.Region, model string, tried map[string]bool) (*servingAccount, error) {
	accounts, err := account.List()
	if err != nil {
		return nil, err
	}
	for _, local := range r.detectLocal() {
		accounts = append(accounts, account.Account{ID: "qoder-local-" + local.Region, Region: account.NormalizeRegion(local.Region)})
	}
	for i := range accounts {
		acct := &accounts[i]
		if len(tried) >= 3 {
			break
		}
		if tried[acct.ID] || account.NormalizeRegion(string(acct.Region)) != account.NormalizeRegion(string(region)) || account.IsGatewayHidden(acct.ID) {
			continue
		}
		secret, e := account.GetSecret(acct.ID)
		if e != nil || secret == "" {
			for _, local := range r.detectLocal() {
				if acct.ID == "qoder-local-"+local.Region {
					secret = local.DeviceToken
					break
				}
			}
		}
		if secret == "" {
			continue
		}
		key := modelCatalogKey(acct, secret)
		r.mu.Lock()
		cool := time.Now().Before(r.accountCooldowns[key])
		snapshot := r.accountModels[key]
		r.mu.Unlock()
		if cool {
			continue
		}
		tried[acct.ID] = true
		probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		b, e := r.bridgeFor(probeCtx, acct, secret)
		if e == nil && !snapshot.expires.After(time.Now()) {
			var models []bridge.QoderModel
			models, e = b.ListAvailableModelsContext(probeCtx)
			if e == nil {
				snapshot = accountModelSnapshot{models: make(map[string]bool), expires: time.Now().Add(5 * time.Minute)}
				for _, m := range models {
					if m.Enable {
						snapshot.models[m.Key] = true
					}
				}
				r.mu.Lock()
				if r.accountModels == nil {
					r.accountModels = make(map[[32]byte]accountModelSnapshot)
				}
				// Bound cache cardinality across credential reimports.
				for id, value := range r.accountModels {
					if !value.expires.After(time.Now()) {
						delete(r.accountModels, id)
					}
				}
				r.accountModels[key] = snapshot
				r.mu.Unlock()
			}
		}
		cancel()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if e != nil || !snapshot.models[model] || account.IsGatewayHidden(acct.ID) {
			continue
		}
		// Re-read native identity after network waits: deletion/reimport must
		// not revive a previously captured credential through failover.
		if !strings.HasPrefix(acct.ID, "qoder-local-") {
			current, e := account.Get(acct.ID)
			currentSecret, se := account.GetSecret(acct.ID)
			if e != nil || se != nil || current == nil || modelCatalogKey(current, currentSecret) != key {
				continue
			}
		}
		return &servingAccount{acct, secret, b}, nil
	}
	return nil, nil
}
