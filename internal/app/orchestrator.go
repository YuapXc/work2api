package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"work2api/internal/config"
	"work2api/internal/core/provider"
	"work2api/internal/crypto"
	"work2api/internal/opencode"
	"work2api/internal/qoder"
	"work2api/internal/store"
	"work2api/internal/workbuddy/billing"
	"work2api/internal/workbuddy/credentials"
	"work2api/internal/workbuddy/desensitize"
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

// Principal is the authenticated application.
type Principal struct{ AppName string }

// Orchestrator holds shared runtime state and the request pipeline.
type Orchestrator struct {
	cfg      *config.Config
	db       *store.DB
	crypto   *crypto.Manager
	pool     *pool.Pool
	models   *models.Registry
	managers map[string]*credentials.Manager

	limMu    sync.Mutex
	limiters map[string]*ratelimit.Limiter

	mcMu           sync.Mutex
	modelCooldowns map[string]cdEntry
	projectAuths   string
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
		managers:       map[string]*credentials.Manager{},
		limiters:       map[string]*ratelimit.Limiter{},
		modelCooldowns: map[string]cdEntry{},
		projectAuths:   projectAuths,
		sessions:       newSessionRouter(),
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
	for _, f := range credentials.FindAuthFiles("", projectAuths) {
		mgr := credentials.NewManager(f)
		s := mgr.Summary()
		uid, _ := s["uid"].(string)
		if uid == "" {
			continue
		}
		if raw, err := mgr.RawSession(); err == nil {
			_, _ = db.UpsertAccount(map[string]any{"auth": raw.Auth, "account": raw.Account})
		}
		o.managers[uid] = mgr
		creds[uid] = mgr
	}
	o.pool = pool.New(creds, projectAuths)
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
	o.models = models.New(o.pool, db)
	registerRuntimes(cfg.DataDir)
	return o, nil
}

// registerRuntimes wires the non-default provider inference runtimes (qoder,
// opencode) into the shared dispatcher. Each is self-contained and stays inert
// when it has no credentials/config, so registration is unconditional and
// idempotent per process. workbuddy remains the default (un-namespaced) path.
var runtimesOnce sync.Once

func registerRuntimes(dataDir string) {
	runtimesOnce.Do(func() {
		provider.RegisterRuntime(qoder.New())
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
	return &Principal{AppName: name}, nil
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
	out := map[string]bool{}
	if uids, ok := entry["account_uids"].([]string); ok {
		for _, u := range uids {
			out[u] = true
		}
		return out, nil
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
	return out, nil
}

func (o *Orchestrator) pickAccount(model, sessionKey string) (*pool.Account, *apiError) {
	allowed, aerr := o.modelAccountUIDs(model)
	if aerr != nil {
		return nil, aerr
	}
	if allowed != nil && len(allowed) == 0 {
		return nil, errBody(503, "模型 "+model+" 当前没有可调用账号，请检查账号状态或刷新模型目录", "model_unavailable")
	}
	ready := map[string]bool{}
	now := nowSec()
	for uid := range allowed {
		if o.modelCooldownUntil(uid, model) <= now {
			ready[uid] = true
		}
	}
	if len(allowed) > 0 && len(ready) == 0 {
		return nil, errBody(429, "模型 "+model+" 的可用账号均已达到每日上限，请稍后重试", "rate_limit_error")
	}
	// 会话粘性：此前绑定的账号若仍可用（在候选集、未模型冷却、账号冷却≤30s、启用），
	// 直接复用以保住上游 prompt cache 命中；否则解粘回池按权重重选并重绑。
	if sessionKey != "" {
		if uid := o.sessions.lookup(sessionKey); uid != "" {
			if ready[uid] {
				for _, a := range o.pool.Accounts() {
					if a.UID == uid {
						if a.Enabled && a.CooldownUntil-now <= 30 {
							return a, nil
						}
						break
					}
				}
			}
			o.sessions.unbind(sessionKey)
		}
	}
	acc := o.pool.Pick(ready)
	if acc == nil {
		return nil, errBody(503, "模型 "+model+" 无可用账号（全部冷却或额度耗尽），请检查账号状态", "auth_error")
	}
	if acc.CooldownUntil-now > 30 {
		return nil, errBody(503, "上游限流中，所有账号均在冷却，请稍后重试", "rate_limit_error")
	}
	if sessionKey != "" {
		o.sessions.bind(sessionKey, acc.UID)
	}
	return acc, nil
}

func (o *Orchestrator) modelReadyUIDs(model string) map[string]bool {
	allowed, _ := o.modelAccountUIDs(model)
	if allowed == nil {
		allowed = map[string]bool{}
		for _, a := range o.pool.Accounts() {
			allowed[a.UID] = true
		}
	}
	ready := map[string]bool{}
	now := nowSec()
	for uid := range allowed {
		if o.modelCooldownUntil(uid, model) <= now {
			ready[uid] = true
		}
	}
	return ready
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
		if eff := o.models.ReasoningEfforts(modelID); eff != nil {
			dyn = map[string][]string{modelID: eff}
		}
	}
	body = reasoning.Sanitize(body, dyn)
	if o.cfg.Desensitize {
		body = desensitize.Body(body, desensitize.Options{Roles: []string{"system", "developer"}})
	}
	return body
}

func (o *Orchestrator) getHeaders(acc *pool.Account) (map[string]string, *apiError) {
	mgr := o.managers[acc.UID]
	if mgr == nil {
		return nil, errBody(503, "账号凭据不可用", "auth_error")
	}
	h, err := mgr.GetHeaders()
	if err != nil {
		o.pool.OnFailure(acc.UID, cooldownHard)
		return nil, errBody(503, "账号 token 刷新失败，请到 WebUI 重新扫码登录", "auth_error")
	}
	return h, nil
}

func (o *Orchestrator) runOnce(ctx context.Context, acc *pool.Account, body map[string]any, sink func(string) error) (bool, error) {
	if o.cfg.Ratelimit {
		o.limiter(acc.UID).Wait()
	}
	headers, aerr := o.getHeaders(acc)
	if aerr != nil {
		return false, aerr
	}
	url, _ := siterouting.ChatURLForProfile(acc.Profile)
	started := false
	wrapped := func(line string) error {
		started = true
		return sink(line)
	}
	err := upstream.Shared().StreamUpstream(ctx, headers, body, url, wrapped)
	return started, err
}

func (o *Orchestrator) openUpstream(ctx context.Context, acc *pool.Account, body map[string]any, model, sessionKey string, sink func(string) error, onRetryFail func(*pool.Account, *upstream.UpstreamError)) (*pool.Account, error) {
	started, err := o.runOnce(ctx, acc, body, sink)
	if err == nil {
		return acc, nil
	}
	if started {
		// 流已开始又中断：软冷却（可能是上游中途掉线），不重试已开始的流。
		o.pool.OnFailure(acc.UID, cooldownSoft)
		return acc, err
	}
	ue, ok := err.(*upstream.UpstreamError)
	if !ok {
		o.pool.OnFailure(acc.UID, cooldownSoft)
		return acc, err
	}
	// 分类并对首个账号施加处罚（禁用/负缓存/冷却/不罚），返回是否应换号。
	act := o.penalizeAccount(acc, model, ue)
	if act.FailFast || !act.Rotate {
		return acc, err
	}
	if o.pool.HealthyCount(o.modelReadyUIDs(model)) < 1 {
		return acc, err
	}
	if onRetryFail != nil {
		onRetryFail(acc, ue)
	}
	alt, aerr := o.pickAccount(model, sessionKey)
	if aerr != nil {
		return acc, err
	}
	started2, err2 := o.runOnce(ctx, alt, body, sink)
	if err2 != nil {
		if ue2, ok := err2.(*upstream.UpstreamError); ok {
			o.penalizeAccount(alt, model, ue2)
		} else if !started2 {
			o.pool.OnFailure(alt.UID, cooldownSoft)
		}
	}
	return alt, err2
}

func (o *Orchestrator) refreshCreditsFor(ctx context.Context, acc *pool.Account) {
	mgr := o.managers[acc.UID]
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
