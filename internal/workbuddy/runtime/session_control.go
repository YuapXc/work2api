package runtime

import (
	"context"
	"math"
	"sort"
	"strings"
)

// Observations and controls share the affinity map's bounded, in-memory lifetime.
// No conversation text, raw session identifier or credential is retained.
type SessionState struct {
	ID                           string          `json:"id"`
	Model                        string          `json:"model"`
	App                          string          `json:"app"`
	UserID                       int64           `json:"user_id"`
	Source                       string          `json:"source"`
	Version                      uint64          `json:"Version"`
	Started                      float64         `json:"started_at"`
	Last                         float64         `json:"last_at"`
	Requests                     int             `json:"requests"`
	Running                      int             `json:"Running"`
	Waiting                      int             `json:"waiting"`
	Credits                      float64         `json:"credits"`
	Known                        int             `json:"credits_known"`
	Unknown                      int             `json:"credits_unknown"`
	Pending                      string          `json:"pending_action"`
	Target                       string          `json:"target_uid"`
	RouteStatus                  string          `json:"route_status"`
	RouteMessage                 string          `json:"route_message"`
	LastSuccess                  string          `json:"last_success_uid"`
	LastAttempt                  string          `json:"last_attempt_uid"`
	ManualUID                    string          `json:"-"`
	ControlEpoch                 uint64          `json:"-"`
	AgentRequests                int             `json:"agent_requests"`
	LastUID                      string          `json:"-"`
	Compatibility                string          `json:"Compatibility"`
	CompatUID, CompatFingerprint string          `json:"-"`
	CompatLevel                  int             `json:"-"`
	Scope                        map[string]bool `json:"-"`
}
type SessionView struct {
	SessionState
	UID     string  `json:"account_uid"`
	Expires float64 `json:"expires_at"`
}
type SessionCallKey struct{}
type SessionCall struct {
	Router         *SessionRouter
	Key            string
	State          *SessionState
	Running        bool
	ControlVersion uint64
	TargetUID      string
}
type SessionSelection struct {
	Key                      string
	Version                  uint64
	Action, Target, Baseline string
	Call                     *SessionCall
}

func (r *SessionRouter) Begin(ctx context.Context, key, model string, p SessionPrincipal, body map[string]any) (context.Context, func()) {
	if key == "" {
		return ctx, func() {}
	}
	r.Mu.Lock()
	if call, _ := ctx.Value(SessionCallKey{}).(*SessionCall); call != nil && call.Router == r && call.Key == key {
		if e, ok := r.M[key]; ok && e.Info == call.State && p != nil {
			e.Info.Scope = CopySessionScope(p.SessionCaller().AccountScope)
		}
		r.Mu.Unlock()
		return ctx, func() {}
	}
	now := nowSec()
	entry, ok := r.M[key]
	if ok && entry.Expires <= now && (entry.Info == nil || entry.Info.Running+entry.Info.Waiting == 0) {
		r.RemoveLocked(key)
		ok = false
	}
	if !ok {
		for len(r.M) >= r.Max && r.Order.Len() > 0 {
			if !r.EvictIdleLocked() {
				r.Mu.Unlock()
				return ctx, func() {}
			}
		}
		entry = SessionEntry{Order: r.Order.PushBack(key)}
	}
	if entry.Info == nil {
		r.Sequence++
		source := strings.SplitN(ExtractSessionKey(body), ":", 2)[0]
		if identity, ok := ctx.Value(RequestSessionIdentityKey{}).(RequestSessionIdentity); ok {
			source = strings.SplitN(identity.Key, ":", 2)[0]
		}
		entry.Info = &SessionState{ID: key, Model: model, Source: source, Version: r.Sequence, Started: now, LastUID: entry.Uid}
	}
	info := entry.Info
	if p != nil {
		info.App = ClipContent(p.SessionCaller().AppName, 120)
		info.UserID = p.SessionCaller().UserID
		info.Scope = CopySessionScope(p.SessionCaller().AccountScope)
	}
	info.Requests++
	if identity, ok := ctx.Value(RequestSessionIdentityKey{}).(RequestSessionIdentity); ok && identity.Agent != "" {
		info.AgentRequests++
	}
	info.Waiting++
	info.Last = now
	entry.Expires = now + r.Ttl
	r.M[key] = entry
	r.Order.MoveToBack(entry.Order)
	call := &SessionCall{Router: r, Key: key, State: info}
	r.Mu.Unlock()
	return context.WithValue(ctx, SessionCallKey{}, call), func() { call.Finish() }
}
func CopySessionScope(scope map[string]bool) map[string]bool {
	if scope == nil {
		return nil
	}
	out := map[string]bool{}
	for k, v := range scope {
		out[k] = v
	}
	return out
}
func (c *SessionCall) Phase(running bool) {
	c.Router.Mu.Lock()
	defer c.Router.Mu.Unlock()
	e, ok := c.Router.M[c.Key]
	if !ok || e.Info != c.State || c.Running == running {
		return
	}
	if running {
		c.State.Waiting--
		c.State.Running++
	} else {
		c.State.Running--
		c.State.Waiting++
	}
	c.Running = running
	c.State.Last = nowSec()
}
func (c *SessionCall) Finish() {
	c.Router.Mu.Lock()
	defer c.Router.Mu.Unlock()
	e, ok := c.Router.M[c.Key]
	if !ok || e.Info != c.State {
		return
	}
	if c.Running {
		c.State.Running--
	} else {
		c.State.Waiting--
	}
	c.State.Last = nowSec()
	e.Expires = c.State.Last + c.Router.Ttl
	c.Router.M[c.Key] = e
}
func SessionPhase(ctx context.Context, running bool) {
	if c, _ := ctx.Value(SessionCallKey{}).(*SessionCall); c != nil {
		c.Phase(running)
	}
}
func SessionCredits(ctx context.Context, credits *float64) {
	if ctx == nil {
		return
	}
	c, _ := ctx.Value(SessionCallKey{}).(*SessionCall)
	if c == nil {
		return
	}
	c.Router.Mu.Lock()
	defer c.Router.Mu.Unlock()
	if e, ok := c.Router.M[c.Key]; !ok || e.Info != c.State {
		return
	}
	if credits != nil && *credits >= 0 && !math.IsNaN(*credits) && !math.IsInf(*credits, 0) {
		c.State.Credits += *credits
		c.State.Known++
	} else {
		c.State.Unknown++
	}
}
func (r *SessionRouter) Views() []SessionView {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	now := nowSec()
	out := []SessionView{}
	for key, e := range r.M {
		if e.Expires <= now && (e.Info == nil || e.Info.Running+e.Info.Waiting == 0) {
			r.RemoveLocked(key)
			continue
		}
		if e.Info != nil {
			state := *e.Info
			state.Scope = CopySessionScope(state.Scope)
			out = append(out, SessionView{state, e.Uid, e.Expires})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Last == out[j].Last {
			return out[i].ID < out[j].ID
		}
		return out[i].Last > out[j].Last
	})
	return out
}
func (r *SessionRouter) View(key string) (SessionView, bool) {
	for _, v := range r.Views() {
		if v.ID == key {
			return v, true
		}
	}
	return SessionView{}, false
}
func (r *SessionRouter) Route(key string) (uid, action, target, baseline string, version uint64) {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	e := r.M[key]
	if e.Info == nil {
		return e.Uid, "", "", e.Uid, 0
	}
	return e.Uid, e.Info.Pending, e.Info.Target, e.Info.LastUID, e.Info.Version
}
func (r *SessionRouter) Reject(key string, version uint64, reason ...string) {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	e := r.M[key]
	if e.Info == nil || e.Info.Version != version {
		return
	}
	e.Info.Pending = ""
	e.Info.Target = ""
	e.Info.RouteStatus = "failed"
	e.Info.RouteMessage = "目标账号权限、成本或可用状态已变化，已恢复自动选择"
	if len(reason) > 0 {
		e.Info.RouteMessage = reason[0]
	}
	r.Sequence++
	e.Info.Version = r.Sequence
	e.Info.ControlEpoch = r.Sequence
}
func (r *SessionRouter) Commit(selection *SessionSelection, uid string) bool {
	if selection == nil || selection.Key == "" {
		return true
	}
	r.Mu.Lock()
	defer r.Mu.Unlock()
	e, ok := r.M[selection.Key]
	if !ok || e.Info == nil {
		return true
	}
	if e.Info.Version != selection.Version {
		return false
	}
	if e.Uid != uid {
		ClearCompatibility(e.Info)
	}
	changed := e.Uid != uid || e.Info.Pending != ""
	if e.Info.Pending != "" {
		e.Info.RouteStatus = "selected"
		e.Info.RouteMessage = "目标账号已选定，等待开始调用"
		if e.Info.Pending == "switch" {
			e.Info.ManualUID = uid
		} else {
			e.Info.ManualUID = ""
		}
		e.Info.Pending = ""
		e.Info.Target = ""
	}
	if changed {
		r.Sequence++
		e.Info.Version = r.Sequence
	}
	if selection.Call != nil && selection.Action != "" {
		selection.Call.ControlVersion = e.Info.ControlEpoch
		selection.Call.TargetUID = uid
	}
	e.Uid = uid
	e.Info.LastUID = uid
	e.Expires = nowSec() + r.Ttl
	r.M[selection.Key] = e
	return true
}

// Only the request that consumed this Control can report its Outcome. Later
// controls and older concurrent calls cannot overwrite that operation.
func (r *SessionRouter) Attempt(ctx context.Context, uid string) {
	c, _ := ctx.Value(SessionCallKey{}).(*SessionCall)
	if c == nil {
		return
	}
	r.Mu.Lock()
	defer r.Mu.Unlock()
	e := r.M[c.Key]
	if e.Info != c.State {
		return
	}
	e.Info.LastAttempt = uid
	if c.ControlVersion != 0 && c.ControlVersion == e.Info.ControlEpoch {
		e.Info.RouteStatus = "executing"
		e.Info.RouteMessage = "指定调整的账号开始调用"
		if uid != c.TargetUID {
			e.Info.RouteMessage = "目标账号失败，正在尝试其他授权账号"
		}
	}
}

func (r *SessionRouter) Outcome(ctx context.Context, uid string, err error) {
	c, _ := ctx.Value(SessionCallKey{}).(*SessionCall)
	if c == nil {
		return
	}
	r.Mu.Lock()
	defer r.Mu.Unlock()
	e := r.M[c.Key]
	if e.Info != c.State {
		return
	}
	if err == nil && e.Uid == uid {
		e.Info.LastSuccess = uid
	}
	if c.ControlVersion == 0 || c.ControlVersion != e.Info.ControlEpoch {
		return
	}
	if err == nil {
		e.Info.RouteStatus = "applied"
		e.Info.RouteMessage = "调整账号已成功完成调用"
		if uid != c.TargetUID {
			e.Info.RouteStatus = "fallback"
			e.Info.RouteMessage = "目标账号失败，已由其他授权账号完成调用"
		}
	} else {
		e.Info.RouteStatus = "failed"
		e.Info.RouteMessage = "调整账号调用未成功；后续按授权和故障规则处理"
	}
}

func (r *SessionRouter) ManuallyPinned(key, uid string) bool {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	e := r.M[key]
	return uid != "" && e.Info != nil && e.Info.ManualUID == uid
}
func (r *SessionRouter) Control(key string, version uint64, action, target string) bool {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	e, ok := r.M[key]
	if !ok || e.Info == nil || e.Info.Version != version {
		return false
	}
	r.Sequence++
	e.Info.Version = r.Sequence
	e.Info.ControlEpoch = r.Sequence
	if action == "cancel" {
		e.Info.Pending = ""
		e.Info.Target = ""
		e.Info.RouteStatus = "cancelled"
		e.Info.RouteMessage = "待生效调整已取消"
	} else {
		e.Info.Pending = action
		e.Info.Target = target
		e.Info.RouteStatus = "pending"
		e.Info.RouteMessage = "等待下一次尚未开始执行的请求"
	}
	return true
}
