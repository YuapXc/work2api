// Package runtime owns WorkBuddy credential, account and catalog state.
// It depends on shared storage/configuration, never the HTTP application.
package runtime

import (
	"context"
	"errors"
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
	modelRefresh   provider.RefreshGate
	MaintenanceMu  sync.Mutex
	maintenance    *maintenanceState
	Pool           *pool.Pool
	Catalog        *models.Registry
	Managers       map[string]*credentials.Manager
	ManagerMu      sync.RWMutex
	AccountMu      sync.Mutex
	Limiters       map[string]*ratelimit.Limiter
	LimMu          sync.Mutex
	ProjectAuths   string
	UpstreamClient Streamer
	RecordUsage    func(context.Context, store.UsageParams)
	cfg            *config.Config
	db             *store.DB
	Sessions       *SessionRouter
	CooldownMu     sync.Mutex
	Cooldowns      map[string]CooldownEntry
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
	return body
}

func (r *Runtime) RefreshCreditsFor(ctx context.Context, db *store.DB, acc *pool.Account) {
	mgr := r.Manager(acc.UID)
	if mgr == nil {
		return
	}
	cr, err := billing.FetchCredits(ctx, mgr)
	if err != nil {
		// 与上游一致：额度查询失败要留日志，否则「刷新额度没反应」无从排查
		log.Printf("额度查询失败 %s: %v", acc.UID, err)
		return
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
	r.maintenance = &maintenanceState{r: r, keepaliveFails: map[string]int{}}
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
	started, err := r.Stream(ctx, acc, body, sink, AttemptOptions{Ratelimit: r.cfg.Ratelimit, Interval: r.cfg.RatelimitInterval, LocalNetworkFailure: LocalNetworkFailure, Observe: func(line string) { Diagnostic(ctx).Observe(line) }, OnAttempt: func(ref provider.AccountRef) { r.Sessions.Attempt(ctx, ref.LocalID) }})
	var dispatch *provider.DispatchError
	if errors.As(err, &dispatch) {
		return started, provider.NewAPIError(dispatch.Status, dispatch.Message, dispatch.Type)
	}
	return started, err
}
