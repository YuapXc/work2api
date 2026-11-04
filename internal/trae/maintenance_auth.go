package trae

import (
	"context"
	"errors"
	"path/filepath"
	"work2api/internal/core/provider"
	"work2api/internal/trae/auth"
	"work2api/internal/trae/upstream"
)

func (r *Runtime) authGuard() *provider.MaintenanceGuard {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.maintenanceGuard == nil {
		path := ""
		if r.dir != "" {
			path = filepath.Join(r.dir, ".maintenance.json")
		}
		r.maintenanceGuard = provider.NewMaintenanceGuard(path)
	}
	return r.maintenanceGuard
}
func (r *Runtime) observeMaintenance(a *auth.Auth, scope, fingerprint string, err error) {
	currentIdentity, readable := a.TryMaintenanceIdentity()
	if errors.Is(err, context.Canceled) || !readable || currentIdentity != fingerprint {
		return
	}
	r.mu.Lock()
	current := r.accounts[accountID(a.UID)]
	valid := current != nil && current.Identities[a.Variant] == a
	r.mu.Unlock()
	if !valid {
		return
	}
	g := r.authGuard()
	key := accountID(a.UID) + ":" + a.Variant + ":" + scope
	g.State(key, fingerprint)
	kind := ""
	hard := false
	if err != nil {
		kind = "temporary"
		var refresh *credentialRefreshError
		if errors.As(err, &refresh) && refresh.requiresLogin() {
			kind = "authorization_expired"
			hard = true
		}
		var remote *upstream.Error
		if errors.As(err, &remote) && remote.Status == 401 {
			kind = "authorization_unverified"
			if scope == "auth" {
				hard = true
			}
		}
	}
	g.Observe(key, fingerprint, kind, hard)
	var rejected *upstream.Error
	if scope != "auth" && errors.As(err, &rejected) && rejected.Status == 401 {
		authKey := accountID(a.UID) + ":" + a.Variant + ":auth"
		g.State(authKey, fingerprint)
		g.Observe(authKey, fingerprint, "authorization_unverified", false)
	}
}
func (r *Runtime) ensureMaintenance(ctx context.Context, a *auth.Auth) error {
	verify := func(ctx context.Context) error {
		before := a.MaintenanceIdentity()
		state := r.authGuard().State(accountID(a.UID)+":"+a.Variant+":auth", before)
		err := r.refreshCredentials(ctx, a, state.Kind == "authorization_unverified")
		if err == nil {
			before = a.MaintenanceIdentity()
		}
		r.observeMaintenance(a, "auth", before, err)
		return err
	}
	key := accountID(a.UID) + ":" + a.Variant + ":auth"
	ran, err := r.authGuard().Recover(ctx, key, a.MaintenanceIdentity(), verify)
	if err != nil || ran {
		return err
	}
	return verify(ctx)
}

// Caller holds r.mu. A failure belongs to the product identity, not every
// identity on the account; a healthy IDE/SOLO identity remains eligible.
func (r *Runtime) identityAvailableLocked(a *auth.Auth) bool {
	if a == nil {
		return false
	}
	if r.maintenanceGuard == nil {
		return true
	}
	identity, ready := a.TryMaintenanceIdentity()
	if !ready {
		return true
	} // Existing cancellable refresh flight owns the identity.
	return !r.maintenanceGuard.State(accountID(a.UID)+":"+a.Variant+":auth", identity).NeedsLogin
}
