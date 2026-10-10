package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"work2api/internal/benchmarks"
	"work2api/internal/config"
	"work2api/internal/core/provider"
	"work2api/internal/crypto"
	"work2api/internal/opencode"
	"work2api/internal/qoder"
	"work2api/internal/store"
	"work2api/internal/workbuddy/credentials"
	"work2api/internal/workbuddy/models"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/ratelimit"
	wbruntime "work2api/internal/workbuddy/runtime"
	"work2api/internal/workbuddy/upstream"
)

type apiError = provider.APIError

var errBody = provider.NewAPIError

// Principal is the authenticated application. AllowedModels is the per-key
// model allowlist (nil = unrestricted); enforced in authorizeModel before any
// upstream dispatch. Portal fields (HANDOFF §7): UserID > 0 marks a portal
// user's key; AccountScope contains own accounts plus authorized shared accounts,
// restricted per model and never falling back to the unrestricted pool.
// PortalModels applies catalog ownership, group eligibility and disabled models.
// PortalModels nil on a
// portal key = no permission at all; that is a DIFFERENT meaning from a private
// key's empty AllowedModels ("unrestricted"), so the two never conflate.
type Principal struct {
	AppName       string
	AppID         int64
	AllowedModels []string
	UserID        int64
	AccountScope  map[string]bool
	PortalModels  map[string]bool
	ModelScopes   map[string]map[string]bool
	OwnedModels   map[string]bool
	SharedModels  map[string]bool
	// EffectiveModel is the resolved model name after prepareModel (empty
	// before). Keeps queue accounting and post-wait model authorization aligned.
	EffectiveModel string
	quota          *portalQuotaReservation
}

// Orchestrator holds shared runtime state and the request pipeline.
type Orchestrator struct {
	wb       *wbruntime.Runtime
	runtimes *provider.Registry
	cfg      *config.Config
	db       *store.DB
	crypto   *crypto.Manager
	bench    *benchmarks.Store

	backup       backupState
	sessions     *sessionRouter
	observations provider.SessionObservations
}

type cdEntry = wbruntime.CooldownEntry

func (o *Orchestrator) DB() *store.DB            { return o.db }
func (o *Orchestrator) Pool() *pool.Pool         { return o.wb.Pool }
func (o *Orchestrator) Models() *models.Registry { return o.wb.Catalog }
func (o *Orchestrator) Crypto() *crypto.Manager  { return o.crypto }

// Close is called after HTTP requests and scheduled tasks have drained.
func (o *Orchestrator) Close() error {
	return errors.Join(o.runtimes.Close(), o.db.Close())
}

func (o *Orchestrator) manager(uid string) *credentials.Manager         { return o.wb.Manager(uid) }
func (o *Orchestrator) setManager(uid string, mgr *credentials.Manager) { o.wb.SetManager(uid, mgr) }

// New builds the orchestrator, loading local auth files into the pool.
func New(cfg *config.Config) (*Orchestrator, error) {
	db, err := store.New(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	o := &Orchestrator{
		cfg:      cfg,
		db:       db,
		crypto:   crypto.NewManager(cfg.DataDir),
		bench:    benchmarks.New(db),
		sessions: newSessionRouter(),
	}
	if existing, err := db.HasEncryptedAppKeys(); err != nil {
		_ = db.Close()
		return nil, err
	} else if existing {
		o.crypto.RequireExistingKey()
	}
	var loadErr error
	o.wb, loadErr = wbruntime.New(cfg, db)
	if loadErr != nil {
		_ = db.Close()
		return nil, loadErr
	}
	o.sessions = o.wb.Sessions
	o.wb.RecordUsage = o.recordUsage
	o.registerRuntimes(cfg.DataDir, cfg.LogLevel)
	return o, nil
}

// registerRuntimes creates the registry owned by this gateway.
func (o *Orchestrator) registerRuntimes(dataDir, logLevel string) {
	o.runtimes = provider.NewRegistry()
	if err := o.runtimes.Register(o.wb); err != nil {
		panic(err)
	}
	if err := o.runtimes.Register(qoder.New(logLevel)); err != nil {
		panic(err)
	}
	if oc, err := opencode.New(nil, dataDir); err != nil {
		log.Printf("opencode 运行时初始化失败（已跳过）: %v", err)
	} else {
		if err := o.runtimes.Register(oc); err != nil {
			panic(err)
		}
		oc.BindRegistry(o.runtimes)
	}
}

func (o *Orchestrator) hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func (o *Orchestrator) genAPIKey() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return "sk-" + hex.EncodeToString(buf)
}

func (o *Orchestrator) checkAPIKey(authorization, xAPIKey string) (*Principal, *apiError) {
	token := ""
	if strings.HasPrefix(authorization, "Bearer ") {
		token = strings.TrimSpace(authorization[7:])
	}
	if token == "" {
		token = xAPIKey
	}
	if token == "" {
		return nil, errBody(401, "missing api key", "auth_error")
	}
	app, err := o.db.FindAppByKey(o.hashKey(token))
	if err != nil || app == nil {
		return nil, errBody(401, "invalid api key", "auth_error")
	}
	name, _ := app["name"].(string)
	id, _ := app["id"].(int64)
	userID, _ := app["user_id"].(int64)
	allowed, err := o.db.AllowedModelsOf(id)
	if err != nil {
		return nil, errBody(503, "密钥权限查询失败", "server_error")
	}
	p := &Principal{AppName: name, AppID: id, AllowedModels: allowed, UserID: userID}
	if userID > 0 {
		if aerr := o.attachPortalScope(p); aerr != nil {
			return nil, aerr
		}
	}
	return p, nil
}

// attachPortalScope computes the scheduling scope and permission model set for
// a portal user's key (HANDOFF §6/§7). Called on every authenticated request:
// personal ownership and shared-group permissions are read from SQLite.
// Withdrawal and disabled groups take effect without permission caching.
func (o *Orchestrator) attachPortalScope(p *Principal) *apiError {
	if !o.cfg.PortalEnabled {
		return errBody(403, "共享服务已关闭", "portal_disabled")
	}
	user, err := o.db.GetUser(p.UserID)
	if err != nil || user == nil || user.Status != "active" {
		return errBody(403, "账号不可用，请联系管理员", "portal_user_disabled")
	}
	if user.MustChangePassword {
		return errBody(403, "请先修改临时密码", "password_change_required")
	}
	eligible, err := o.db.ActiveContributionUIDs(p.UserID)
	if err != nil {
		return errBody(500, "资格查询失败", "server_error")
	}
	// 资格为空必须拒绝，不能转换成"不受限制"的 Principal（HANDOFF §7）。
	owned, err := o.db.OwnedAccountUIDs(p.UserID)
	if err != nil {
		return errBody(503, "本人账号查询失败", "server_error")
	}
	if len(owned) == 0 {
		return errBody(403, "请先添加并验证你的 WorkBuddy 账号", "portal_no_eligibility")
	}
	// Eligibility interacts with the group scope: an account that left all
	// enabled groups or whose contribution was revoked drops out here.
	p.AccountScope = map[string]bool{}
	p.ModelScopes = map[string]map[string]bool{}
	p.OwnedModels = map[string]bool{}
	p.SharedModels = map[string]bool{}
	models := map[string]bool{}
	settings, err := o.db.GetSettings()
	if err != nil {
		return errBody(503, "模型权限查询失败", "server_error")
	}
	aliases := parseModelAliases(settings["model_aliases"])
	resolve := func(m string) string {
		if target, ok := aliases[m]; ok {
			return target
		}
		return m
	}
	disabled := map[string]bool{}
	for _, m := range parseJSONStringArray(settings["portal_disabled_models"]) {
		disabled[m] = true
		disabled[resolve(m)] = true
	}
	// Personal rights derive from the owner's verified account and the actual
	// catalog, independently of shared-group membership. Never widen to a site
	// or another user's account when an explicit UID scope exists.
	for _, entry := range o.wb.Catalog.ListCached() {
		m := str2(entry["id"])
		if m == "" || m == "auto" || strings.Contains(m, "/") || disabled[m] {
			continue
		}
		uids := o.modelEntryAccountUIDs(entry)
		for uid := range uids {
			if !owned[uid] {
				continue
			}
			if p.ModelScopes[m] == nil {
				p.ModelScopes[m] = map[string]bool{}
			}
			p.ModelScopes[m][uid] = true
			models[m], p.OwnedModels[m] = true, true
		}
	}
	grants, err := o.db.GrantedGroups(p.UserID)
	if err != nil {
		return errBody(500, "资格查询失败", "server_error")
	}
	for _, g := range grants {
		if len(eligible) == 0 || !g.Enabled || g.Provider != "workbuddy" {
			continue
		}
		// 未配置共享模型范围的分组保持关闭（HANDOFF §2）：空串 = 未配置。
		if strings.TrimSpace(g.AllowedModels) == "" {
			continue
		}
		uids, err := o.db.GroupAccountUIDsWithActiveContribution(g.ID)
		if err != nil {
			return errBody(503, "共享账户查询失败", "server_error")
		}
		if len(uids) == 0 {
			continue
		}
		for _, m := range parseJSONStringArray(g.AllowedModels) {
			resolved := resolve(m)
			if disabled[m] || disabled[resolved] || resolved == "auto" || strings.Contains(resolved, "/") {
				continue
			}
			models[resolved] = true
			p.SharedModels[resolved] = true
			if p.ModelScopes[resolved] == nil {
				p.ModelScopes[resolved] = map[string]bool{}
			}
			for _, uid := range uids {
				p.ModelScopes[resolved][uid] = true
			}
		}
	}
	// 管理员禁用模型从允许集中扣除：disabled_reason 标记的账号只是账号级冷却；
	// 模型禁用由管理端 settings 维护的 portal_disabled_models 列表承担。
	p.PortalModels = models
	return nil
}

// parseJSONStringArray decodes a stored JSON array of strings, returning nil
// for garbage (display/config values, never auth-critical alone).
func parseJSONStringArray(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []string
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	return out
}

// authorizeModel enforces the principal's per-key model allowlist. The model
// is checked as-requested AND alias-resolved, so a key allowing "gpt" permits
// calls made via that alias, and a key listing real ids permits aliases resolving
// to those ids. Empty allowlist = unrestricted (private keys only — a portal
// key is separately gated by PortalModels, and portal authorization is deny by
// default). Retry/failover must not widen this: both paths share modelAllowed.
func (s *Server) authorizeModel(principal *Principal, requestedModel string) *apiError {
	if principal == nil {
		return nil
	}
	if principal.UserID > 0 {
		model := strings.TrimSpace(requestedModel)
		if model == "" {
			model = "auto"
		}
		if !s.portalModelAllowed(principal, model, s.o.resolveModel(model)) {
			return errBody(403, "该模型不在共享范围或当前资格不允许（模型："+model+"）", "model_not_allowed")
		}
		return nil
	}
	if len(principal.AllowedModels) == 0 {
		return nil
	}
	model := strings.TrimSpace(requestedModel)
	if model == "" {
		model = "auto"
	}
	if modelAllowed(principal, model, s.o.resolveModel(model)) {
		return nil
	}
	return errBody(403, "该 API 密钥未被授权使用模型 "+model+"（可在 WebUI「API 密钥」中调整可用模型）", "model_not_allowed")
}

// portalModelAllowed applies HANDOFF §7: 分组 ∩ 资格 ∩ Key 白名单 − 禁用.
// PortalModels already carries group∩eligibility−disabled; the key allowlist
// narrows further. Deny by default: a portal principal with no PortalModels
// permits nothing.
func (s *Server) portalModelAllowed(principal *Principal, model, resolved string) bool {
	if principal == nil || principal.PortalModels == nil {
		return false
	}
	if !principal.PortalModels[resolved] || resolved == "auto" || strings.Contains(resolved, "/") {
		return false
	}
	if len(principal.AllowedModels) == 0 {
		return false
	}
	for _, allowed := range principal.AllowedModels {
		if s.o.resolveModel(allowed) == resolved {
			return true
		}
	}
	return false
}

// modelAllowed shares the same requested-id/alias-target policy between
// request authorization and the public model catalog.
func modelAllowed(principal *Principal, model, resolved string) bool {
	if principal == nil || len(principal.AllowedModels) == 0 {
		return true
	}
	for _, allowed := range principal.AllowedModels {
		if allowed == model || allowed == resolved {
			return true
		}
	}
	return false
}

func (s *Server) prepareModel(principal *Principal, payload map[string]any) *apiError {
	if payload == nil {
		return errBody(400, "请求体必须是 JSON 对象", "invalid_request_error")
	}
	model := ""
	if value := payload["model"]; value != nil {
		name, ok := value.(string)
		if !ok {
			return errBody(400, "model 必须是字符串", "invalid_request_error")
		}
		if name = strings.TrimSpace(name); name != "" {
			model = name
		}
	}
	if model == "" {
		model = "auto"
		if principal != nil && principal.UserID > 0 {
			// Portal keys: auto is a real WorkBuddy model and must not act as an
			// implicit fallback (HANDOFF §7). Exactly one permitted model → use
			// it; zero or multiple → require explicit model.
			allowed := map[string]bool{}
			for m := range principal.PortalModels {
				if s.portalModelAllowed(principal, m, s.o.resolveModel(m)) {
					allowed[m] = true
				}
			}
			switch len(allowed) {
			case 1:
				for m := range allowed {
					model = m
				}
			default:
				return errBody(400, "请明确指定 model（可通过 /v1/models 查询可用模型）", "invalid_request_error")
			}
		} else if principal != nil {
			switch len(principal.AllowedModels) {
			case 1:
				model = principal.AllowedModels[0]
			case 0:
				// Unrestricted keys retain the upstream default.
			default:
				return errBody(400, "该 API 密钥允许多个模型，请明确指定 model（可通过 /v1/models 查询可用模型）", "invalid_request_error")
			}
		}
	}
	payload["model"] = model
	principal.EffectiveModel = s.o.resolveModel(model)
	if aerr := s.authorizeModel(principal, model); aerr != nil {
		return aerr
	}
	if principal != nil && principal.UserID > 0 {
		principal.AccountScope = principal.ModelScopes[s.o.resolveModel(model)]
		if principal.AccountScope == nil {
			principal.AccountScope = map[string]bool{}
		}
	}
	return nil
}

func (o *Orchestrator) limiter(uid string) *ratelimit.Limiter {
	return o.wb.Limiter(uid, o.cfg.RatelimitInterval)
}

func (o *Orchestrator) modelCooldownUntil(uid, model string) float64 {
	return o.wb.ModelCooldownUntil(uid, model)
}

func (o *Orchestrator) markDailyModelLimit(acc *pool.Account, model string, raw []byte) bool {
	return o.wb.MarkDailyModelLimit(acc, model, raw)
}

func (o *Orchestrator) penalizeAccount(acc *pool.Account, model string, ue *upstream.UpstreamError) errAction {
	return o.wb.PenalizeAccount(acc, model, ue)
}

func (o *Orchestrator) modelAccountUIDs(model string) (map[string]bool, *apiError) {
	return o.wb.ModelAccountUIDs(model)
}

func (o *Orchestrator) modelEntryAccountUIDs(entry map[string]any) map[string]bool {
	return o.wb.ModelEntryAccountUIDs(entry)
}

func (s *Server) pickAccountFor(principal *Principal, model, sessionKey string) (*pool.Account, *apiError) {
	if principal != nil && principal.UserID > 0 {
		return s.o.pickAccountExcludingIn(model, sessionKey, nil, principal.AccountScope)
	}
	return s.o.pickAccount(model, sessionKey)
}

func (o *Orchestrator) pickAccount(model, sessionKey string) (*pool.Account, *apiError) {
	return o.wb.PickAccount(model, sessionKey)
}

func (o *Orchestrator) pickAccountExcluding(model, sessionKey string, tried map[string]bool) (*pool.Account, *apiError) {
	return o.wb.PickAccountExcluding(model, sessionKey, tried)
}

func (o *Orchestrator) pickAccountExcludingIn(model, sessionKey string, tried map[string]bool, scope map[string]bool) (*pool.Account, *apiError) {
	return o.wb.PickAccountExcludingIn(model, sessionKey, tried, scope)
}

func (o *Orchestrator) pickInScope(model, sessionKey string, tried map[string]bool, scope map[string]bool) (*pool.Account, *apiError) {
	return o.wb.PickInScope(model, sessionKey, tried, scope)
}

var errPoolExhausted = wbruntime.ErrPoolExhausted

func (o *Orchestrator) modelCostByUID(model string, ready map[string]bool) map[string]float64 {
	return o.wb.ModelCostByUID(model, ready)
}

func (o *Orchestrator) expiryWindowDays() float64 { return o.wb.ExpiryWindowDays() }

func (o *Orchestrator) resolveModel(model string) string {
	s, _ := o.db.GetSettings()
	aliases := parseModelAliases(s["model_aliases"])
	if real, ok := aliases[model]; ok {
		return real
	}
	return model
}

func (o *Orchestrator) enhanceBody(body map[string]any) map[string]any {
	if model, ok := body["model"].(string); ok {
		body["model"] = o.resolveModel(model)
	}
	return o.wb.EnhanceBody(body)
}

func (o *Orchestrator) runOnce(ctx context.Context, acc *pool.Account, body map[string]any, sink func(string) error) (bool, error) {
	return o.wb.RunOnce(o.invocationContext(ctx), acc, body, sink)
}
func (o *Orchestrator) invocationContext(ctx context.Context) context.Context {
	call := provider.CurrentInvocation(ctx)
	if call.Provider == "" {
		call.Provider = o.wb.Name()
	}
	if p, ok := ctx.Value(principalContextKey{}).(*Principal); ok {
		call.Caller = p.SessionCaller()
	}
	if validate, ok := ctx.Value(portalDispatchCheckKey{}).(func(string, string) *apiError); ok {
		inheritedCheck := call.Check
		call.Check = func(ctx context.Context, ref provider.AccountRef, model string) error {
			if inheritedCheck != nil {
				if err := inheritedCheck(ctx, ref, model); err != nil {
					return err
				}
			}
			if ref.Provider != "workbuddy" {
				return errBody(403, "当前账号渠道不支持共享调用", "permission_error")
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := validate(ref.LocalID, model); err != nil {
				return err
			}
			return nil
		}
	}
	if lease, ok := ctx.Value(modelLeaseKey{}).(*modelLease); ok {
		call.Wait = lease.throttle

		call.Select = func(ctx context.Context, choose func(map[string]bool) (provider.AccountRef, error)) (provider.AccountRef, error) {
			ref, err := lease.bindProviderAccount(ctx, func(busy map[string]bool) (*provider.AccountRef, *apiError) {
				candidate, err := choose(provider.LocalBusy(busy, call.Provider))
				if err != nil {
					var ae *apiError
					if errors.As(err, &ae) {
						return nil, ae
					}
					return nil, errBody(503, err.Error(), "server_error")
				}
				if candidate.LocalID == "" {
					return nil, nil
				}
				return &candidate, nil
			})
			if err != nil {
				return provider.AccountRef{}, err
			}
			if ref == nil {
				return provider.AccountRef{}, nil
			}
			return *ref, nil
		}

	}
	return provider.WithInvocation(ctx, call)
}

func channelIdentityCompatBody(body map[string]any) (map[string]any, bool) {
	return channelCompatibilityBody(body, 1)
}

// Retries are bounded to five total upstream attempts, including compatibility
// retries. No replay is allowed after output; every account stays in scope.
func (o *Orchestrator) openUpstream(ctx context.Context, acc *pool.Account, body map[string]any, model, sessionKey string, sink func(string) error, onRetryFail func(*pool.Account, *upstream.UpstreamError)) (*pool.Account, error) {
	ctx = o.invocationContext(ctx)
	return o.wb.OpenUpstream(ctx, acc, body, model, sessionKey, sink, onRetryFail)
}

func (o *Orchestrator) openUpstreamScoped(ctx context.Context, acc *pool.Account, body map[string]any, model, sessionKey string, sink func(string) error, onRetryFail func(*pool.Account, *upstream.UpstreamError), scope map[string]bool) (served *pool.Account, callErr error) {
	ctx = o.invocationContext(ctx)
	return o.wb.OpenUpstreamScoped(ctx, acc, body, model, sessionKey, sink, onRetryFail, scope)
}

func (o *Orchestrator) accountSelector(model, preferred string, tried map[string]bool, scope map[string]bool, routing ...*sessionSelection) (func(map[string]bool) (*pool.Account, *apiError), *apiError) {
	return o.wb.AccountSelector(model, preferred, tried, scope, routing...)
}

func (o *Orchestrator) preferDomesticSession(key string, current *pool.Account, ready map[string]bool, costs map[string]float64, window float64) *pool.Account {
	return o.wb.PreferDomesticSession(key, current, ready, costs, window)
}

func (o *Orchestrator) refreshCreditsFor(ctx context.Context, acc *pool.Account) {
	o.wb.RefreshCreditsFor(ctx, o.db, acc)
}
func (o *Orchestrator) refreshAllCredits(ctx context.Context) { o.wb.RefreshAllCredits(ctx, o.db) }

func nowSec() float64 { return float64(time.Now().UnixNano()) / 1e9 }

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
		return itoa(x)
	}
	return ""
}

func filepathJoin(a, b string) string {
	if a == "" {
		return b
	}
	if strings.HasSuffix(a, "/") || strings.HasSuffix(a, "\\") {
		return a + b
	}
	return a + "/" + b
}

func (p *Principal) SessionCaller() provider.Caller {
	if p == nil {
		return provider.Caller{}
	}
	return provider.Caller{AppID: p.AppID, UserID: p.UserID, AppName: p.AppName, AccountScope: p.AccountScope}
}

// Legacy WB helpers/tests adapt channel accounts to neutral lease identities.
func (l *modelLease) bindAccount(ctx context.Context, choose func(map[string]bool) (*pool.Account, *apiError)) (*pool.Account, error) {
	var selected *pool.Account
	_, err := l.bindProviderAccount(ctx, func(busy map[string]bool) (*provider.AccountRef, *apiError) {
		a, e := choose(provider.LocalBusy(busy, "workbuddy"))
		selected = a
		if e != nil || a == nil {
			return nil, e
		}
		return &provider.AccountRef{Provider: "workbuddy", LocalID: a.UID}, nil
	})
	return selected, err
}

// ProviderStateLockPaths is evaluated before runtime/database construction.
func ProviderStateLockPaths() []string { return append(wbruntime.LockPaths(), qoder.LockPaths()...) }
