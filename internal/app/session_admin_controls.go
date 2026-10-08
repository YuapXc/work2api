package app

import (
	"encoding/json"
	"io"
	"math"
	"net/http"
	"sort"

	"work2api/internal/workbuddy/siterouting"
)

type sessionAccountOption struct {
	UID          string   `json:"uid"`
	Label        string   `json:"label"`
	Site         string   `json:"site"`
	Remaining    *float64 `json:"remaining"`
	Cost         *float64 `json:"cost"`
	Running      int      `json:"running"`
	Selectable   bool     `json:"selectable"`
	Reason       string   `json:"reason"`
	CostRelation string   `json:"cost_relation"`
}

func (s *Server) adminSessions(w http.ResponseWriter, r *http.Request) {
	labels := map[string]string{}
	for _, a := range s.accountsForDisplay() {
		uid, _ := a["uid"].(string)
		labels[uid] = accountLabel(a)
	}
	rows := []map[string]any{}
	for _, v := range s.o.sessions.views() {
		rows = append(rows, map[string]any{"id": v.ID, "session": v, "account_label": labels[v.UID], "target_label": labels[v.Target]})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"sessions": rows, "memory_only": true, "capacity": s.o.sessions.max})
}
func (s *Server) sessionOptions(v sessionView) ([]sessionAccountOption, *apiError) {
	allowed, aerr := s.o.modelAccountUIDs(v.Model)
	if aerr != nil {
		return nil, aerr
	}
	if v.UserID > 0 {
		principal := &Principal{UserID: v.UserID}
		if aerr := s.o.attachPortalScope(principal); aerr != nil {
			return nil, aerr
		}
		scope := principal.ModelScopes[v.Model]
		for uid := range allowed {
			if !scope[uid] || !v.scope[uid] {
				delete(allowed, uid)
			}
		}
	} else {
		excluded, err := s.o.db.PrivatePoolExcludedUIDs()
		if err != nil {
			return nil, errBody(503, "账号权限查询失败", "server_error")
		}
		for uid := range excluded {
			delete(allowed, uid)
		}
	}
	labels := map[string]string{}
	for _, a := range s.accountsForDisplay() {
		uid, _ := a["uid"].(string)
		labels[uid] = accountLabel(a)
	}
	baseline := v.UID
	if baseline == "" {
		baseline = v.LastUID
	}
	var costs map[string]float64
	// modelCostByUID expects an explicit ready set; include baseline for comparison.
	costUIDs := copySessionScope(allowed)
	costUIDs[baseline] = true
	costs = s.o.modelCostByUID(v.Model, costUIDs)
	base, baseKnown := costs[baseline]
	counts := map[string]int{}
	if s.modelsAdmission != nil {
		counts = s.modelsAdmission.snapshot()["account_running"].(map[string]int)
	}
	out := []sessionAccountOption{}
	now := nowSec()
	for _, a := range s.o.pool.Accounts() {
		if !allowed[a.UID] {
			continue
		}
		option := sessionAccountOption{UID: a.UID, Label: labels[a.UID], Site: siterouting.ProfileSite(a.Profile), Running: counts[a.UID], Remaining: a.CreditsRemain}
		if c, known := costs[a.UID]; known && c >= 0 && !math.IsNaN(c) && !math.IsInf(c, 0) {
			value := c
			option.Cost = &value
		}
		option.CostRelation = "成本未知"
		if baseKnown && base >= 0 && !math.IsNaN(base) && !math.IsInf(base, 0) && option.Cost != nil {
			switch {
			case *option.Cost > base+1e-9:
				option.CostRelation = "更高成本"
			case *option.Cost < base-1e-9:
				option.CostRelation = "更低成本"
			default:
				option.CostRelation = "同成本"
			}
		}
		switch {
		case a.UID == v.UID:
			option.Reason = "当前账号"
		case !a.Healthy(now) || s.o.modelCooldownUntil(a.UID, v.Model) > now:
			option.Reason = "账号或模型暂不可用"
		case !baseKnown || base < 0 || math.IsNaN(base) || math.IsInf(base, 0) || option.Cost == nil:
			option.Reason = "成本未知，不能指定"
		case *option.Cost > base+1e-9:
			option.Reason = "成本高于当前账号"
		default:
			option.Selectable = true
		}
		out = append(out, option)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Selectable != out[j].Selectable {
			return out[i].Selectable
		}
		if out[i].Cost != nil && out[j].Cost != nil && *out[i].Cost != *out[j].Cost {
			return *out[i].Cost < *out[j].Cost
		}
		return out[i].UID < out[j].UID
	})
	return out, nil
}
func (s *Server) adminSessionOptions(w http.ResponseWriter, r *http.Request) {
	v, ok := s.o.sessions.view(r.PathValue("id"))
	if !ok {
		writeAPIErr(w, errBody(404, "会话已过期或不存在", "not_found"))
		return
	}
	options, aerr := s.sessionOptions(v)
	if aerr != nil {
		writeAPIErr(w, aerr)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"session": v, "accounts": options})
}
func (s *Server) adminSessionControl(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Version uint64 `json:"version"`
		Action  string `json:"action"`
		UID     string `json:"uid"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		writeAPIErr(w, errBody(400, "请求格式错误", "invalid_request_error"))
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeAPIErr(w, errBody(400, "请求格式错误", "invalid_request_error"))
		return
	}
	v, ok := s.o.sessions.view(r.PathValue("id"))
	if !ok {
		writeAPIErr(w, errBody(404, "会话已过期或不存在", "not_found"))
		return
	}
	if body.Action != "switch" && body.Action != "reselect" && body.Action != "cancel" {
		writeAPIErr(w, errBody(400, "不支持的会话操作", "invalid_request_error"))
		return
	}
	if body.Action == "switch" {
		options, aerr := s.sessionOptions(v)
		if aerr != nil {
			writeAPIErr(w, aerr)
			return
		}
		permitted := false
		for _, a := range options {
			if a.UID == body.UID && a.Selectable {
				permitted = true
			}
		}
		if !permitted {
			writeAPIErr(w, errBody(400, "目标账号不在可选范围，或成本、可用状态已变化", "session_target_unavailable"))
			return
		}
	} else {
		body.UID = ""
	}
	if !s.o.sessions.control(v.ID, body.Version, body.Action, body.UID) {
		writeAPIErr(w, errBody(409, "会话状态已变化，请刷新后重新选择", "session_version_conflict"))
		return
	}
	if s.modelsAdmission != nil {
		s.modelsAdmission.mu.Lock()
		s.modelsAdmission.signalLocked()
		s.modelsAdmission.mu.Unlock()
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
