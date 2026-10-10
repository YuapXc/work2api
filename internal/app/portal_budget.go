package app

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sync"

	"work2api/internal/store"
)

type portalDispatchCheckKey struct{}

// Re-read authorization after global admission, and after every account wait.
// A queued request must never retain eligibility which has since been revoked.
func (s *Server) refreshPortalRequest(w http.ResponseWriter, r *http.Request, p *Principal, payload map[string]any) (*http.Request, bool) {
	if p.AppID == 0 {
		return r, true
	}
	scope := p.AccountScope
	admittedModel := p.EffectiveModel
	refresh := func(uid, actualModel string) *apiError {
		fresh, aerr := s.o.checkAPIKey(r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"))
		if aerr != nil {
			return aerr
		}
		if fresh.AppID != p.AppID || fresh.UserID != p.UserID {
			return errBody(403, "密钥归属已改变", "auth_error")
		}
		if aerr := s.prepareModel(fresh, payload); aerr != nil {
			return aerr
		}
		if admittedModel != "" && fresh.EffectiveModel != admittedModel {
			return errBody(403, "模型映射已改变，请重新发起请求", "model_not_allowed")
		}
		if actualModel != "" && s.o.resolveModel(strOr(payload["model"], "")) != actualModel {
			return errBody(403, "模型映射已改变，请重新发起请求", "model_not_allowed")
		}
		if fresh.UserID > 0 && uid != "" && !fresh.AccountScope[uid] {
			return errBody(403, "共享账户资格已撤销，请重新发起请求", "portal_no_eligibility")
		}
		if fresh.UserID == 0 && uid != "" {
			privateOnly, err := s.o.db.PrivatePoolExcludedUIDs()
			if err != nil {
				return errBody(503, "账号权限查询失败", "server_error")
			}
			if privateOnly[uid] {
				return errBody(403, "该账号不再允许私人池调用，请重新发起请求", "account_not_allowed")
			}
		}
		reservation := p.quota
		if fresh.UserID > 0 {
			clear(scope)
			for uid := range fresh.AccountScope {
				scope[uid] = true
			}
			fresh.AccountScope = scope
		}
		*p = *fresh
		p.quota = reservation
		return nil
	}
	if aerr := refresh("", ""); aerr != nil {
		writeAPIErr(w, aerr)
		return r, false
	}
	return r.WithContext(context.WithValue(r.Context(), portalDispatchCheckKey{}, refresh)), true
}

type portalQuotaReservation struct {
	mu                        sync.Mutex
	db                        *store.DB
	userID                    int64
	day                       float64
	input, output             int64
	actualInput, actualOutput int64
	known                     bool
	settled                   bool
}

func (q *portalQuotaReservation) Observe(usage map[string]any) {
	if q == nil {
		return
	}
	i, okI := usage["prompt_tokens"]
	if !okI {
		i, okI = usage["input_tokens"]
	}
	o, okO := usage["completion_tokens"]
	if !okO {
		o, okO = usage["output_tokens"]
	}
	valid := func(v any) bool {
		switch n := v.(type) {
		case float64:
			return n >= 0 && n == float64(int64(n)) && n < 1e12
		case int:
			return n >= 0
		}
		return false
	}
	if !okI || !okO || !valid(i) || !valid(o) {
		return
	}
	in, out := usageTokens(usage)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.actualInput, q.actualOutput, q.known = int64(in), int64(out), true
}

// Missing upstream usage and interrupted requests retain the reservation.
func (q *portalQuotaReservation) settle() {
	if q == nil {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.settled {
		return
	}
	q.settled = true
	if !q.known {
		return
	}
	if err := q.db.SettleUserDailyQuota(q.userID, q.day, 1, q.input, q.output, 1, q.actualInput, q.actualOutput); err != nil {
		// 没有独立预占记录时，不能从共享日桶无条件扣款补偿；这会扣掉别的请求。
		log.Printf("共享额度结算失败，保留预占（user=%d day=%d）：%v", q.userID, int64(q.day), err)
	}
}

func (s *Server) reservePortalBudget(w http.ResponseWriter, p *Principal, payload map[string]any) bool {
	if p.UserID == 0 {
		return true
	}
	c := s.o.cfg
	// Daily quotas are optional. Existing body/response guards remain unchanged.
	if c.PortalDailyRequests <= 0 && c.PortalDailyInputTokens <= 0 && c.PortalDailyOutputTokens <= 0 {
		return true
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		writeAPIErr(w, errBody(400, "共享请求体无效", "invalid_request_error"))
		return false
	}
	output := s.o.wb.Catalog.MaxOutputTokens(s.o.resolveModel(strOr(payload["model"], "")))
	for _, key := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens"} {
		if v, present := payload[key]; present {
			n, ok := v.(float64)
			if !ok || n <= 0 || n > 1e9 || n != float64(int(n)) {
				writeAPIErr(w, errBody(400, "输出上限必须为正整数", "invalid_request_error"))
				return false
			}
			if output <= 0 || int(n) < output {
				output = int(n)
			}
		}
	}
	if c.PortalDailyOutputTokens > 0 && output <= 0 {
		writeAPIErr(w, errBody(503, "模型输出上限未知，无法预占已配置的输出额度", "server_error"))
		return false
	}
	if output > 0 {
		// All protocol-specific limits must forward the same reservation.
		// Otherwise ignored/competing client fields can under-reserve output.
		payload["max_tokens"] = output
		payload["max_completion_tokens"] = output
		if _, present := payload["input"]; present {
			payload["max_output_tokens"] = output
		}
	}
	// 注意：input 预占单位是请求体字节数（非 token），而结算写回上游真实
	// input_tokens——同一列两种口径。中文/长 prompt 下字节数约为 token 数的
	// 3~6 倍，并发窗口内会互相挤占并产生偏保守的 429。启用输入日预算前
	// （PortalDailyInputTokens>0）必须先统一口径（按字符估算 token 或改名
	// 为「输入字节数上限」）。当前默认 0 不启用，仅作警示。
	q := &portalQuotaReservation{db: s.o.db, userID: p.UserID, day: float64(localMidnightUnix()), input: int64(len(raw)), output: int64(output)}
	err = s.o.db.ReserveUserDailyQuota(q.userID, q.day, 1, q.input, q.output, int64(c.PortalDailyRequests), int64(c.PortalDailyInputTokens), int64(c.PortalDailyOutputTokens))
	if err != nil {
		if errors.Is(err, store.ErrDailyQuota) {
			w.Header().Set("Retry-After", "3600")
			writeAPIErr(w, errBody(429, "今日共享额度不足（包括进行中的请求预占），请稍后再试", "daily_quota_exceeded"))
		} else {
			writeAPIErr(w, errBody(503, "共享额度暂不可验证，请稍后重试", "server_error"))
		}
		return false
	}
	p.quota = q
	return true
}
