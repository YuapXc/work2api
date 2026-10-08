package app

import (
	"context"
	"math"
	"sort"
	"strings"
)

// Observations and controls share the affinity map's bounded, in-memory lifetime.
// No conversation text, raw session identifier or credential is retained.
type sessionState struct {
	ID           string  `json:"id"`
	Model        string  `json:"model"`
	App          string  `json:"app"`
	UserID       int64   `json:"user_id"`
	Source       string  `json:"source"`
	Version      uint64  `json:"version"`
	Started      float64 `json:"started_at"`
	Last         float64 `json:"last_at"`
	Requests     int     `json:"requests"`
	Running      int     `json:"running"`
	Waiting      int     `json:"waiting"`
	Credits      float64 `json:"credits"`
	Known        int     `json:"credits_known"`
	Unknown      int     `json:"credits_unknown"`
	Pending      string  `json:"pending_action"`
	Target       string  `json:"target_uid"`
	RouteStatus  string  `json:"route_status"`
	RouteMessage string  `json:"route_message"`
	LastUID      string  `json:"-"`
	scope        map[string]bool
}
type sessionView struct {
	sessionState
	UID     string  `json:"account_uid"`
	Expires float64 `json:"expires_at"`
}
type sessionCallKey struct{}
type sessionCall struct {
	router  *sessionRouter
	key     string
	state   *sessionState
	running bool
}
type sessionSelection struct {
	key                      string
	version                  uint64
	action, target, baseline string
}

func (r *sessionRouter) begin(ctx context.Context, key, model string, p *Principal, body map[string]any) (context.Context, func()) {
	if key == "" {
		return ctx, func() {}
	}
	r.mu.Lock()
	if call, _ := ctx.Value(sessionCallKey{}).(*sessionCall); call != nil && call.router == r && call.key == key {
		if e, ok := r.m[key]; ok && e.info == call.state && p != nil {
			e.info.scope = copySessionScope(p.AccountScope)
		}
		r.mu.Unlock()
		return ctx, func() {}
	}
	now := nowSec()
	entry, ok := r.m[key]
	if ok && entry.expires <= now && (entry.info == nil || entry.info.Running+entry.info.Waiting == 0) {
		r.removeLocked(key)
		ok = false
	}
	if !ok {
		for len(r.m) >= r.max && r.order.Len() > 0 {
			if !r.evictIdleLocked() {
				r.mu.Unlock()
				return ctx, func() {}
			}
		}
		entry = sessionEntry{order: r.order.PushBack(key)}
	}
	if entry.info == nil {
		r.sequence++
		source := strings.SplitN(extractSessionKey(body), ":", 2)[0]
		entry.info = &sessionState{ID: key, Model: model, Source: source, Version: r.sequence, Started: now, LastUID: entry.uid}
	}
	info := entry.info
	if p != nil {
		info.App = clipContent(p.AppName, 120)
		info.UserID = p.UserID
		info.scope = copySessionScope(p.AccountScope)
	}
	info.Requests++
	info.Waiting++
	info.Last = now
	entry.expires = now + r.ttl
	r.m[key] = entry
	r.order.MoveToBack(entry.order)
	call := &sessionCall{router: r, key: key, state: info}
	r.mu.Unlock()
	return context.WithValue(ctx, sessionCallKey{}, call), func() { call.finish() }
}
func copySessionScope(scope map[string]bool) map[string]bool {
	if scope == nil {
		return nil
	}
	out := map[string]bool{}
	for k, v := range scope {
		out[k] = v
	}
	return out
}
func (c *sessionCall) phase(running bool) {
	c.router.mu.Lock()
	defer c.router.mu.Unlock()
	e, ok := c.router.m[c.key]
	if !ok || e.info != c.state || c.running == running {
		return
	}
	if running {
		c.state.Waiting--
		c.state.Running++
	} else {
		c.state.Running--
		c.state.Waiting++
	}
	c.running = running
	c.state.Last = nowSec()
}
func (c *sessionCall) finish() {
	c.router.mu.Lock()
	defer c.router.mu.Unlock()
	e, ok := c.router.m[c.key]
	if !ok || e.info != c.state {
		return
	}
	if c.running {
		c.state.Running--
	} else {
		c.state.Waiting--
	}
	c.state.Last = nowSec()
	e.expires = c.state.Last + c.router.ttl
	c.router.m[c.key] = e
}
func sessionPhase(ctx context.Context, running bool) {
	if c, _ := ctx.Value(sessionCallKey{}).(*sessionCall); c != nil {
		c.phase(running)
	}
}
func sessionCredits(ctx context.Context, credits *float64) {
	if ctx == nil {
		return
	}
	c, _ := ctx.Value(sessionCallKey{}).(*sessionCall)
	if c == nil {
		return
	}
	c.router.mu.Lock()
	defer c.router.mu.Unlock()
	if e, ok := c.router.m[c.key]; !ok || e.info != c.state {
		return
	}
	if credits != nil && *credits >= 0 && !math.IsNaN(*credits) && !math.IsInf(*credits, 0) {
		c.state.Credits += *credits
		c.state.Known++
	} else {
		c.state.Unknown++
	}
}
func (r *sessionRouter) views() []sessionView {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := nowSec()
	out := []sessionView{}
	for key, e := range r.m {
		if e.expires <= now && (e.info == nil || e.info.Running+e.info.Waiting == 0) {
			r.removeLocked(key)
			continue
		}
		if e.info != nil {
			state := *e.info
			state.scope = copySessionScope(state.scope)
			out = append(out, sessionView{state, e.uid, e.expires})
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
func (r *sessionRouter) view(key string) (sessionView, bool) {
	for _, v := range r.views() {
		if v.ID == key {
			return v, true
		}
	}
	return sessionView{}, false
}
func (r *sessionRouter) route(key string) (uid, action, target, baseline string, version uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.m[key]
	if e.info == nil {
		return e.uid, "", "", e.uid, 0
	}
	return e.uid, e.info.Pending, e.info.Target, e.info.LastUID, e.info.Version
}
func (r *sessionRouter) reject(key string, version uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e := r.m[key]
	if e.info == nil || e.info.Version != version {
		return
	}
	e.info.Pending = ""
	e.info.Target = ""
	e.info.RouteStatus = "failed"
	e.info.RouteMessage = "目标账号权限、成本或可用状态已变化，已恢复自动选择"
	r.sequence++
	e.info.Version = r.sequence
}
func (r *sessionRouter) commit(selection *sessionSelection, uid string) bool {
	if selection == nil || selection.key == "" {
		return true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[selection.key]
	if !ok || e.info == nil {
		return true
	}
	if e.info.Version != selection.version {
		return false
	}
	changed := e.uid != uid || e.info.Pending != ""
	if e.info.Pending != "" {
		e.info.RouteStatus = "applied"
		e.info.RouteMessage = "账号调整已生效"
		e.info.Pending = ""
		e.info.Target = ""
	}
	if changed {
		r.sequence++
		e.info.Version = r.sequence
	}
	e.uid = uid
	e.info.LastUID = uid
	e.expires = nowSec() + r.ttl
	r.m[selection.key] = e
	return true
}
func (r *sessionRouter) control(key string, version uint64, action, target string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[key]
	if !ok || e.info == nil || e.info.Version != version {
		return false
	}
	r.sequence++
	e.info.Version = r.sequence
	if action == "cancel" {
		e.info.Pending = ""
		e.info.Target = ""
		e.info.RouteStatus = "cancelled"
		e.info.RouteMessage = "待生效调整已取消"
	} else {
		e.info.Pending = action
		e.info.Target = target
		e.info.RouteStatus = "pending"
		e.info.RouteMessage = "等待下一次尚未开始执行的请求"
	}
	return true
}
