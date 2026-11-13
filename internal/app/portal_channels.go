package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"work2api/internal/core/provider"
	"work2api/internal/opencode"
	"work2api/internal/qoder"
	qa "work2api/internal/qoder/account"
)

func (s *Server) reserveChannelContribution(userID int64, uid, name, site string, exists bool) (int64, error) {
	s.o.wb.AccountMu.Lock()
	defer s.o.wb.AccountMu.Unlock()
	user, err := s.o.db.GetUser(userID)
	if err != nil || user == nil || user.Status != "active" || user.MustChangePassword || !s.o.cfg.PortalEnabled {
		return 0, fmt.Errorf("用户当前不可贡献账号")
	}
	owner, err := s.o.db.ContributionByAccount(uid)
	if err != nil {
		return 0, fmt.Errorf("归属查询失败")
	}
	if owner == nil {
		if exists {
			return 0, fmt.Errorf("账号已存在系统中，请联系管理员处理归属")
		}
		return s.o.db.CreateContribution(userID, uid, name, site, "verifying")
	}
	if owner.UserID != userID || owner.Provider != name {
		return 0, fmt.Errorf("账号已归属其他用户")
	}
	if owner.Status == "active" {
		return 0, fmt.Errorf("账号已贡献，无需重复添加")
	}
	changed, err := s.o.db.SetContributionStatus(owner.ID, userID, owner.Status, "verifying", "")
	if err != nil || !changed {
		return 0, fmt.Errorf("账号状态已变更，请重试")
	}
	return owner.ID, nil
}

func (s *Server) portalChannelImport(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name != "qoder" && name != "opencode" {
		writeAPIErr(w, errBody(400, "渠道不支持此贡献方式", "invalid_request_error"))
		return
	}
	rt, ok := s.o.runtimes.ByName(name)
	if !ok {
		writeAPIErr(w, errBody(503, "渠道不可用", "provider_unavailable"))
		return
	}
	importer, ok := rt.(provider.AccountImporter)
	if !ok {
		writeAPIErr(w, errBody(400, "不支持凭据导入", "invalid_request_error"))
		return
	}
	body, err := readJSON(r)
	if err != nil || body["accepted"] != true {
		writeAPIErr(w, errBody(400, "请确认贡献账号及额度使用范围", "invalid_request_error"))
		return
	}
	token, _ := body["token"].(string)
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 16384 {
		writeAPIErr(w, errBody(400, "请输入有效个人凭据", "invalid_request_error"))
		return
	}
	if name == "qoder" && str2(body["region"]) != "cn" && str2(body["region"]) != "global" {
		writeAPIErr(w, errBody(400, "请选择国内或国际区域", "invalid_request_error"))
		return
	}
	// Only these options may cross into the runtime; never accept a callback,
	// upstream URL, path, ownership or config patch from the browser.
	options := map[string]any{"portal": true, "region": body["region"], "tier": body["tier"]}
	owner := portalCtx(r).ID
	var contribution int64
	options["claim"] = func(id string) error {
		uid := id
		exists := false
		site := str2(body["tier"])
		if name == "qoder" {
			uid = qoder.PortalAccountUID(id)
			a, _ := qa.Get(id)
			exists = a != nil
			site = str2(body["region"])
		} else if channel, ok := rt.(*opencode.Runtime); ok {
			exists = channel.HasPortalKey(uid)
		}
		var e error
		contribution, e = s.reserveChannelContribution(owner, uid, name, site, exists)
		return e
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	account, err := importer.AddAccountByToken(ctx, token, options)
	if err != nil {
		writeAPIErr(w, errBody(400, err.Error(), "invalid_request_error"))
		return
	}
	if contribution == 0 {
		writeAPIErr(w, errBody(503, "贡献登记未完成", "server_error"))
		return
	}
	if err := s.o.db.ActivateContribution(contribution, owner); err != nil {
		writeAPIErr(w, errBody(503, "贡献登记失败，可重新添加", "server_error"))
		return
	}
	result := map[string]any{"ok": true, "contribution_id": contribution}
	s.verifyAddedAccount(ctx, name, account, result)
	writeJSON(w, 200, result)
}

func (s *Server) portalQoderOAuth(w http.ResponseWriter, r *http.Request) {
	rt, ok := s.o.runtimes.ByName("qoder")
	if !ok {
		writeAPIErr(w, errBody(503, "Qoder 不可用", "provider_unavailable"))
		return
	}
	channel, ok := rt.(*qoder.Runtime)
	if !ok {
		writeAPIErr(w, errBody(503, "Qoder 不可用", "provider_unavailable"))
		return
	}
	body, err := readJSON(r)
	if err != nil {
		writeAPIErr(w, errBody(400, "无效请求", "invalid_request_error"))
		return
	}
	owner := portalCtx(r).ID
	id := str2(body["task_id"])
	if r.PathValue("action") == "begin" {
		if !s.o.cfg.PortalEnabled || body["accepted"] != true {
			writeAPIErr(w, errBody(403, "请确认贡献范围且门户必须启用", "forbidden"))
			return
		}
		region := str2(body["region"])
		if region != "cn" && region != "global" {
			writeAPIErr(w, errBody(400, "请选择国内或国际区域", "invalid_request_error"))
			return
		}
		reservation := s.o.genAPIKey()
		now := time.Now()
		task := &portalContributionTask{userID: owner, site: "qoder", created: now, expires: now.Add(portalTaskTTL)}
		s.portalMu.Lock()
		count := 0
		for key, t := range s.portalTasks {
			if now.After(t.expires) {
				delete(s.portalTasks, key)
			} else if t.userID == owner {
				count++
			}
		}
		if count >= s.o.cfg.PortalMaxTasksPerUser || len(s.portalTasks) >= portalMaxTasks {
			s.portalMu.Unlock()
			writeAPIErr(w, localOverload("portal_tasks"))
			return
		}
		s.portalTasks[reservation] = task
		s.portalMu.Unlock()
		claim := func(localID string) error {
			if !s.portalTaskLive(task) {
				return fmt.Errorf("登录任务已取消或过期")
			}
			a, _ := qa.Get(localID)
			_, e := s.reserveChannelContribution(owner, qoder.PortalAccountUID(localID), "qoder", region, a != nil)
			return e
		}
		res, e := channel.OAuthBegin(map[string]any{"region": region, "portal": true, "claim": claim})
		if e != nil {
			s.removePortalTask(reservation)
			writeAPIErr(w, errBody(502, "发起登录失败", "upstream_error"))
			return
		}
		id = str2(res["login_id"])
		s.portalMu.Lock()
		delete(s.portalTasks, reservation)
		s.portalTasks[id] = task
		s.portalMu.Unlock()
		writeJSON(w, 200, map[string]any{"task_id": id, "auth_url": res["login_url"], "expires_at": task.expires.Unix()})
		return
	}
	s.portalMu.Lock()
	task := s.portalTasks[id]
	valid := task != nil && task.userID == owner && task.site == "qoder" && !time.Now().After(task.expires)
	s.portalMu.Unlock()
	if !valid {
		writeAPIErr(w, errBody(404, "登录任务不存在或已过期", "not_found"))
		return
	}
	switch r.PathValue("action") {
	case "cancel":
		s.removePortalTask(id)
		qa.CancelLogin(id)
		writeJSON(w, 200, map[string]any{"ok": true})
	case "poll":
		res, e := channel.OAuthPoll(id)
		if e != nil {
			writeAPIErr(w, errBody(503, "登录状态查询失败", "server_error"))
			return
		}
		if res["status"] == "ready" {
			a, _ := res["account"].(map[string]any)
			uid := qoder.PortalAccountUID(str2(a["id"]))
			c, e := s.o.db.ContributionByAccount(uid)
			if e != nil || c == nil || c.UserID != owner || c.Provider != "qoder" {
				writeAPIErr(w, errBody(503, "账号归属验证失败", "server_error"))
				return
			}
			s.portalMu.Lock()
			valid = s.portalTasks[id] == task && !time.Now().After(task.expires)

			if !valid {
				s.portalMu.Unlock()
				writeAPIErr(w, errBody(409, "任务已取消", "conflict"))
				return
			}
			if c.Status == "verifying" {
				if e := s.o.db.ActivateContribution(c.ID, owner); e != nil {
					s.portalMu.Unlock()
					writeAPIErr(w, errBody(503, "贡献激活失败", "server_error"))
					return
				}
			}
			delete(s.portalTasks, id)
			s.portalMu.Unlock()
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			_ = channel.RefreshOwnedAccount(ctx, str2(a["id"]))
			cancel()
			s.removePortalTask(id)
			writeJSON(w, 200, map[string]any{"status": "ready", "account_uid_masked": maskUID(uid), "models_verified": s.contributionCatalogStatus(uid) == "ready"})
			return
		}
		status := res["status"]
		if status == "error" {
			status = "failed"
			s.removePortalTask(id)
		}
		writeJSON(w, 200, map[string]any{"status": status, "message": res["message"]})
	default:
		writeAPIErr(w, errBody(404, "未知操作", "not_found"))
	}
}

// Reservation and upstream login IDs share task identity. Invalid tasks must
// use the same balanced locking path as successful authorization.
func (s *Server) portalTaskLive(task *portalContributionTask) bool {
	s.portalMu.Lock()
	defer s.portalMu.Unlock()
	for _, current := range s.portalTasks {
		if current == task && !time.Now().After(current.expires) {
			return true
		}
	}
	return false
}
