// Package runtime owns WorkBuddy credential, account and catalog state.
// It depends on shared storage/configuration, never the HTTP application.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"path/filepath"
	"strconv"
	"sync"
	"time"
	"work2api/internal/config"
	"work2api/internal/core/provider"
	"work2api/internal/store"
	"work2api/internal/workbuddy/billing"
	"work2api/internal/workbuddy/credentials"
	"work2api/internal/workbuddy/models"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/ratelimit"
	"work2api/internal/workbuddy/reasoning"
	"work2api/internal/workbuddy/siterouting"
	"work2api/internal/workbuddy/upstream"
)

type Streamer interface {
	StreamUpstream(context.Context, map[string]string, map[string]any, string, upstream.LineFunc) error
}

// Runtime owns the mutable WorkBuddy resources for exactly one gateway.
// AccountMu coordinates import/delete with the gateway's ownership transaction.
type Runtime struct {
	creditMu         sync.Mutex
	maintenanceGuard *provider.MaintenanceGuard
	creditStates     map[string]creditState
	creditRefresh    map[string]*provider.RefreshGate
	modelRefresh     provider.RefreshGate
	MaintenanceMu    sync.Mutex
	maintenance      *maintenanceState
	Pool             *pool.Pool
	Catalog          *models.Registry
	Managers         map[string]*credentials.Manager
	ManagerMu        sync.RWMutex
	AccountMu        sync.Mutex
	Limiters         map[string]*ratelimit.Limiter
	LimMu            sync.Mutex
	ProjectAuths     string
	UpstreamClient   Streamer
	RecordUsage      func(context.Context, store.UsageParams)
	cfg              *config.Config
	db               *store.DB
	Sessions         *SessionRouter
	CooldownMu       sync.Mutex
	Cooldowns        map[string]CooldownEntry
}

type creditState struct {
	Identity     string
	Manager      *credentials.Manager
	Updated      float64
	Failed       bool
	Kind         string
	Reauthorize  bool
	LoginExpired bool
}

func New(cfg *config.Config, db *store.DB) (*Runtime, error) {
	r := &Runtime{ProjectAuths: filepath.Join(config.PackageRoot, "auths"), Managers: map[string]*credentials.Manager{}, Limiters: map[string]*ratelimit.Limiter{}}
	creds := map[string]pool.Credential{}
	settings, err := db.GetSettings()
	if err != nil {
		return nil, err
	}
	for _, f := range credentials.FindAuthFiles("", r.ProjectAuths) {
		mgr := credentials.NewManager(f)
		s := mgr.Summary()
		uid, _ := s["uid"].(string)
		if uid == "" {
			continue
		}
		if settings[store.HiddenAccountKey(uid)] == "1" {
			continue
		}
		if raw, err := mgr.RawSession(); err == nil {
			if _, err := PersistAccount(db, map[string]any{"auth": raw.Auth, "account": raw.Account}); err != nil {
				return nil, err
			}
		}
		r.SetManager(uid, mgr)
		creds[uid] = mgr
	}
	r.Pool = pool.New(creds, r.ProjectAuths)
	contributions, err := db.ListContributions()
	if err != nil {
		return nil, err
	}
	if rows, err := db.ListAccounts(); err == nil {
		for _, row := range rows {
			uid, _ := row["uid"].(string)
			if uid == "" {
				continue
			}
			r.Pool.SetCredits(uid, fptr(row["credits_remaining"]), fptr(row["credits_total"]), fptr(row["credits_expire_at"]), nil)
			if alias, _ := row["alias"].(string); alias != "" {
				r.Pool.SetAlias(uid, alias)
			}
			if p := intOf(row["priority"]); p != 0 {
				r.Pool.SetPriority(uid, p)
			}
			if intOf(row["enabled"]) == 0 {
				reason, _ := row["disabled_reason"].(string)
				if reason == "" {
					reason = "手动停用"
				}
				r.Pool.SetEnabled(uid, false, reason)
			}
		}
	}
	for _, c := range contributions {
		if c.Status != "active" && c.Status != "private" {
			r.Pool.SetEnabled(c.AccountUID, false, "共享贡献不可用")
		}
	}
	r.Catalog = models.New(r.Pool, db)
	r.Configure(cfg, db, NewSessionRouter())

	return r, nil
}

func (r *Runtime) Manager(uid string) *credentials.Manager {
	r.ManagerMu.RLock()
	defer r.ManagerMu.RUnlock()
	return r.Managers[uid]
}
func (r *Runtime) SetManager(uid string, mgr *credentials.Manager) {
	if mgr != nil {
		mgr.BindMaintenance(r.billingGuard(), uid)
	}
	r.ManagerMu.Lock()
	defer r.ManagerMu.Unlock()
	if mgr == nil {
		delete(r.Managers, uid)
	} else {
		if r.Managers == nil {
			r.Managers = map[string]*credentials.Manager{}
		}
		r.Managers[uid] = mgr
	}
}

// Close retires refresh workers without removing the original credential files.
func (r *Runtime) Close() error {
	r.ManagerMu.RLock()
	managers := make([]*credentials.Manager, 0, len(r.Managers))
	for _, mgr := range r.Managers {
		managers = append(managers, mgr)
	}
	r.ManagerMu.RUnlock()
	var failures []error
	for _, mgr := range managers {
		if mgr != nil {
			failures = append(failures, mgr.Retire(false))
		}
	}
	return errors.Join(failures...)
}
func (r *Runtime) Limiter(uid string, interval float64) *ratelimit.Limiter {
	r.LimMu.Lock()
	defer r.LimMu.Unlock()
	if r.Limiters == nil {
		r.Limiters = map[string]*ratelimit.Limiter{}
	}
	l, ok := r.Limiters[uid]
	if !ok {
		l = ratelimit.New(time.Duration(interval*float64(time.Second)), 300*time.Millisecond)
		r.Limiters[uid] = l
	}
	return l
}

func fptr(v any) *float64 {
	switch x := v.(type) {
	case float64:
		return &x
	case int64:
		f := float64(x)
		return &f
	case string:
		if x == "" {
			return nil
		}
		if f, err := strconv.ParseFloat(x, 64); err == nil {
			return &f
		}
	}
	return nil
}

func intOf(v any) int {
	switch x := v.(type) {
	case int64:
		return int(x)
	case float64:
		return int(x)
	case int:
		return x
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

func toStrLoose(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case int:
		return strconv.Itoa(x)
	}
	return ""
}

// PersistAccount preserves WorkBuddy's legacy UID and profile normalization.
func PersistAccount(db *store.DB, raw map[string]any) (string, error) {
	account, _ := raw["account"].(map[string]any)
	auth, _ := raw["auth"].(map[string]any)
	if auth == nil {
		auth = raw
	}
	profile, err := siterouting.ProfileForAuth(auth)
	if err != nil {
		profile = siterouting.DefaultProfile
	}
	str := func(v any) string {
		switch x := v.(type) {
		case string:
			return x
		case []byte:
			return string(x)
		case float64:
			if x == math.Trunc(x) {
				return strconv.FormatInt(int64(x), 10)
			}
		case int64:
			return strconv.FormatInt(x, 10)
		}
		return ""
	}
	return db.UpsertProviderAccount(store.ProviderAccount{
		UID: str(account["uid"]), Provider: "workbuddy", Nickname: str(account["nickname"]), EnterpriseID: str(account["enterpriseId"]), Domain: str(auth["domain"]), Profile: profile, Auth: raw,
	})
}

func (r *Runtime) enhanceBody(body map[string]any) map[string]any {
	modelID, _ := body["model"].(string)
	maxOut := r.Catalog.MaxOutputTokens(modelID)
	// 同时钳制 max_tokens 与 max_completion_tokens（新版 OpenAI SDK 用后者）：
	// 超过模型上限的值会被上游直接 400。上游对两个键都做了裁剪。
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		mt, ok := body[key]
		if !ok || mt == nil {
			continue
		}
		if n, err := strconv.Atoi(toStrLoose(mt)); err == nil {
			if maxOut > 0 && n > maxOut {
				n = maxOut
			}
			body[key] = n
		} else {
			delete(body, key)
		}
	}
	var dyn map[string][]string
	if modelID != "" {
		requested := toStrLoose(body["reasoning_effort"])
		if requested == "" {
			requested = toStrLoose(body["reasoningEffort"])
		}
		if eff := r.Catalog.RequestEfforts(modelID, requested); eff != nil {
			dyn = map[string][]string{modelID: eff}
		}
	}
	body = reasoning.Sanitize(body, dyn)
	// Channel compatibility rewrites happen once per request, before the first
	// attempt: the upstream rejects these fingerprints deterministically, so an
	// escalation after a 400 would only add a round trip and an account strike.
	if r.cfg != nil && r.cfg.ChannelIdentityCompat {
		if neutralized, changed := NeutralizeChannelFingerprints(body); changed {
			body = neutralized
		}
	}
	return body
}

func (r *Runtime) RefreshCreditsFor(ctx context.Context, db *store.DB, acc *pool.Account) {
	r.creditMu.Lock()
	if r.creditRefresh == nil {
		r.creditRefresh = map[string]*provider.RefreshGate{}
	}
	gate := r.creditRefresh[acc.UID]
	if gate == nil {
		gate = &provider.RefreshGate{}
		r.creditRefresh[acc.UID] = gate
	}
	r.creditMu.Unlock()
	_ = gate.Do(ctx, fmt.Sprintf("%p", r.Manager(acc.UID)), func(ctx context.Context) error {
		mgr := r.Manager(acc.UID)
		if mgr == nil {
			return nil
		}
		identity, err := mgr.MaintenanceIdentity()
		if err != nil {
			return err
		}
		ran, err := r.billingGuard().Recover(ctx, acc.UID, identity, func(ctx context.Context) error { return r.refreshCreditsFor(ctx, db, acc) })
		if err != nil || ran {
			return err
		}
		return r.refreshCreditsFor(ctx, db, acc)
	})
}
func (r *Runtime) refreshCreditsFor(ctx context.Context, db *store.DB, acc *pool.Account) error {
	mgr := r.Manager(acc.UID)
	if mgr == nil {
		return nil
	}
	if _, err := mgr.GetHeadersContext(ctx); err != nil {
		r.observeBilling(acc.UID, mgr, err)
		return err
	}
	identity, _ := mgr.MaintenanceIdentity()
	cr, err := billing.FetchCredits(ctx, mgr)
	var queryErr *billing.QueryError
	if errors.As(err, &queryErr) && (queryErr.Kind == "unauthorized" || queryErr.Kind == "login_expired") && ctx.Err() == nil {
		if refreshErr := mgr.RefreshBillingAuthorization(ctx); refreshErr == nil {
			identity, _ = mgr.MaintenanceIdentity()
			cr, err = billing.FetchCredits(ctx, mgr)
		} else {
			err = refreshErr
		}
	}
	r.AccountMu.Lock()
	defer r.AccountMu.Unlock()
	if ctx.Err() != nil || r.Manager(acc.UID) != mgr {
		return ctx.Err()
	}
	currentIdentity, _ := mgr.MaintenanceIdentity()
	if currentIdentity != identity {
		return provider.ErrMaintenancePaused
	}
	r.creditMu.Lock()
	if r.creditStates == nil {
		r.creditStates = map[string]creditState{}
	}
	state := r.creditStates[acc.UID]
	if state.Manager != mgr {
		state = creditState{Manager: mgr}
	}
	state.Identity = identity
	state.Failed = err != nil
	state.Reauthorize = false
	state.LoginExpired = errors.Is(err, credentials.ErrLoginRequired)
	state.Kind = ""
	queryErr = nil
	if errors.As(err, &queryErr) {
		state.Kind = queryErr.Kind
		state.LoginExpired = state.LoginExpired || queryErr.Kind == "login_expired"
		state.Reauthorize = queryErr.Kind == "unauthorized" || queryErr.Kind == "credential" && state.LoginExpired
	}
	state.Reauthorize = state.Reauthorize || state.LoginExpired
	if err == nil {
		state.Updated = nowSec()
	}
	r.creditStates[acc.UID] = state
	r.creditMu.Unlock()
	r.observeBilling(acc.UID, mgr, err)
	if err != nil {
		// 与上游一致：额度查询失败要留日志，否则「刷新额度没反应」无从排查
		log.Printf("额度查询失败 %s: %v", acc.UID, err)
		return err
	}
	r.Pool.SetCredits(acc.UID, &cr.Remain, &cr.Total, cr.ExpireAt, cr.Packages)
	fields := map[string]any{"credits_remaining": cr.Remain, "credits_total": cr.Total}
	if cr.ExpireAt != nil {
		fields["credits_expire_at"] = strconv.FormatFloat(*cr.ExpireAt, 'f', -1, 64)
	} else {
		// 之前有到期时间、现在没有了要清掉，否则 DB 留着过期的旧时间戳
		fields["credits_expire_at"] = ""
	}
	_ = db.SetAccountState(acc.UID, fields)
	// 仅在余额 > 0 时自动解冻：余额耗尽的账号不该被 credit 刷新顺手解冻
	if cr.Remain > 0 {
		r.Pool.ClearCooldown(acc.UID)
	}
	return nil
}

// refreshAllCredits refreshes every account's credits concurrently. Done in
// parallel because each account issues several sequential upstream billing
// calls (~seconds each); refreshing serially made the manual "刷新额度" button
// exceed the client's 30s timeout once more than one account was configured.
func (r *Runtime) RefreshAllCredits(ctx context.Context, db *store.DB) {
	var wg sync.WaitGroup
	for _, a := range r.Pool.Accounts() {
		wg.Add(1)
		go func(acc *pool.Account) {
			defer wg.Done()
			r.RefreshCreditsFor(ctx, db, acc)
		}(a)
	}
	wg.Wait()
}

type CooldownEntry struct {
	Until  float64
	Reason string
}

func (r *Runtime) Configure(cfg *config.Config, db *store.DB, sessions *SessionRouter) {
	r.cfg, r.db, r.Sessions = cfg, db, sessions
	guard := r.billingGuard()
	for uid, mgr := range r.Managers {
		mgr.BindMaintenance(guard, uid)
	}
	if r.Catalog != nil {
		r.Catalog.SetMaintenanceGuard(guard)
	}
	r.maintenance = &maintenanceState{r: r}
	r.Cooldowns = map[string]CooldownEntry{}
	if rows, err := db.ActiveModelCooldowns(0); err == nil {
		for _, row := range rows {
			uid, _ := row["account_uid"].(string)
			model, _ := row["model"].(string)
			until, _ := row["cooldown_until"].(float64)
			reason, _ := row["reason"].(string)
			if uid != "" && model != "" && until > 0 {
				r.Cooldowns[uid+"|"+model] = CooldownEntry{Until: until, Reason: reason}
			}
		}
	}
}
func (r *Runtime) RunOnce(ctx context.Context, acc *pool.Account, body map[string]any, sink func(string) error) (bool, error) {
	started, err := r.Stream(ctx, acc, body, sink, AttemptOptions{Ratelimit: r.cfg.Ratelimit && !r.cfg.ModelLimitsDisabled, Interval: r.cfg.RatelimitInterval, LocalNetworkFailure: LocalNetworkFailure, Observe: func(line string) { Diagnostic(ctx).ObserveChunk(upstream.DecodeFrame(ctx, line)) }, OnAttempt: func(ref provider.AccountRef) { r.Sessions.Attempt(ctx, ref.LocalID) }})
	var dispatch *provider.DispatchError
	if errors.As(err, &dispatch) {
		return started, provider.NewAPIError(dispatch.Status, dispatch.Message, dispatch.Type)
	}
	return started, err
}
