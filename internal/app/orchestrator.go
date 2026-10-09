package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"work2api/internal/benchmarks"
	"work2api/internal/config"
	"work2api/internal/core/provider"
	"work2api/internal/crypto"
	"work2api/internal/opencode"
	"work2api/internal/qoder"
	"work2api/internal/store"
	"work2api/internal/streamwatch"
	"work2api/internal/workbuddy/billing"
	"work2api/internal/workbuddy/credentials"
	"work2api/internal/workbuddy/models"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/ratelimit"
	"work2api/internal/workbuddy/reasoning"
	"work2api/internal/workbuddy/siterouting"
	"work2api/internal/workbuddy/upstream"
)

type apiError struct {
	status int
	body   map[string]any
}

func (e *apiError) Error() string { return "api error " + itoa(e.status) }

func errBody(status int, message, typ string) *apiError {
	return &apiError{status: status, body: map[string]any{"error": map[string]any{"message": message, "type": typ}}}
}

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

type upstreamStreamer interface {
	StreamUpstream(context.Context, map[string]string, map[string]any, string, upstream.LineFunc) error
}

// Orchestrator holds shared runtime state and the request pipeline.
type Orchestrator struct {
	upstreamClient upstreamStreamer
	cfg            *config.Config
	db             *store.DB
	crypto         *crypto.Manager
	pool           *pool.Pool
	models         *models.Registry
	bench          *benchmarks.Store
	managers       map[string]*credentials.Manager
	managerMu      sync.RWMutex
	accountMu      sync.Mutex

	limMu    sync.Mutex
	limiters map[string]*ratelimit.Limiter

	mcMu           sync.Mutex
	modelCooldowns map[string]cdEntry
	projectAuths   string
	backup         backupState
	sessions       *sessionRouter
}

type cdEntry struct {
	until  float64
	reason string
}

func (o *Orchestrator) DB() *store.DB            { return o.db }
func (o *Orchestrator) Pool() *pool.Pool         { return o.pool }
func (o *Orchestrator) Models() *models.Registry { return o.models }
func (o *Orchestrator) Crypto() *crypto.Manager  { return o.crypto }

func (o *Orchestrator) manager(uid string) *credentials.Manager {
	o.managerMu.RLock()
	defer o.managerMu.RUnlock()
	return o.managers[uid]
}

func (o *Orchestrator) setManager(uid string, mgr *credentials.Manager) {
	o.managerMu.Lock()
	defer o.managerMu.Unlock()
	if mgr == nil {
		delete(o.managers, uid)
	} else {
		o.managers[uid] = mgr
	}
}

// New builds the orchestrator, loading local auth files into the pool.
func New(cfg *config.Config) (*Orchestrator, error) {
	db, err := store.New(cfg.DBPath)
	if err != nil {
		return nil, err
	}
	projectAuths := filepathJoin(config.PackageRoot, "auths")
	o := &Orchestrator{
		cfg:            cfg,
		db:             db,
		crypto:         crypto.NewManager(cfg.DataDir),
		bench:          benchmarks.New(db),
		managers:       map[string]*credentials.Manager{},
		limiters:       map[string]*ratelimit.Limiter{},
		modelCooldowns: map[string]cdEntry{},
		projectAuths:   projectAuths,
		sessions:       newSessionRouter(),
	}
	if existing, err := db.HasEncryptedAppKeys(); err != nil {
		_ = db.Close()
		return nil, err
	} else if existing {
		o.crypto.RequireExistingKey()
	}
	// 重启后回填持久化的 (账号,模型) 冷却（6004 每日上限）：否则内存 map 为空，
	// 已达上限的模型会被重新选中、白打一次上游、给客户端漏一个瞬时 6004 再轮转。
	if rows, err := db.ActiveModelCooldowns(0); err == nil {
		for _, row := range rows {
			uid, _ := row["account_uid"].(string)
			model, _ := row["model"].(string)
			until, _ := row["cooldown_until"].(float64)
			reason, _ := row["reason"].(string)
			if uid != "" && model != "" && until > 0 {
				o.modelCooldowns[uid+"|"+model] = cdEntry{until: until, reason: reason}
			}
		}
	}
	creds := map[string]pool.Credential{}
	settings, err := db.GetSettings()
	if err != nil {
		return nil, err
	}
	for _, f := range credentials.FindAuthFiles("", projectAuths) {
		mgr := credentials.NewManager(f)
		s := mgr.Summary()
		uid, _ := s["uid"].(string)
		if uid == "" {
			continue
		}
		if settings[hiddenAccountKey(uid)] == "1" {
			continue
		}
		if raw, err := mgr.RawSession(); err == nil {
			_, _ = db.UpsertAccount(map[string]any{"auth": raw.Auth, "account": raw.Account})
		}
		o.setManager(uid, mgr)
		creds[uid] = mgr
	}
	o.pool = pool.New(creds, projectAuths)
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
			o.pool.SetCredits(uid, fptr(row["credits_remaining"]), fptr(row["credits_total"]), fptr(row["credits_expire_at"]), nil)
			if alias, _ := row["alias"].(string); alias != "" {
				o.pool.SetAlias(uid, alias)
			}
			if p := intOf(row["priority"]); p != 0 {
				o.pool.SetPriority(uid, p)
			}
			if intOf(row["enabled"]) == 0 {
				reason, _ := row["disabled_reason"].(string)
				if reason == "" {
					reason = "手动停用"
				}
				o.pool.SetEnabled(uid, false, reason)
			}
		}
	}
	for _, c := range contributions {
		if c.Status != "active" && c.Status != "private" {
			o.pool.SetEnabled(c.AccountUID, false, "共享贡献不可用")
		}
	}
	o.models = models.New(o.pool, db)
	registerRuntimes(cfg.DataDir, cfg.LogLevel)
	return o, nil
}

// registerRuntimes wires the non-default provider inference runtimes (qoder,
// opencode) into the shared dispatcher. Each is self-contained and stays inert
// when it has no credentials/config, so registration is unconditional and
// idempotent per process. workbuddy remains the default (un-namespaced) path.
var runtimesOnce sync.Once

func registerRuntimes(dataDir, logLevel string) {
	runtimesOnce.Do(func() {
		provider.RegisterRuntime(qoder.New(logLevel))
		if oc, err := opencode.New(nil, dataDir); err != nil {
			log.Printf("opencode 运行时初始化失败（已跳过）: %v", err)
		} else {
			provider.RegisterRuntime(oc)
		}
	})
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
	for _, entry := range o.models.ListCached() {
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
	o.limMu.Lock()
	defer o.limMu.Unlock()
	l, ok := o.limiters[uid]
	if !ok {
		l = ratelimit.New(time.Duration(o.cfg.RatelimitInterval*float64(time.Second)), 300*time.Millisecond)
		o.limiters[uid] = l
	}
	return l
}

func (o *Orchestrator) modelCooldownUntil(uid, model string) float64 {
	o.mcMu.Lock()
	defer o.mcMu.Unlock()
	e, ok := o.modelCooldowns[uid+"|"+model]
	if !ok {
		return 0
	}
	if e.until <= nowSec() {
		delete(o.modelCooldowns, uid+"|"+model)
		return 0
	}
	return e.until
}

func (o *Orchestrator) markDailyModelLimit(acc *pool.Account, model string, raw []byte) bool {
	reset, reason, ok := dailyModelLimit(raw, 0)
	if !ok {
		return false
	}
	o.mcMu.Lock()
	o.modelCooldowns[acc.UID+"|"+model] = cdEntry{until: reset, reason: reason}
	o.mcMu.Unlock()
	_ = o.db.SetModelCooldown(acc.UID, model, reset, reason)
	return true
}

// penalizeAccount 对失败请求做统一处置：先看是否 6004 每日模型上限（有明确重置墙钟，
// 走 (账号,模型) 冷却），否则按错误分类表决定 禁用 / (账号,模型)负缓存 / 账号冷却 /
// 不罚（fail-fast 与 client）。返回的 errAction 由调用方据 Rotate/FailFast 决定是否换号。
// 这是账号池处罚的**唯一权威点**：handler 侧的错误日志一律 updatePool:false。
func (o *Orchestrator) penalizeAccount(acc *pool.Account, model string, ue *upstream.UpstreamError) errAction {
	// 6004 每日模型上限：上游给了明确重置时间，按 (账号,模型) 冷却到该时刻。
	if o.markDailyModelLimit(acc, model, ue.Raw) {
		return errAction{Rotate: true, ModelScoped: true, Reason: "该模型今日已达上限"}
	}
	now := nowSec()
	kind := classifyUpstream(ue.StatusCode, ue.Raw, ue.Header)
	act := actionFor(kind, ue.Raw, ue.Header, now)
	switch {
	case act.Disable:
		o.pool.SetEnabled(acc.UID, false, act.Reason)
	case act.ModelScoped && act.Cooldown > 0:
		until := now + act.Cooldown
		o.mcMu.Lock()
		o.modelCooldowns[acc.UID+"|"+model] = cdEntry{until: until, reason: act.Reason}
		o.mcMu.Unlock()
		_ = o.db.SetModelCooldown(acc.UID, model, until, act.Reason)
	case act.FailFast:
		// 请求自身的问题：不罚号、不换号，透传原文
	case act.Cooldown > 0:
		o.pool.OnFailure(acc.UID, act.Cooldown)
	default:
		// client / none：只换号不罚
	}
	return act
}

func (o *Orchestrator) modelAccountUIDs(model string) (map[string]bool, *apiError) {
	entries := o.models.ListCached()
	if len(entries) == 0 {
		return nil, errBody(503, "模型目录尚未就绪，请稍后重试或在管理端刷新模型目录", "model_unavailable")
	}
	var entry map[string]any
	for _, m := range entries {
		if m["id"] == model {
			entry = m
			break
		}
	}
	if entry == nil {
		return nil, errBody(400, "模型 "+model+" 不在当前模型目录中，请检查模型名或刷新模型目录", "invalid_request_error")
	}
	return o.modelEntryAccountUIDs(entry), nil
}

func (o *Orchestrator) modelEntryAccountUIDs(entry map[string]any) map[string]bool {
	out := map[string]bool{}
	if uids, ok := entry["account_uids"].([]string); ok {
		for _, u := range uids {
			out[u] = true
		}
		return out
	}
	profiles := map[string]bool{}
	if ps, ok := entry["profiles"].([]string); ok {
		for _, p := range ps {
			profiles[p] = true
		}
	}
	for _, a := range o.pool.Accounts() {
		if profiles[a.Profile] {
			out[a.UID] = true
		}
	}
	return out
}

// pickAccountFor returns the account picker entry point for a principal.
// Portal principals schedule ONLY inside their shared-group scope (HANDOFF §7
// 池隔离): failure/rotation/cooldowns stay within that candidate set and can
// never fall back to the private pool.
func (s *Server) pickAccountFor(principal *Principal, model, sessionKey string) (*pool.Account, *apiError) {
	if principal != nil && principal.UserID > 0 {
		return s.o.pickAccountExcludingIn(model, sessionKey, nil, principal.AccountScope)
	}
	return s.o.pickAccount(model, sessionKey)
}

func (o *Orchestrator) pickAccount(model, sessionKey string) (*pool.Account, *apiError) {
	return o.pickAccountExcluding(model, sessionKey, nil)
}

// pickAccountExcluding is pickAccount with a set of already-tried UIDs removed
// from the ready candidates, so failover can rotate through fresh accounts until
// the candidate set is exhausted. tried nil = first pick (no exclusions).
func (o *Orchestrator) pickAccountExcluding(model, sessionKey string, tried map[string]bool) (*pool.Account, *apiError) {
	return o.pickAccountExcludingIn(model, sessionKey, tried, nil)
}

// pickAccountExcludingIn is pickAccountExcluding with an optional UID scope
// (nil = whole pool). Portal principals pass AccountScope so neither the
// initial pick nor any failover rotation can leave the shared pool (HANDOFF
// §7 池隔离).
func (o *Orchestrator) pickAccountExcludingIn(model, sessionKey string, tried map[string]bool, scope map[string]bool) (*pool.Account, *apiError) {
	if scope != nil {
		return o.pickInScope(model, sessionKey, tried, scope)
	}
	allowed, aerr := o.modelAccountUIDs(model)
	if aerr != nil {
		return nil, aerr
	}
	privateOnly, err := o.db.PrivatePoolExcludedUIDs()
	if err != nil {
		return nil, errBody(503, "账号权限查询失败", "server_error")
	}
	for uid := range privateOnly {
		delete(allowed, uid)
	}
	if allowed != nil && len(allowed) == 0 {
		return nil, errBody(503, "模型 "+model+" 当前没有可调用账号，请检查账号状态或刷新模型目录", "model_unavailable")
	}
	ready := map[string]bool{}
	now := nowSec()
	for uid := range allowed {
		if tried[uid] {
			continue
		}
		if o.modelCooldownUntil(uid, model) <= now {
			ready[uid] = true
		}
	}
	if len(allowed) > 0 && len(ready) == 0 {
		if len(tried) > 0 {
			// 换号重试时候选已耗尽（都试过或都在冷却），交由调用方返回上一次的错误。
			return nil, errBody(503, "模型 "+model+" 的可用账号均已尝试或冷却", "auth_error")
		}
		return nil, errBody(429, "模型 "+model+" 的可用账号暂处于冷却，请稍后重试", "rate_limit_error")
	}
	// 会话粘性：此前绑定的账号若仍可用（在候选集、未模型冷却、账号冷却≤30s、启用、
	// 且本轮未试过），直接复用以保住上游 prompt cache 命中；否则解粘回池按权重重选并重绑。
	if sessionKey != "" {
		if uid := o.sessions.lookup(sessionKey); uid != "" && !tried[uid] {
			if ready[uid] {
				if a := o.pool.Get(uid); a != nil && a.Callable() && a.CooldownUntil-now <= sessionStickyMaxCooldown {
					return o.preferDomesticSession(sessionKey, a, ready, o.modelCostByUID(model, ready), o.expiryWindowDays()), nil
				}
			}
			o.sessions.unbind(sessionKey)
		}
	}
	acc := o.pool.Pick(ready, o.modelCostByUID(model, ready), o.expiryWindowDays())
	if acc == nil {
		return nil, errBody(503, "模型 "+model+" 无可用账号（全部冷却或额度耗尽），请检查账号状态", "auth_error")
	}
	if acc.CooldownUntil-now > sessionStickyMaxCooldown {
		return nil, errBody(503, "上游限流中，所有账号均在冷却，请稍后重试", "rate_limit_error")
	}
	if sessionKey != "" {
		o.sessions.bind(sessionKey, acc.UID)
	}
	return acc, nil
}

// pickInScope is the scoped candidate filter (portal shared pool): the same
// pick pipeline but candidates ∩ scope from the start, so a portal request
// never touches a private account — including via failover rotation.
func (o *Orchestrator) pickInScope(model, sessionKey string, tried map[string]bool, scope map[string]bool) (*pool.Account, *apiError) {
	allowed, aerr := o.modelAccountUIDs(model)
	if aerr != nil {
		return nil, aerr
	}
	if len(allowed) == 0 {
		return nil, errBody(503, "共享池中模型 "+model+" 暂无可用账号，请稍后重试", "model_unavailable")
	}
	ready := map[string]bool{}
	now := nowSec()
	scopedCandidates := 0
	for uid := range allowed {
		if tried[uid] || !scope[uid] {
			continue
		}
		if account := o.pool.Get(uid); account == nil || !account.Enabled {
			continue
		}
		scopedCandidates++
		if o.modelCooldownUntil(uid, model) <= now {
			ready[uid] = true
		}
	}
	if len(ready) == 0 {
		// Only real enabled scoped candidates in model cooldown justify a 429.
		if len(tried) == 0 && scopedCandidates > 0 {
			return nil, errBody(429, "模型 "+model+" 的授权账号暂处于冷却，请稍后重试", "rate_limit_error")
		}
		return nil, errPoolExhausted(model, tried)
	}
	if sessionKey != "" {
		if uid := o.sessions.lookup(sessionKey); uid != "" {
			if ready[uid] {
				if a := o.pool.Get(uid); a != nil && a.Callable() && a.CooldownUntil-now <= sessionStickyMaxCooldown {
					return o.preferDomesticSession(sessionKey, a, ready, o.modelCostByUID(model, ready), o.expiryWindowDays()), nil
				}
			}
			o.sessions.unbind(sessionKey)
		}
	}
	acc := o.pool.Pick(ready, o.modelCostByUID(model, ready), o.expiryWindowDays())
	if acc == nil {
		return nil, errPoolExhausted(model, nil)
	}
	if acc.CooldownUntil-now > sessionStickyMaxCooldown {
		return nil, errPoolExhausted(model, tried)
	}
	if sessionKey != "" {
		o.sessions.bind(sessionKey, acc.UID)
	}
	return acc, nil
}

func errPoolExhausted(model string, tried map[string]bool) *apiError {
	if len(tried) > 0 {
		return errBody(503, "共享池中模型 "+model+" 的可用账号均已尝试或冷却", "auth_error")
	}
	return errBody(503, "共享池中模型 "+model+" 暂无可用账号（冷却或额度耗尽），请稍后重试", "auth_error")
}

// modelCostByUID maps each ready account UID to its per-model cost coefficient
// (the model's per-site catalog credits), for cost-aware selection. Returns nil
// when cost_aware_routing is off or no cost data exists, so Pick stays cost-blind.
// Accounts on a site the catalog gave no value for are omitted → unknown, which
// Pick ranks last.
func (o *Orchestrator) modelCostByUID(model string, ready map[string]bool) map[string]float64 {
	settings, _ := o.db.GetSettings()
	if v, ok := settings["cost_aware_routing"]; ok && v != "1" {
		return nil
	}
	cbr := o.models.CreditsByRegion(model)
	if len(cbr) == 0 {
		return nil
	}
	out := map[string]float64{}
	for _, a := range o.pool.Accounts() {
		if !ready[a.UID] {
			continue
		}
		if c, ok := cbr[siterouting.ProfileSite(a.Profile)]; ok {
			out[a.UID] = c
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// expiryWindowDays is the configured look-ahead (setting expiry_priority_days)
// for soon-to-expire credit prioritization in Pick; falls back to the pool
// default on a missing/invalid value. Cheap now that GetSettings is memoized.
func (o *Orchestrator) expiryWindowDays() float64 {
	settings, _ := o.db.GetSettings()
	if v, ok := settings["expiry_priority_days"]; ok {
		if d, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && d > 0 {
			return d
		}
	}
	return pool.DefaultExpiryWindowDays
}

func (o *Orchestrator) resolveModel(model string) string {
	s, _ := o.db.GetSettings()
	aliases := parseModelAliases(s["model_aliases"])
	if real, ok := aliases[model]; ok {
		return real
	}
	return model
}

func (o *Orchestrator) enhanceBody(body map[string]any) map[string]any {
	if m, ok := body["model"].(string); ok && m != "" {
		body["model"] = o.resolveModel(m)
	}
	modelID, _ := body["model"].(string)
	maxOut := o.models.MaxOutputTokens(modelID)
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
		if eff := o.models.RequestEfforts(modelID, requested); eff != nil {
			dyn = map[string][]string{modelID: eff}
		}
	}
	body = reasoning.Sanitize(body, dyn)
	return body
}

func (o *Orchestrator) getHeaders(ctx context.Context, acc *pool.Account) (map[string]string, error) {
	mgr := o.manager(acc.UID)
	if mgr == nil {
		return nil, errBody(503, "账号凭据不可用", "auth_error")
	}
	h, err := mgr.GetChatHeadersContext(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, credentials.ErrLoginRequired) {
			return nil, &upstream.UpstreamError{StatusCode: 401, Raw: []byte(`{"error":{"message":"账号凭据失效，请重新授权","type":"auth_error"}}`)}
		}
		if isLocalNetworkFailure(err) {
			return nil, err
		}
		return nil, &upstream.UpstreamError{StatusCode: 503, Raw: []byte(`{"error":{"message":"账号凭据刷新暂时失败，请稍后重试","type":"upstream_error"}}`)}
	}
	return h, nil
}

func (o *Orchestrator) runOnce(ctx context.Context, acc *pool.Account, body map[string]any, sink func(string) error) (bool, error) {
	if o.cfg.Ratelimit && !o.limiter(acc.UID).Try() {
		wait := o.limiter(acc.UID).Wait
		var err error
		if lease, ok := ctx.Value(modelLeaseKey{}).(*modelLease); ok {
			err = lease.throttle(ctx, wait)
		} else {
			err = wait(ctx)
		}
		if err != nil {
			if errors.Is(err, ratelimit.ErrQueueFull) {
				return false, errBody(429, "账号等待队列已满，请稍后重试", "rate_limit_error")
			}
			return false, err
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if validate, ok := ctx.Value(portalDispatchCheckKey{}).(func(string, string) *apiError); ok {
		if aerr := validate(acc.UID, strOr(body["model"], "")); aerr != nil {
			return false, aerr
		}
	}
	headers, aerr := o.getHeaders(ctx, acc)
	if aerr != nil {
		return false, aerr
	}
	// Refresh waiters can outlive a permission change or client cancellation.
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if validate, ok := ctx.Value(portalDispatchCheckKey{}).(func(string, string) *apiError); ok {
		if aerr := validate(acc.UID, strOr(body["model"], "")); aerr != nil {
			return false, aerr
		}
	}
	url, _ := siterouting.ChatURLForProfile(acc.Profile)
	started := false
	var responseBytes int64
	var preamble []string
	preambleBytes := 0
	wrapped := func(line string) error {
		responseBytes += int64(len(line))
		if responseBytes > streamwatch.ResponseLimit(ctx) {
			return streamwatch.ErrResponseTooLarge
		}
		if !started {
			var chunk map[string]any
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			parsed := json.Unmarshal([]byte(data), &chunk) == nil
			terminal := data == "[DONE]"
			if choices, ok := chunk["choices"].([]any); ok {
				for _, value := range choices {
					choice, _ := value.(map[string]any)
					if finish, _ := choice["finish_reason"].(string); finish != "" {
						terminal = true
					}
				}
			}
			if parsed && !terminal && !upstream.HasOutput(chunk) {
				preambleBytes += len(line)
				if preambleBytes > 64*1024 {
					return streamwatch.ErrResponseTooLarge
				}
				preamble = append(preamble, line)
				return nil
			}
			started = true // From here, a writer error also prohibits replay.
			for _, prefix := range preamble {
				diagnostic(ctx).observe(prefix)
				if err := sink(prefix); err != nil {
					return err
				}
			}
			preamble = nil
		}
		diagnostic(ctx).observe(line)
		return sink(line)
	}
	client := o.upstreamClient
	if client == nil {
		client = upstream.Shared()
	}
	o.sessions.attempt(ctx, acc.UID)
	err := client.StreamUpstream(ctx, headers, body, url, wrapped)
	return started, err
}

// channelIdentityCompatBody retains the legacy helper's identity-only contract.
func channelIdentityCompatBody(body map[string]any) (map[string]any, bool) {
	return channelCompatibilityBody(body, 1)
}

// Retries are bounded to five total upstream attempts, including compatibility
// retries. No replay is allowed after output; every account stays in scope.
func (o *Orchestrator) openUpstream(ctx context.Context, acc *pool.Account, body map[string]any, model, sessionKey string, sink func(string) error, onRetryFail func(*pool.Account, *upstream.UpstreamError)) (*pool.Account, error) {
	return o.openUpstreamScoped(ctx, acc, body, model, sessionKey, sink, onRetryFail, nil)
}

func (o *Orchestrator) openUpstreamScoped(ctx context.Context, acc *pool.Account, body map[string]any, model, sessionKey string, sink func(string) error, onRetryFail func(*pool.Account, *upstream.UpstreamError), scope map[string]bool) (served *pool.Account, callErr error) {
	defer func() {
		if scope != nil {
			if ue, ok := callErr.(*upstream.UpstreamError); ok {
				copyErr := *ue
				copyErr.Raw = []byte(`{"error":{"message":"共享上游暂不可用，请稍后重试","type":"upstream_error"}}`)
				callErr = &copyErr
			}
		}
	}()
	const maxFailoverAttempts = 5
	tried := map[string]bool{}
	compatTries := 0
	compatUID := ""
	compatLevel := 0
	fingerprint := compatibilityFingerprint(body)
	for attempt := 0; ; attempt++ {

		sessionPhase(ctx, false)
		_, tracked := ctx.Value(sessionCallKey{}).(*sessionCall)
		if lease, leased := ctx.Value(modelLeaseKey{}).(*modelLease); leased || tracked {
			for revisionRetry := 0; ; revisionRetry++ {
				selection := &sessionSelection{key: sessionKey}
				selection.call, _ = ctx.Value(sessionCallKey{}).(*sessionCall)
				choose, chooseErr := o.accountSelector(model, acc.UID, tried, scope, selection)
				if chooseErr != nil {
					return acc, chooseErr
				}
				var selected *pool.Account
				var selectErr error
				if leased {
					selected, selectErr = lease.bindAccount(ctx, choose)
				} else {
					var aerr *apiError
					selected, aerr = choose(map[string]bool{})
					if aerr != nil {
						selectErr = aerr
					}
				}
				if selectErr != nil {
					return acc, selectErr
				}
				if selected == nil && !leased && revisionRetry < 3 {
					continue
				}
				acc = selected
				if acc == nil {
					return acc, errBody(503, "账号暂不可用", "model_unavailable")
				}
				// A private account may be moved to shared-only while this request
				// waits. Recheck ownership after waiting, before reading credentials.
				if scope == nil {
					excluded, err := o.db.PrivatePoolExcludedUIDs()
					if err != nil {
						return acc, errBody(503, "账号权限查询失败", "server_error")
					}
					if excluded[acc.UID] {
						if selection.action == "switch" || selection.action == "reselect" {
							o.sessions.reject(selection.key, selection.version)
						}
						return acc, errBody(403, "账号使用范围已变更，请重新请求", "permission_error")
					}
				}
				if validate, ok := ctx.Value(portalDispatchCheckKey{}).(func(string, string) *apiError); ok {
					if aerr := validate(acc.UID, model); aerr != nil {
						if selection.action == "switch" || selection.action == "reselect" {
							o.sessions.reject(selection.key, selection.version)
						}
						return acc, aerr
					}
				}
				// Catalog prices can change while this request is queued. Recheck
				// outside admission's mutex before consuming a manual preference.
				if selection.action == "switch" || selection.action == "reselect" {
					fresh := o.modelCostByUID(model, map[string]bool{selection.baseline: true, selection.target: true})
					base, baseOK := fresh[selection.baseline]
					target, targetOK := fresh[selection.target]
					if !baseOK || !targetOK || base < 0 || target < 0 || math.IsNaN(base) || math.IsNaN(target) || math.IsInf(base, 0) || math.IsInf(target, 0) || target > base+1e-9 {
						o.sessions.reject(selection.key, selection.version)
						if revisionRetry >= 3 {
							return acc, errBody(503, "会话账号调整频繁，请重试", "session_routing_changed")
						}
						continue
					}
				}
				if o.sessions.commit(selection, acc.UID) {
					break
				}
				if revisionRetry >= 3 {
					return acc, errBody(503, "会话账号调整频繁，请重试", "session_routing_changed")
				}
			}

			if sessionKey != "" && !tracked {
				o.sessions.bind(sessionKey, acc.UID)
			}
		}
		sessionPhase(ctx, true)
		tried[acc.UID] = true
		// Preserve client instructions and tool metadata on every account attempt.
		// Region failover must not rewrite the conversation or its identifiers.
		streamwatch.StartAttempt(ctx)
		attemptBody := body
		level := 0
		if o.cfg.ChannelIdentityCompat && siterouting.ProfileSite(acc.Profile) == siterouting.Domestic {
			level = o.sessions.compatibility(sessionKey, acc.UID, fingerprint)
			if !o.cfg.ChannelMetadataCompat && level > 1 {
				level = 0
			}
			if acc.UID == compatUID {
				level = compatLevel
			}
			if level > 0 {
				var matched bool
				attemptBody, matched = channelCompatibilityBody(body, level)
				if !matched {
					level = 0
					attemptBody = body
				}
			}
		}
		compatUID, compatLevel = "", 0
		if d := diagnostic(ctx); d != nil {
			d.FinishReason, d.ErrorKind, d.Started = "", "", false
			d.EffectiveLimits = outputLimits(attemptBody)
			d.Compatibility = compatibilityName(level)
		}
		started, err := o.runOnce(ctx, acc, attemptBody, sink)
		if d := diagnostic(ctx); d != nil {
			d.ErrorKind = diagnosticErrorKind(ctx, err)
		}
		if err == nil {
			o.sessions.rememberCompatibility(sessionKey, acc.UID, fingerprint, level)
		}
		ue, upstreamErr := err.(*upstream.UpstreamError)
		denied := upstreamErr && isChannelDenied(ue.StatusCode, string(ue.Raw))
		if denied && started {
			o.sessions.unbindMatching(sessionKey, acc.UID)
		}

		o.sessions.outcome(ctx, acc.UID, err)
		status := 200
		if ue, ok := err.(*upstream.UpstreamError); ok {
			status = ue.StatusCode
		}
		streamwatch.AttemptResult(ctx, status)
		if err == nil {
			return acc, nil
		}
		if errors.Is(err, streamwatch.ErrResponseTooLarge) {
			return acc, err
		}
		if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return acc, err
		}
		if aerr, ok := err.(*apiError); ok && (aerr.status == 429 || aerr.status == 401 || aerr.status == 403) {
			return acc, err
		}
		if started {
			if denied {
				return acc, err
			}
			// 流已开始又中断：软冷却（可能是上游中途掉线），不重试已开始的流。
			if !isLocalNetworkFailure(err) {
				o.pool.OnFailure(acc.UID, cooldownSoft)
			}
			return acc, err
		}
		if !upstreamErr {
			// Typed DNS/local routing failures are not account failures. Fail
			// promptly without poisoning healthy accounts or retrying the same path.
			if !isLocalNetworkFailure(err) {
				o.pool.OnFailure(acc.UID, cooldownSoft)
			}
			return acc, err
		}
		if denied {
			log.Printf("渠道拒绝诊断: profile=%s model=%s compatibility=%s compat_enabled=%t started=%t attempt=%d", acc.Profile, model, compatibilityName(level), o.cfg.ChannelIdentityCompat, started, attempt+1)
		}
		if denied && o.cfg.ChannelIdentityCompat && compatTries < 2 && attempt+1 < maxFailoverAttempts && siterouting.ProfileSite(acc.Profile) == siterouting.Domestic {
			maxLevel := 1
			if o.cfg.ChannelMetadataCompat {
				maxLevel = 2
			}
			for next := level + 1; next <= maxLevel; next++ {
				if _, matched := channelCompatibilityBody(body, next); matched {
					compatUID, compatLevel = acc.UID, next
					compatTries++
					delete(tried, acc.UID)
					break
				}
			}
			if compatUID != "" {
				continue
			}
		}

		// 分类并对该账号施加处罚（禁用/负缓存/冷却/不罚），返回是否应换号。
		act := o.penalizeAccount(acc, model, ue)
		if act.FailFast || !act.Rotate {
			return acc, err
		}
		if sessionKey != "" {
			o.sessions.unbindMatching(sessionKey, acc.UID)
		}
		if attempt+1 >= maxFailoverAttempts {
			return acc, err
		}
		alt, aerr := o.pickAccountExcludingIn(model, sessionKey, tried, scope)
		if aerr != nil {
			return acc, err // 候选耗尽：返回最后一次的上游错误
		}
		if onRetryFail != nil {
			onRetryFail(acc, ue)
		}
		acc = alt
	}
}

// Capture authorized candidates outside admission's mutex. Dispatch consults
// live cooldown/enable state; portal authority is revalidated just before sending.
// The selector is synchronous and does not perform network or database I/O.
func (o *Orchestrator) accountSelector(model, preferred string, tried map[string]bool, scope map[string]bool, routing ...*sessionSelection) (func(map[string]bool) (*pool.Account, *apiError), *apiError) {
	allowed, aerr := o.modelAccountUIDs(model)
	if aerr != nil {
		return nil, aerr
	}
	if scope == nil {
		excluded, err := o.db.PrivatePoolExcludedUIDs()
		if err != nil {
			return nil, errBody(503, "账号权限查询失败", "server_error")
		}
		for uid := range excluded {
			delete(allowed, uid)
		}
	} else {
		for uid := range allowed {
			if !scope[uid] {
				delete(allowed, uid)
			}
		}
	}
	for uid := range tried {
		delete(allowed, uid)
	}
	cost := o.modelCostByUID(model, allowed)
	window := o.expiryWindowDays()
	var migrationFrom, migrationTarget string
	return func(busy map[string]bool) (*pool.Account, *apiError) {
		ready := map[string]bool{}
		now := nowSec()
		for _, candidate := range o.pool.Accounts() {
			// Keep the original selected account's short account-cooldown grace;
			// model cooldown remains a strict exclusion as in the original picker.
			preferredGrace := candidate.UID == preferred && candidate.Callable() && candidate.CooldownUntil-now <= sessionStickyMaxCooldown
			if !allowed[candidate.UID] || (!candidate.Healthy(now) && !preferredGrace) || o.modelCooldownUntil(candidate.UID, model) > now {
				continue
			}
			ready[candidate.UID] = true
		}
		if len(ready) == 0 {
			return nil, errPoolExhausted(model, tried)
		}
		// Preserve original routing before considering occupancy. Busy accounts
		// must not lose affinity or cost/expiry preference merely to fill slots.

		routePreferred := preferred
		if len(routing) > 0 && routing[0].key != "" {
			selection := routing[0]
			uid, action, target, baseline, version := o.sessions.route(selection.key)
			selection.version = version
			selection.action, selection.target, selection.baseline = action, target, baseline
			if uid != "" {
				routePreferred = uid
			}
			switch action {
			case "reselect":
				routePreferred = ""
				baseCost, known := cost[baseline]
				alternatives := map[string]bool{}
				for uid := range ready {
					if value, ok := cost[uid]; uid != baseline && known && ok && baseCost >= 0 && !math.IsNaN(baseCost) && !math.IsInf(baseCost, 0) && value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0) && value <= baseCost+1e-9 {
						alternatives[uid] = true
					}
				}
				if len(alternatives) == 0 {
					o.sessions.reject(selection.key, version, "没有其他同成本或更低成本的健康授权账号，保留原选择")
					return nil, nil
				}
				ready = alternatives
				routePreferred = preferred
			case "switch":
				baseCost, baseKnown := cost[baseline]
				targetCost, targetKnown := cost[target]
				candidate := o.pool.Get(target)
				if !ready[target] || candidate == nil || !candidate.Healthy(now) || !baseKnown || !targetKnown || baseCost < 0 || targetCost < 0 || math.IsNaN(baseCost) || math.IsNaN(targetCost) || math.IsInf(baseCost, 0) || math.IsInf(targetCost, 0) || targetCost > baseCost+1e-9 {
					o.sessions.reject(selection.key, version)
					return nil, nil // Retry selection against the new version on the queue wakeup.
				}
				routePreferred = target
			}
		}
		var selected *pool.Account
		if ready[routePreferred] {

			selected = o.pool.Get(routePreferred)
			if len(routing) == 0 || routing[0].action == "" {
				key := func() string {
					if len(routing) > 0 {
						return routing[0].key
					}
					return ""
				}()
				if migrationFrom == routePreferred && ready[migrationTarget] && !o.sessions.manuallyPinned(key, routePreferred) {
					selected = o.pool.Get(migrationTarget)
				} else {
					selected = o.preferDomesticSession(key, selected, ready, cost, window)
					if selected != nil && selected.UID != routePreferred {
						migrationFrom, migrationTarget = routePreferred, selected.UID
					} else {
						migrationFrom, migrationTarget = "", ""
					}
				}
			}
		} else {
			selected = o.pool.Pick(ready, cost, window)
			if selected != nil {
				preferred = selected.UID
			}
		}
		if selected != nil && busy[selected.UID] {
			return nil, nil
		}
		if selected != nil && len(routing) > 0 && routing[0].action == "reselect" {
			routing[0].target = selected.UID
		}
		return selected, nil
	}, nil
}

// Migrate automatic paid international bindings only when the cheapest healthy
// authorized group has a domestic option. Explicit administrator pins win.
func (o *Orchestrator) preferDomesticSession(key string, current *pool.Account, ready map[string]bool, costs map[string]float64, window float64) *pool.Account {
	if current == nil || siterouting.ProfileSite(current.Profile) != siterouting.International || o.sessions.manuallyPinned(key, current.UID) {
		return current
	}
	base, known := costs[current.UID]
	if !known || base <= 0 || math.IsNaN(base) || math.IsInf(base, 0) {
		return current
	}
	minimum := math.Inf(1)
	for _, candidate := range o.pool.Accounts() {
		if !ready[candidate.UID] || !candidate.Healthy(nowSec()) {
			continue
		}
		if cost, ok := costs[candidate.UID]; ok && cost >= 0 && !math.IsNaN(cost) && !math.IsInf(cost, 0) && cost < minimum {
			minimum = cost
		}
	}
	if minimum <= 0 || minimum > base+1e-9 {
		return current
	}
	for _, candidate := range o.pool.Accounts() {
		cost, ok := costs[candidate.UID]
		if ready[candidate.UID] && candidate.Healthy(nowSec()) && siterouting.ProfileSite(candidate.Profile) == siterouting.Domestic && ok && math.Abs(cost-minimum) <= 1e-9 {
			if selected := o.pool.Pick(ready, costs, window); selected != nil {
				return selected
			}
		}
	}
	return current
}

func (o *Orchestrator) refreshCreditsFor(ctx context.Context, acc *pool.Account) {
	mgr := o.manager(acc.UID)
	if mgr == nil {
		return
	}
	cr, err := billing.FetchCredits(ctx, mgr)
	if err != nil {
		// 与上游一致：额度查询失败要留日志，否则「刷新额度没反应」无从排查
		log.Printf("额度查询失败 %s: %v", acc.UID, err)
		return
	}
	o.pool.SetCredits(acc.UID, &cr.Remain, &cr.Total, cr.ExpireAt, cr.Packages)
	fields := map[string]any{"credits_remaining": cr.Remain, "credits_total": cr.Total}
	if cr.ExpireAt != nil {
		fields["credits_expire_at"] = strconv.FormatFloat(*cr.ExpireAt, 'f', -1, 64)
	} else {
		// 之前有到期时间、现在没有了要清掉，否则 DB 留着过期的旧时间戳
		fields["credits_expire_at"] = ""
	}
	_ = o.db.SetAccountState(acc.UID, fields)
	// 仅在余额 > 0 时自动解冻：余额耗尽的账号不该被 credit 刷新顺手解冻
	if cr.Remain > 0 {
		o.pool.ClearCooldown(acc.UID)
	}
}

// refreshAllCredits refreshes every account's credits concurrently. Done in
// parallel because each account issues several sequential upstream billing
// calls (~seconds each); refreshing serially made the manual "刷新额度" button
// exceed the client's 30s timeout once more than one account was configured.
func (o *Orchestrator) refreshAllCredits(ctx context.Context) {
	var wg sync.WaitGroup
	for _, a := range o.pool.Accounts() {
		wg.Add(1)
		go func(acc *pool.Account) {
			defer wg.Done()
			o.refreshCreditsFor(ctx, acc)
		}(a)
	}
	wg.Wait()
}

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
