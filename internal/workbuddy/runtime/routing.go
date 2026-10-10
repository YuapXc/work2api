package runtime

import (
	"context"
	"errors"
	"log"
	"math"
	"strconv"
	"strings"
	"work2api/internal/core/provider"
	"work2api/internal/streamwatch"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/siterouting"
	"work2api/internal/workbuddy/upstream"
)

type apiError = provider.APIError

var errBody = provider.NewAPIError

func (o *Runtime) ModelCooldownUntil(uid, model string) float64 {
	o.CooldownMu.Lock()
	defer o.CooldownMu.Unlock()
	e, ok := o.Cooldowns[uid+"|"+model]
	if !ok {
		return 0
	}
	if e.Until <= nowSec() {
		delete(o.Cooldowns, uid+"|"+model)
		return 0
	}
	return e.Until
}

func (o *Runtime) MarkDailyModelLimit(acc *pool.Account, model string, raw []byte) bool {
	reset, reason, ok := DailyModelLimit(raw, 0)
	if !ok {
		return false
	}
	o.CooldownMu.Lock()
	o.Cooldowns[acc.UID+"|"+model] = CooldownEntry{Until: reset, Reason: reason}
	o.CooldownMu.Unlock()
	_ = o.db.SetModelCooldown(acc.UID, model, reset, reason)
	return true
}

// PenalizeAccount 对失败请求做统一处置：先看是否 6004 每日模型上限（有明确重置墙钟，
// 走 (账号,模型) 冷却），否则按错误分类表决定 禁用 / (账号,模型)负缓存 / 账号冷却 /
// 不罚（fail-fast 与 client）。返回的 ErrAction 由调用方据 Rotate/FailFast 决定是否换号。
// 这是账号池处罚的**唯一权威点**：handler 侧的错误日志一律 updatePool:false。

func (o *Runtime) PenalizeAccount(acc *pool.Account, model string, ue *upstream.UpstreamError) ErrAction {
	// 6004 每日模型上限：上游给了明确重置时间，按 (账号,模型) 冷却到该时刻。
	if o.MarkDailyModelLimit(acc, model, ue.Raw) {
		return ErrAction{Rotate: true, ModelScoped: true, Reason: "该模型今日已达上限"}
	}
	now := nowSec()
	kind := ClassifyUpstream(ue.StatusCode, ue.Raw, ue.Header)
	act := ActionFor(kind, ue.Raw, ue.Header, now)
	switch {
	case act.Disable:
		o.Pool.SetEnabled(acc.UID, false, act.Reason)
	case act.ModelScoped && act.Cooldown > 0:
		until := now + act.Cooldown
		o.CooldownMu.Lock()
		o.Cooldowns[acc.UID+"|"+model] = CooldownEntry{Until: until, Reason: act.Reason}
		o.CooldownMu.Unlock()
		_ = o.db.SetModelCooldown(acc.UID, model, until, act.Reason)
	case act.FailFast:
		// 请求自身的问题：不罚号、不换号，透传原文
	case act.Cooldown > 0:
		o.Pool.OnFailure(acc.UID, act.Cooldown)
	default:
		// client / none：只换号不罚
	}
	return act
}

func (o *Runtime) ModelAccountUIDs(model string) (map[string]bool, *apiError) {
	entries := o.Catalog.ListCached()
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
	return o.ModelEntryAccountUIDs(entry), nil
}

func (o *Runtime) ModelEntryAccountUIDs(entry map[string]any) map[string]bool {
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
	for _, a := range o.Pool.Accounts() {
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

func (o *Runtime) PickAccount(model, sessionKey string) (*pool.Account, *apiError) {
	return o.PickAccountExcluding(model, sessionKey, nil)
}

// PickAccountExcluding is PickAccount with a set of already-tried UIDs removed
// from the ready candidates, so failover can rotate through fresh accounts until
// the candidate set is exhausted. tried nil = first pick (no exclusions).

func (o *Runtime) PickAccountExcluding(model, sessionKey string, tried map[string]bool) (*pool.Account, *apiError) {
	return o.PickAccountExcludingIn(model, sessionKey, tried, nil)
}

// PickAccountExcludingIn is PickAccountExcluding with an optional UID scope
// (nil = whole pool). Portal principals pass AccountScope so neither the
// initial pick nor any failover rotation can leave the shared pool (HANDOFF
// §7 池隔离).

func (o *Runtime) PickAccountExcludingIn(model, sessionKey string, tried map[string]bool, scope map[string]bool) (*pool.Account, *apiError) {
	if scope != nil {
		return o.PickInScope(model, sessionKey, tried, scope)
	}
	allowed, aerr := o.ModelAccountUIDs(model)
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
		if o.ModelCooldownUntil(uid, model) <= now {
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
		if uid := o.Sessions.Lookup(sessionKey); uid != "" && !tried[uid] {
			if ready[uid] {
				if a := o.Pool.Get(uid); a != nil && a.Callable() && a.CooldownUntil-now <= SessionStickyMaxCooldown {
					return o.PreferDomesticSession(sessionKey, a, ready, o.ModelCostByUID(model, ready), o.ExpiryWindowDays()), nil
				}
			}
			o.Sessions.Unbind(sessionKey)
		}
	}
	acc := o.Pool.Pick(ready, o.ModelCostByUID(model, ready), o.ExpiryWindowDays())
	if acc == nil {
		return nil, errBody(503, "模型 "+model+" 无可用账号（全部冷却或额度耗尽），请检查账号状态", "auth_error")
	}
	if acc.CooldownUntil-now > SessionStickyMaxCooldown {
		return nil, errBody(503, "上游限流中，所有账号均在冷却，请稍后重试", "rate_limit_error")
	}
	if sessionKey != "" {
		o.Sessions.Bind(sessionKey, acc.UID)
	}
	return acc, nil
}

// PickInScope is the scoped candidate filter (portal shared pool): the same
// pick pipeline but candidates ∩ scope from the start, so a portal request
// never touches a private account — including via failover rotation.

func (o *Runtime) PickInScope(model, sessionKey string, tried map[string]bool, scope map[string]bool) (*pool.Account, *apiError) {
	allowed, aerr := o.ModelAccountUIDs(model)
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
		if account := o.Pool.Get(uid); account == nil || !account.Enabled {
			continue
		}
		scopedCandidates++
		if o.ModelCooldownUntil(uid, model) <= now {
			ready[uid] = true
		}
	}
	if len(ready) == 0 {
		// Only real enabled scoped candidates in model cooldown justify a 429.
		if len(tried) == 0 && scopedCandidates > 0 {
			return nil, errBody(429, "模型 "+model+" 的授权账号暂处于冷却，请稍后重试", "rate_limit_error")
		}
		return nil, ErrPoolExhausted(model, tried)
	}
	if sessionKey != "" {
		if uid := o.Sessions.Lookup(sessionKey); uid != "" {
			if ready[uid] {
				if a := o.Pool.Get(uid); a != nil && a.Callable() && a.CooldownUntil-now <= SessionStickyMaxCooldown {
					return o.PreferDomesticSession(sessionKey, a, ready, o.ModelCostByUID(model, ready), o.ExpiryWindowDays()), nil
				}
			}
			o.Sessions.Unbind(sessionKey)
		}
	}
	acc := o.Pool.Pick(ready, o.ModelCostByUID(model, ready), o.ExpiryWindowDays())
	if acc == nil {
		return nil, ErrPoolExhausted(model, nil)
	}
	if acc.CooldownUntil-now > SessionStickyMaxCooldown {
		return nil, ErrPoolExhausted(model, tried)
	}
	if sessionKey != "" {
		o.Sessions.Bind(sessionKey, acc.UID)
	}
	return acc, nil
}

func (o *Runtime) ModelCostByUID(model string, ready map[string]bool) map[string]float64 {
	settings, _ := o.db.GetSettings()
	if v, ok := settings["cost_aware_routing"]; ok && v != "1" {
		return nil
	}
	cbr := o.Catalog.CreditsByRegion(model)
	if len(cbr) == 0 {
		return nil
	}
	out := map[string]float64{}
	for _, a := range o.Pool.Accounts() {
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

// ExpiryWindowDays is the configured look-ahead (setting expiry_priority_days)
// for soon-to-expire credit prioritization in Pick; falls back to the pool
// default on a missing/invalid value. Cheap now that GetSettings is memoized.

func (o *Runtime) ExpiryWindowDays() float64 {
	settings, _ := o.db.GetSettings()
	if v, ok := settings["expiry_priority_days"]; ok {
		if d, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && d > 0 {
			return d
		}
	}
	return pool.DefaultExpiryWindowDays
}

func (o *Runtime) EnhanceBody(body map[string]any) map[string]any { return o.enhanceBody(body) }

func (o *Runtime) OpenUpstream(ctx context.Context, acc *pool.Account, body map[string]any, model, sessionKey string, sink func(string) error, onRetryFail func(*pool.Account, *upstream.UpstreamError)) (*pool.Account, error) {
	return o.OpenUpstreamScoped(ctx, acc, body, model, sessionKey, sink, onRetryFail, nil)
}

func (o *Runtime) OpenUpstreamScoped(ctx context.Context, acc *pool.Account, body map[string]any, model, sessionKey string, sink func(string) error, onRetryFail func(*pool.Account, *upstream.UpstreamError), scope map[string]bool) (served *pool.Account, callErr error) {
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
	fingerprint := CompatibilityFingerprint(body)
	for attempt := 0; ; attempt++ {

		SessionPhase(ctx, false)
		_, tracked := ctx.Value(SessionCallKey{}).(*SessionCall)
		if selectAccount := provider.CurrentInvocation(ctx).Select; selectAccount != nil || tracked {
			leased := selectAccount != nil
			for revisionRetry := 0; ; revisionRetry++ {
				selection := &SessionSelection{Key: sessionKey}
				selection.Call, _ = ctx.Value(SessionCallKey{}).(*SessionCall)
				choose, chooseErr := o.AccountSelector(model, acc.UID, tried, scope, selection)
				if chooseErr != nil {
					return acc, chooseErr
				}
				var selected *pool.Account
				var selectErr error
				if leased {
					ref, err := selectAccount(ctx, func(busy map[string]bool) (provider.AccountRef, error) {
						a, e := choose(busy)
						if e != nil {
							return provider.AccountRef{}, e
						}
						if a == nil {
							return provider.AccountRef{}, nil
						}
						return provider.AccountRef{Provider: "workbuddy", LocalID: a.UID}, nil
					})
					selectErr = err
					if ref.Provider == "workbuddy" {
						selected = o.Pool.Get(ref.LocalID)
					}
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
						if selection.Action == "switch" || selection.Action == "reselect" {
							o.Sessions.Reject(selection.Key, selection.Version)
						}
						return acc, errBody(403, "账号使用范围已变更，请重新请求", "permission_error")
					}
				}
				if validate := provider.CurrentInvocation(ctx).Check; validate != nil {
					if aerr := validate(ctx, provider.AccountRef{Provider: "workbuddy", LocalID: acc.UID}, model); aerr != nil {
						if selection.Action == "switch" || selection.Action == "reselect" {
							o.Sessions.Reject(selection.Key, selection.Version)
						}
						return acc, aerr
					}
				}
				// Catalog prices can change while this request is queued. Recheck
				// outside admission's mutex before consuming a manual preference.
				if selection.Action == "switch" || selection.Action == "reselect" {
					fresh := o.ModelCostByUID(model, map[string]bool{selection.Baseline: true, selection.Target: true})
					base, baseOK := fresh[selection.Baseline]
					target, targetOK := fresh[selection.Target]
					if !baseOK || !targetOK || base < 0 || target < 0 || math.IsNaN(base) || math.IsNaN(target) || math.IsInf(base, 0) || math.IsInf(target, 0) || target > base+1e-9 {
						o.Sessions.Reject(selection.Key, selection.Version)
						if revisionRetry >= 3 {
							return acc, errBody(503, "会话账号调整频繁，请重试", "session_routing_changed")
						}
						continue
					}
				}
				if o.Sessions.Commit(selection, acc.UID) {
					break
				}
				if revisionRetry >= 3 {
					return acc, errBody(503, "会话账号调整频繁，请重试", "session_routing_changed")
				}
			}

			if sessionKey != "" && !tracked {
				o.Sessions.Bind(sessionKey, acc.UID)
			}
		}
		SessionPhase(ctx, true)
		tried[acc.UID] = true
		// Preserve client instructions and tool metadata on every account attempt.
		// Region failover must not rewrite the conversation or its identifiers.
		streamwatch.StartAttempt(ctx)
		attemptBody := body
		level := 0
		if o.cfg.ChannelIdentityCompat && siterouting.ProfileSite(acc.Profile) == siterouting.Domestic {
			level = o.Sessions.Compatibility(sessionKey, acc.UID, fingerprint)
			if !o.cfg.ChannelMetadataCompat && level > 1 {
				level = 0
			}
			if acc.UID == compatUID {
				level = compatLevel
			}
			if level > 0 {
				var matched bool
				attemptBody, matched = ChannelCompatibilityBody(body, level)
				if !matched {
					level = 0
					attemptBody = body
				}
			}
		}
		compatUID, compatLevel = "", 0
		if d := Diagnostic(ctx); d != nil {
			d.FinishReason, d.ErrorKind, d.Started = "", "", false
			d.EffectiveLimits = OutputLimits(attemptBody)
			d.Compatibility = CompatibilityName(level)
		}
		started, err := o.RunOnce(ctx, acc, attemptBody, sink)
		if d := Diagnostic(ctx); d != nil {
			d.ErrorKind = DiagnosticErrorKind(ctx, err)
		}
		if err == nil {
			o.Sessions.RememberCompatibility(sessionKey, acc.UID, fingerprint, level)
		}
		ue, upstreamErr := err.(*upstream.UpstreamError)
		denied := upstreamErr && IsChannelDenied(ue.StatusCode, string(ue.Raw))
		if denied && started {
			o.Sessions.UnbindMatching(sessionKey, acc.UID)
		}

		o.Sessions.Outcome(ctx, acc.UID, err)
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
		if aerr, ok := err.(*apiError); ok && (aerr.Status == 429 || aerr.Status == 401 || aerr.Status == 403) {
			return acc, err
		}
		if started {
			if denied {
				return acc, err
			}
			// 流已开始又中断：软冷却（可能是上游中途掉线），不重试已开始的流。
			if !LocalNetworkFailure(err) {
				o.Pool.OnFailure(acc.UID, CooldownSoft)
			}
			return acc, err
		}
		if !upstreamErr {
			// Typed DNS/local routing failures are not account failures. Fail
			// promptly without poisoning healthy accounts or retrying the same path.
			if !LocalNetworkFailure(err) {
				o.Pool.OnFailure(acc.UID, CooldownSoft)
			}
			return acc, err
		}
		if denied {
			log.Printf("渠道拒绝诊断: profile=%s model=%s compatibility=%s compat_enabled=%t started=%t attempt=%d", acc.Profile, model, CompatibilityName(level), o.cfg.ChannelIdentityCompat, started, attempt+1)
		}
		if denied && o.cfg.ChannelIdentityCompat && compatTries < 2 && attempt+1 < maxFailoverAttempts && siterouting.ProfileSite(acc.Profile) == siterouting.Domestic {
			maxLevel := 1
			if o.cfg.ChannelMetadataCompat {
				maxLevel = 2
			}
			for next := level + 1; next <= maxLevel; next++ {
				if _, matched := ChannelCompatibilityBody(body, next); matched {
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
		act := o.PenalizeAccount(acc, model, ue)
		if act.FailFast || !act.Rotate {
			return acc, err
		}
		if sessionKey != "" {
			o.Sessions.UnbindMatching(sessionKey, acc.UID)
		}
		if attempt+1 >= maxFailoverAttempts {
			return acc, err
		}
		alt, aerr := o.PickAccountExcludingIn(model, sessionKey, tried, scope)
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

func (o *Runtime) AccountSelector(model, preferred string, tried map[string]bool, scope map[string]bool, routing ...*SessionSelection) (func(map[string]bool) (*pool.Account, *apiError), *apiError) {
	allowed, aerr := o.ModelAccountUIDs(model)
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
	cost := o.ModelCostByUID(model, allowed)
	window := o.ExpiryWindowDays()
	var migrationFrom, migrationTarget string
	return func(busy map[string]bool) (*pool.Account, *apiError) {
		ready := map[string]bool{}
		now := nowSec()
		for _, candidate := range o.Pool.Accounts() {
			// Keep the original selected account's short account-cooldown grace;
			// model cooldown remains a strict exclusion as in the original picker.
			preferredGrace := candidate.UID == preferred && candidate.Callable() && candidate.CooldownUntil-now <= SessionStickyMaxCooldown
			if !allowed[candidate.UID] || (!candidate.Healthy(now) && !preferredGrace) || o.ModelCooldownUntil(candidate.UID, model) > now {
				continue
			}
			ready[candidate.UID] = true
		}
		if len(ready) == 0 {
			return nil, ErrPoolExhausted(model, tried)
		}
		// Preserve original routing before considering occupancy. Busy accounts
		// must not lose affinity or cost/expiry preference merely to fill slots.

		routePreferred := preferred
		if len(routing) > 0 && routing[0].Key != "" {
			selection := routing[0]
			uid, action, target, baseline, version := o.Sessions.Route(selection.Key)
			selection.Version = version
			selection.Action, selection.Target, selection.Baseline = action, target, baseline
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
					o.Sessions.Reject(selection.Key, version, "没有其他同成本或更低成本的健康授权账号，保留原选择")
					return nil, nil
				}
				ready = alternatives
				routePreferred = preferred
			case "switch":
				baseCost, baseKnown := cost[baseline]
				targetCost, targetKnown := cost[target]
				candidate := o.Pool.Get(target)
				if !ready[target] || candidate == nil || !candidate.Healthy(now) || !baseKnown || !targetKnown || baseCost < 0 || targetCost < 0 || math.IsNaN(baseCost) || math.IsNaN(targetCost) || math.IsInf(baseCost, 0) || math.IsInf(targetCost, 0) || targetCost > baseCost+1e-9 {
					o.Sessions.Reject(selection.Key, version)
					return nil, nil // Retry selection against the new version on the queue wakeup.
				}
				routePreferred = target
			}
		}
		var selected *pool.Account
		if ready[routePreferred] {

			selected = o.Pool.Get(routePreferred)
			if len(routing) == 0 || routing[0].Action == "" {
				key := func() string {
					if len(routing) > 0 {
						return routing[0].Key
					}
					return ""
				}()
				if migrationFrom == routePreferred && ready[migrationTarget] && !o.Sessions.ManuallyPinned(key, routePreferred) {
					selected = o.Pool.Get(migrationTarget)
				} else {
					selected = o.PreferDomesticSession(key, selected, ready, cost, window)
					if selected != nil && selected.UID != routePreferred {
						migrationFrom, migrationTarget = routePreferred, selected.UID
					} else {
						migrationFrom, migrationTarget = "", ""
					}
				}
			}
		} else {
			selected = o.Pool.Pick(ready, cost, window)
			if selected != nil {
				preferred = selected.UID
			}
		}
		if selected != nil && busy[selected.UID] {
			return nil, nil
		}
		if selected != nil && len(routing) > 0 && routing[0].Action == "reselect" {
			routing[0].Target = selected.UID
		}
		return selected, nil
	}, nil
}

// Migrate automatic paid international bindings only when the cheapest healthy
// authorized group has a domestic option. Explicit administrator pins win.

func (o *Runtime) PreferDomesticSession(key string, current *pool.Account, ready map[string]bool, costs map[string]float64, window float64) *pool.Account {
	if current == nil || siterouting.ProfileSite(current.Profile) != siterouting.International || o.Sessions.ManuallyPinned(key, current.UID) {
		return current
	}
	base, known := costs[current.UID]
	if !known || base <= 0 || math.IsNaN(base) || math.IsInf(base, 0) {
		return current
	}
	minimum := math.Inf(1)
	for _, candidate := range o.Pool.Accounts() {
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
	for _, candidate := range o.Pool.Accounts() {
		cost, ok := costs[candidate.UID]
		if ready[candidate.UID] && candidate.Healthy(nowSec()) && siterouting.ProfileSite(candidate.Profile) == siterouting.Domestic && ok && math.Abs(cost-minimum) <= 1e-9 {
			if selected := o.Pool.Pick(ready, costs, window); selected != nil {
				return selected
			}
		}
	}
	return current
}
func ErrPoolExhausted(model string, tried map[string]bool) *apiError {
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
