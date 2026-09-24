package app

// 上游错误分类表 + 处置策略。忠实移植自上游 Workbuddy2API 的
// workbuddy_one/gateway/errors.py（classify / action_for，吸收 Sliverkiss Classify）。
//
// 为什么需要：仅按 HTTP 状态码判罚会把「请求自身的问题」误当成「账号的问题」——
// prompt 超上下文（11115）会白轮健康号并把好号打进冷却；WAF 403 只换号不罚 → 连环
// 403；模型不存在（11102）无负缓存会反复选中坏号；凭证失效（12153）只冷却救不活。
// 分类把三件事拆开：罚不罚号(cooldown) / 换不换号(rotate) / 是否彻底禁用(disable)。
//
// **判定顺序即语义优先级，不要随手调换**（见 classifyUpstream 的注释）。

import (
	"encoding/json"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrKind 是上游错误的语义分类。
type ErrKind string

const (
	errNone           ErrKind = "none"
	errHardCredit     ErrKind = "hard_credit"     // 积分耗尽（402 / 429+14018）→ 冷却到次日 04:00
	errSoftRate       ErrKind = "soft_rate"       // 限流 → 短冷却
	errSessionDead    ErrKind = "session_dead"    // 登录态失效（401+12153）→ 禁用
	errNotFound       ErrKind = "not_found"       // 404 偶发 → 短冷却
	errServer         ErrKind = "server"          // 上游 5xx
	errContentBlocked ErrKind = "content_blocked" // 内容策略拦截 → 不罚号，透传
	errBadParams      ErrKind = "bad_params"      // 请求体畸形（11101）→ 不罚号，仍轮转
	errAccountFault   ErrKind = "account_fault"   // 账号级授权/配额故障（11140 / 14017）
	errModelBlocked   ErrKind = "model_blocked"   // 11102 该后端无此模型 → (账号,模型) 负缓存
	errWAFBlock       ErrKind = "waf_block"       // 403 无业务信封（WAF 拦截）→ 软冷却 + 抖动
	errPromptTooLong  ErrKind = "prompt_too_long" // 11115 超上下文 → fail-fast
	errImageInvalid   ErrKind = "image_invalid"   // 11135 图片无效 → fail-fast
	errClient         ErrKind = "client"          // 其他 4xx → 只换号不罚
)

// errAction 是分类到处置的映射结果（三个正交维度 + 元信息）。
type errAction struct {
	Rotate      bool    // 换不换号
	Cooldown    float64 // 罚多久（秒），0 表示不罚
	Disable     bool    // 是否彻底禁用账号（终态，需人工重新登录）
	FailFast    bool    // 请求自身的问题：不罚号、不轮转、透传
	ModelScoped bool    // 冷却仅针对 (账号,模型) 组合，而非整个账号
	Reason      string
}

const (
	softCooldownSec   = 60.0
	notFoundCooldown  = 60.0
	wafCooldownBase   = 60.0
	serverCooldown    = 120.0
	softRateMax       = 7200.0
	modelBlockBaseTTL = 6 * 3600.0
	retryAfterSanity  = 7200.0
)

var (
	rateMarkers           = []string{"rate limit", "rate-limiting", "rate-limited", "frequency limit", "too many requests", "usage limit", "请求过于频繁", "使用频率", "限流"}
	sessionDeadMarkers    = []string{"Offline user session not found", "12153"}
	accountFaultMarkers   = []string{"request illegal", "trial not activated", "trial version is not yet activated"}
	promptTooLongMarkers  = []string{`"code":11115`, `"code":"11115"`, "prompt is too long"}
	contentBlockedMarkers = []string{"blocked by security policy", "unapproved channel", "illegal api invocation"}
	badParamsMarkers      = []string{"Unmarshal chat params failed"}
	invalidImageMarkers   = []string{"invalid image_url content", "invalid_image_data", "replace the image"}
	hardCreditMarkers     = []string{"insufficient credit", "no credit", "credit exhausted", "credits exhausted", "out of credit", "quota exceeded", "quota exhaust", "payment required", "credit not enough", "not enough credit", "积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分"}
	promptTooLongStatuses = map[int]bool{400: true, 404: true, 413: true}
	resetRe               = regexp.MustCompile(`(?i)(?:将在|reset at)\s*(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2})`)
	retryAfterHeaders     = []string{"Retry-After", "Retry-After-Ms", "X-Ratelimit-Reset"}
	codeMarkerCache       = map[string]*regexp.Regexp{}
)

// 预编译用到的业务码正则（只读，避免运行期并发写 map）。
func init() {
	for _, code := range []string{"11115", "11135", "11101", "14018"} {
		codeMarkerCache[code] = regexp.MustCompile(`"code"\s*:\s*"?` + regexp.QuoteMeta(code) + `"?`)
	}
}

func hitFold(text, lower string, patterns []string) bool {
	for _, p := range patterns {
		if strings.Contains(lower, strings.ToLower(p)) || strings.Contains(text, p) {
			return true
		}
	}
	return false
}

func hitLower(lower string, patterns []string) bool {
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// codeMarker 在（已小写的）原文里找 `"code": <code>` 形态，容忍 JSON 空白与字符串形态。
func codeMarker(lower, code string) bool {
	re := codeMarkerCache[code]
	if re == nil {
		return false
	}
	return re.MatchString(lower)
}

func jsonRoot(text string) map[string]any {
	var m map[string]any
	if json.Unmarshal([]byte(text), &m) != nil {
		return nil
	}
	return m
}

// isModelBlocked 判定 11102「该后端无此模型」——只比对 code/msg 独立字段，绝不整段
// 子串匹配（错误体带 requestId 等，整段匹配会把 "11102" 撞在 ID 上）。仅 400/404。
func isModelBlocked(status int, text, lower string) bool {
	if (status != 400 && status != 404) || text == "" {
		return false
	}
	if !strings.Contains(text, "11102") && !strings.Contains(lower, "service info not found") {
		return false
	}
	root := jsonRoot(text)
	if root == nil {
		return false
	}
	nodes := []map[string]any{root}
	if inner, ok := root["error"].(map[string]any); ok {
		nodes = append(nodes, inner)
	}
	code, msg := "", ""
	for _, node := range nodes {
		for _, k := range []string{"code", "errCode", "error_code"} {
			if v := strings.TrimSpace(toStr(node[k])); v != "" && code == "" {
				code = v
			}
		}
		for _, k := range []string{"msg", "message"} {
			if v, ok := node[k].(string); ok && strings.TrimSpace(v) != "" && msg == "" {
				msg = strings.TrimSpace(v)
			}
		}
	}
	if code == "11102" {
		return true
	}
	return strings.Contains(strings.ToLower(msg), "service info not found")
}

func hasBusinessEnvelope(text string) bool {
	return strings.Contains(text, `"code":`) || strings.Contains(text, `"msg":`)
}

func isWAFBlocked(status int, text string) bool {
	return status == 403 && !hasBusinessEnvelope(text)
}

func parseRateReset(raw string) (float64, bool) {
	m := resetRe.FindStringSubmatch(raw)
	if m == nil {
		return 0, false
	}
	loc := time.FixedZone("UTC+8", 8*3600)
	t, err := time.ParseInLocation("2006-01-02 15:04:05", m[1], loc)
	if err != nil {
		return 0, false
	}
	return float64(t.Unix()), true
}

func parseRetryAfter(headers http.Header, now float64) (float64, bool) {
	if headers == nil {
		return 0, false
	}
	for _, name := range retryAfterHeaders {
		raw := strings.TrimSpace(headers.Get(name))
		if raw == "" {
			continue
		}
		n, err := strconv.ParseFloat(raw, 64)
		if err != nil { // 非纯数字（HTTP-Date 等）不解析
			continue
		}
		switch strings.ToLower(name) {
		case "retry-after-ms":
			n /= 1000.0
		case "x-ratelimit-reset":
			if n > 1e11 { // 毫秒级 epoch
				n /= 1000.0
			}
			n -= now
		}
		if n <= 0 || n > retryAfterSanity {
			continue
		}
		return n, true
	}
	return 0, false
}

// nextDay4am 返回 now 之后最近的一个 04:00（本地时区）epoch 秒。上游签到在 04:00
// 结算积分，余额耗尽的账号冷却到这里。
func nextDay4am(now float64) float64 {
	n := time.Unix(int64(now), 0)
	target := time.Date(n.Year(), n.Month(), n.Day(), 4, 0, 0, 0, n.Location())
	if n.Hour() >= 4 {
		target = target.AddDate(0, 0, 1)
	}
	return float64(target.Unix())
}

func softRateCooldown(raw string, headers http.Header, now float64) float64 {
	if reset, ok := parseRateReset(raw); ok {
		return clampFloat(reset-now, 60.0, softRateMax)
	}
	if ra, ok := parseRetryAfter(headers, now); ok {
		return clampFloat(ra, 1.0, softRateMax)
	}
	return softCooldownSec
}

func wafCooldown(headers http.Header, now float64) float64 {
	if ra, ok := parseRetryAfter(headers, now); ok {
		return clampFloat(ra, 1.0, softRateMax)
	}
	return wafCooldownBase * (0.75 + rand.Float64()*0.5) // 基 60s ±25% 抖动，避免惊群
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// classifyUpstream 把上游响应分类为 ErrKind。判定顺序即语义优先级，改前先读上游注释。
func classifyUpstream(status int, raw []byte, _ http.Header) ErrKind {
	text := string(raw)
	lower := strings.ToLower(text)
	switch {
	case isModelBlocked(status, text, lower):
		return errModelBlocked
	case status == 402:
		return errHardCredit
	case hitFold(text, lower, sessionDeadMarkers):
		return errSessionDead
	case hitFold(text, lower, accountFaultMarkers):
		return errAccountFault
	case status == 429 && codeMarker(lower, "14018"):
		return errHardCredit
	case status == 429:
		return errSoftRate
	case hitFold(text, lower, hardCreditMarkers):
		return errHardCredit
	case hitFold(text, lower, rateMarkers):
		return errSoftRate
	case promptTooLongStatuses[status] && (hitFold(text, lower, promptTooLongMarkers) || codeMarker(lower, "11115")):
		return errPromptTooLong
	case status == 404:
		return errNotFound
	case status >= 500:
		return errServer
	case isWAFBlocked(status, text):
		return errWAFBlock
	case status == 400 && (hitFold(text, lower, invalidImageMarkers) || codeMarker(lower, "11135")):
		return errImageInvalid
	case status >= 400:
		if hitLower(lower, contentBlockedMarkers) {
			return errContentBlocked
		}
		if hitFold(text, lower, badParamsMarkers) || codeMarker(lower, "11101") {
			return errBadParams
		}
		return errClient
	}
	return errNone
}

// actionFor 把 ErrKind 映射为处置策略（运营决策，与分类解耦）。
func actionFor(kind ErrKind, raw []byte, headers http.Header, now float64) errAction {
	switch kind {
	case errHardCredit:
		return errAction{Rotate: true, Cooldown: maxFloat(60.0, nextDay4am(now)-now), Reason: "余额不足"}
	case errSoftRate:
		return errAction{Rotate: true, Cooldown: softRateCooldown(string(raw), headers, now), Reason: "429 限流"}
	case errWAFBlock:
		return errAction{Rotate: true, Cooldown: wafCooldown(headers, now), Reason: "WAF 403 拦截"}
	case errSessionDead:
		return errAction{Rotate: true, Cooldown: 0, Disable: true, Reason: "登录态失效（12153），请重新扫码登录"}
	case errNotFound:
		return errAction{Rotate: true, Cooldown: notFoundCooldown, Reason: "上游 404"}
	case errAccountFault:
		if strings.Contains(strings.ToLower(string(raw)), "request illegal") {
			return errAction{Rotate: true, Cooldown: 0, Disable: true, Reason: "账号被上游封禁（11140），请重新扫码登录"}
		}
		return errAction{Rotate: true, Cooldown: softCooldownSec, Reason: "账号级故障（试用未激活）"}
	case errServer:
		return errAction{Rotate: true, Cooldown: serverCooldown, Reason: "上游 5xx"}
	case errContentBlocked, errPromptTooLong, errImageInvalid:
		return errAction{Rotate: false, Cooldown: 0, FailFast: true}
	case errModelBlocked:
		return errAction{Rotate: true, Cooldown: modelBlockBaseTTL, ModelScoped: true, Reason: "模型在该账号不可用（11102）"}
	case errBadParams:
		return errAction{Rotate: true, Cooldown: 0, Reason: "请求体畸形（不罚号）"}
	default: // errClient / errNone：只换号不罚
		return errAction{Rotate: true, Cooldown: 0}
	}
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
