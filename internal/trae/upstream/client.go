// Adapted from mmqz/cpa-multi-plugins, MIT; see NOTICE.md.
package upstream

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"time"

	"work2api/internal/trae/auth"
)

type ErrKind int

const (
	ErrNone          ErrKind = iota // 成功
	ErrPlanLimit                    // 1005 + plan → 权益不足（硬冷却 12h）
	ErrSoftRate                     // 429 → 短冷却 60s
	ErrSessionDead                  // 401 + Cloud-IDE-JWT 失效 → 禁用
	ErrNotFound                     // 404 → 短冷却 60s 不累计 errCount
	ErrServer                       // 5xx
	ErrClient                       // 其他 4xx
	ErrInputTooLarge                // v0.12.50: 413/过大文案 → 请求级问题，不冷却账号
	ErrModelUnavailable
)

func (k ErrKind) String() string {
	switch k {
	case ErrPlanLimit:
		return "plan_limit"
	case ErrSoftRate:
		return "soft_rate"
	case ErrSessionDead:
		return "session_dead"
	case ErrNotFound:
		return "not_found"
	case ErrServer:
		return "server"
	case ErrClient:
		return "client"
	case ErrInputTooLarge:
		return "input_too_large"
	case ErrModelUnavailable:
		return "model_unavailable"
	default:
		return "none"
	}
}

type Error struct {
	Kind    ErrKind
	Status  int
	Msg     string
	BizCode int32
}

func (e *Error) Error() string {
	if e.BizCode != 0 {
		return fmt.Sprintf("upstream biz_code=%d: %s", e.BizCode, e.Msg)
	}
	return fmt.Sprintf("upstream %s (http %d): %s", e.Kind, e.Status, e.Msg)
}
func bizError(code int32, prefix, upstreamMsg string) *Error {
	detail := strings.TrimSpace(upstreamMsg)
	if detail == "" {
		if code == 9074 {
			detail = "当前参与用户太多，请稍后再试"
		} else {
			detail = "Token 已过期，请重新登录"
		}
	}
	msg := fmt.Sprintf("%s (code=%d): %s", prefix, code, detail)
	kind := ErrSessionDead
	if code == 9074 {
		kind = ErrSoftRate
	}
	return &Error{Kind: kind, Msg: msg, BizCode: code}
}

// Require explicit input/context size language, not token-rate or queue limits.
var inputTooLargeMarkers = []string{
	"context length", "maximum context", "context window", "request entity too large",
	"input too large", "input too long", "prompt too long", "message too long",
	"messages too long", "history too long", "request payload too large", "request body too large",
	"输入过长", "内容过长", "上下文过长", "上下文长度", "超出模型上限",
}

func MsgIndicatesInputTooLarge(s string) bool {
	lower := strings.ToLower(s)
	for _, m := range inputTooLargeMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}
func Classify(status int, body string) ErrKind {
	lower := strings.ToLower(body)
	code := BusinessCode([]byte(body))
	if status == http.StatusRequestEntityTooLarge {
		return ErrInputTooLarge
	}
	// Explicit transport status wins over incidental words in an error message.
	if status == http.StatusUnauthorized {
		return ErrSessionDead
	}
	if status == http.StatusTooManyRequests {
		return ErrSoftRate
	}
	if code == 1005 || (strings.Contains(body, "1005") && strings.Contains(lower, "plan")) {
		return ErrPlanLimit
	}
	if code == 4008 || status == http.StatusPaymentRequired {
		return ErrPlanLimit
	}
	if status == http.StatusNotFound {
		return ErrNotFound
	}
	if status >= 400 && MsgIndicatesInputTooLarge(body) {
		return ErrInputTooLarge
	}
	if status >= 500 {
		return ErrServer
	}
	if status >= 400 && status < 500 && code == 4001 {
		return ErrModelUnavailable
	}
	if status >= 400 {
		return ErrClient
	}
	return ErrNone
}

func BusinessCode(body []byte) int64 {
	var doc struct {
		Code    int64 `json:"code"`
		BizCode int64 `json:"biz_code"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return 0
	}
	if doc.Code != 0 {
		return doc.Code
	}
	return doc.BizCode
}

type Client struct {
	HTTP       *http.Client
	StreamHTTP *http.Client

	AgentHost string // https://trae-api-cn.mchost.guru
	UgHost    string // https://api.trae.cn
	OAuthHost string // https://api.trae.com.cn
	ClientID  string // en1oxy7wnw8j9n
}

func New() *Client {
	tr := &http.Transport{
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   20,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 120 * time.Second, // 首字节兜底（长推理预留），不限制整流时长
	}
	return &Client{
		HTTP:       &http.Client{Timeout: 120 * time.Second, Transport: tr},
		StreamHTTP: &http.Client{Transport: tr}, // 无总超时
		AgentHost:  AgentHost,
		UgHost:     UgHost,
		OAuthHost:  OAuthHost,
		ClientID:   ClientID,
	}
}

func (c *Client) agentBase() string { return c.AgentHost }
func (c *Client) ugBase() string    { return c.UgHost }
func (c *Client) oauthBase() string { return c.OAuthHost }
func (c *Client) doJSON(req *http.Request) (json.RawMessage, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		kind := Classify(resp.StatusCode, string(raw))
		return nil, &Error{Kind: kind, Status: resp.StatusCode, Msg: truncate(string(raw), 200)}
	}
	return raw, nil
}
func (c *Client) RefreshToken(a *auth.Auth) error {
	a.Lock()
	defer a.Unlock()
	return c.refreshLocked(a)
}
func (c *Client) RefreshTokenIfNeeded(a *auth.Auth, skew time.Duration) (bool, error) {
	a.Lock()
	defer a.Unlock()
	if !a.NeedsRefreshLocked(skew) && !auth.IssuedTooLongLocked(a.AccessToken) {
		return false, nil
	}
	if err := c.refreshLocked(a); err != nil {
		return false, err
	}
	return true, nil
}
func (c *Client) refreshLocked(a *auth.Auth) error {
	if strings.TrimSpace(a.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	if err := c.boundExchangeLocked(a); err != nil {
		log.Printf("Trae device-bound refresh unavailable; trying legacy exchange for the same product")
	} else {
		return nil
	}
	lineageClient := ClientIDFor(strings.ToLower(strings.TrimSpace(a.Variant)))
	hosts := exchangeHosts(a.APIHost, c.OAuthHost)
	body := map[string]any{
		"ClientID":     lineageClient,
		"RefreshToken": a.RefreshToken, // 已持 a 写锁，直接读
		"ClientSecret": "-",
		"UserID":       "",
	}
	raw, _ := json.Marshal(body)
	var data json.RawMessage
	var lastErr error
	for _, host := range hosts {
		req, err := http.NewRequest(http.MethodPost, host+EpExchange, bytes.NewReader(raw))
		if err != nil {
			return err
		}
		OAuthHeaders(req)
		data, lastErr = c.doJSON(req)
		if lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		return lastErr
	}
	var resp struct {
		Result struct {
			Token               string `json:"Token"`
			TokenExpireAt       int64  `json:"TokenExpireAt"`
			TokenExpireDuration int64  `json:"TokenExpireDuration"`
			RefreshToken        string `json:"RefreshToken"`
			RefreshExpireAt     int64  `json:"RefreshExpireAt"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return fmt.Errorf("exchange parse: %w", err)
	}
	if resp.Result.Token == "" {
		return fmt.Errorf("refresh_failed: no token in response — re-login required")
	}
	log.Printf("exchange refresh: uid=%s variant=%q client=%s", a.UID, a.Variant, lineageClient)
	a.AccessToken = resp.Result.Token
	if resp.Result.RefreshToken != "" {
		a.RefreshToken = resp.Result.RefreshToken
	}
	if resp.Result.TokenExpireAt > 0 {
		a.ExpiresAt = normalizeExpiresAt(resp.Result.TokenExpireAt)
	} else if resp.Result.TokenExpireDuration > 0 {
		a.ExpiresAt = time.Now().Add(time.Duration(resp.Result.TokenExpireDuration) * time.Second).Unix()
	}
	return nil
}
func normalizeExpiresAt(v int64) int64 {
	if v > 1e12 {
		return v / 1000
	}
	return v
}
func (c *Client) ChatStream(a *auth.Auth, body []byte, resolvedModel string) (rc io.ReadCloser, status int, respBody []byte, err error) {
	rc, status, respBody, _, err = c.ChatStreamWithHeaders(a, body, resolvedModel)
	return
}
func (c *Client) ChatStreamWithHeaders(a *auth.Auth, body []byte, resolvedModel string) (rc io.ReadCloser, status int, respBody []byte, headers http.Header, err error) {
	prepared := PrepareBodyResolved(body, a.Variant, resolvedModel)
	return c.ChatPreparedStreamWithHeaders(a, prepared)
}

// prepared is request-local and already normalized for this identity/model.
func (c *Client) ChatPreparedStreamWithHeaders(a *auth.Auth, prepared []byte) (rc io.ReadCloser, status int, respBody []byte, headers http.Header, err error) {
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpChat, bytes.NewReader(prepared))
	if err != nil {
		return nil, 0, nil, nil, err
	}
	SOLOHeaders(req, a, true)
	hc := c.HTTP
	if c.StreamHTTP != nil {
		hc = c.StreamHTTP
	}
	resp, err := hc.Do(req)
	if err != nil {
		log.Printf("chat_stream uid=%s: transport error: %v", a.UID, err)
		return nil, 0, nil, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		kind := Classify(resp.StatusCode, string(raw))
		log.Printf("Trae upstream status=%d kind=%s", resp.StatusCode, kind)
		return nil, resp.StatusCode, raw, resp.Header.Clone(), nil
	}
	return resp.Body, resp.StatusCode, nil, resp.Header.Clone(), nil
}

type ModelInfo struct {
	ReasoningOptions       []string `json:"reasoning_options,omitempty"`
	DefaultReasoning       string   `json:"default_reasoning,omitempty"`
	ID                     string
	Name                   string
	ContextWindow          int64 // = context_window_tokens.dev（目录未给则 0）
	MaxTokens              int64
	Multimodal             *bool        `json:"multimodal,omitempty"`
	Capability             string       `json:"capability,omitempty"`
	ThinkingControl        *bool        `json:"thinking_control,omitempty"`
	ToolResponseMultimodal *bool        `json:"tool_response_multimodal,omitempty"`
	EntitlementCosts       []float64    `json:"entitlement_usage_cost,omitempty"`
	Pricing                []ModelPrice `json:"pricing,omitempty"`
}

var soloAgentOnlyDeadNames = map[string]struct{}{
	"agnes-2.0-flash":   {},
	"agnes-agent-x":     {},
	"deepseek-v4-flash": {},
	"deepseek-v4-pro":   {},
}

func configIsSoloAgentOnly(configName string) bool {
	_, dead := soloAgentOnlyDeadNames[strings.ToLower(configName)]
	return dead
}
func (c *Client) FetchModels(a *auth.Auth) ([]ModelInfo, error) {
	body := map[string]any{
		"function":            FunctionFor(a.Variant),
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	}
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, c.agentBase()+EpModels, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	SOLOHeaders(req, a, false)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ConfigInfoList *[]struct {
			ConfigName          string `json:"config_name"`
			ConfigSwitch        *bool  `json:"config_switch"`
			IsInvisibleToUser   *bool  `json:"is_invisible_to_user"`
			ContextWindowTokens struct {
				Dev int64 `json:"dev"`
			} `json:"context_window_tokens"`
			DisplayConfig         modelDisplay    `json:"display_config"`
			Contact               json.RawMessage `json:"display_contact_config"`
			ReasoningEffortConfig struct {
				SupportThinking *bool             `json:"support_thinking"`
				Options         []json.RawMessage `json:"options"`
				DefaultLevel    string            `json:"default_level"`
			} `json:"reasoning_effort_config"`
			Details []struct {
				MaxTokens       int64           `json:"max_tokens"`
				EntitlementCost json.RawMessage `json:"model_entitlement_usage_cost"`
			} `json:"model_detail_list"`
		} `json:"config_info_list"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("models parse: %w", err)
	}
	if resp.ConfigInfoList == nil {
		return nil, fmt.Errorf("models api response missing catalog")
	}
	out := make([]ModelInfo, 0, len(*resp.ConfigInfoList))
	for _, cfg := range *resp.ConfigInfoList {
		if cfg.ConfigName == "" {
			continue
		}
		if cfg.ConfigSwitch != nil && !*cfg.ConfigSwitch {
			continue // 已下线开关
		}
		if cfg.IsInvisibleToUser != nil && *cfg.IsInvisibleToUser {
			continue // 内部配置，非用户可选模型
		}
		if cfg.DisplayConfig.DisplayName == "" {
			continue // 租户自定义占位模板
		}
		if cfg.DisplayConfig.Invisible || cfg.DisplayConfig.Custom || builderPresent(cfg.DisplayConfig.Builder) {
			continue
		}
		if configIsSoloAgentOnly(cfg.ConfigName) {
			continue
		}
		maxTokens := int64(0)
		costs := []float64{}
		for _, detail := range cfg.Details {
			var cost *float64
			if json.Unmarshal(detail.EntitlementCost, &cost) == nil && cost != nil && *cost >= 0 {
				costs = append(costs, *cost)
			}
			if detail.MaxTokens > 0 && (maxTokens == 0 || detail.MaxTokens < maxTokens) {
				maxTokens = detail.MaxTokens
			}
		}
		options := []string{}
		for _, raw := range cfg.ReasoningEffortConfig.Options {
			var option string
			if json.Unmarshal(raw, &option) == nil && NativeEffortName(option) != "" && !slices.Contains(options, option) {
				options = append(options, option)
			}
		}
		out = append(out, ModelInfo{
			ReasoningOptions:       options,
			DefaultReasoning:       cfg.ReasoningEffortConfig.DefaultLevel,
			ID:                     cfg.ConfigName,
			Name:                   cfg.DisplayConfig.DisplayName,
			ContextWindow:          cfg.ContextWindowTokens.Dev,
			MaxTokens:              maxTokens,
			Multimodal:             cfg.DisplayConfig.Multimodal,
			Capability:             cfg.DisplayConfig.Capability,
			ThinkingControl:        cfg.ReasoningEffortConfig.SupportThinking,
			ToolResponseMultimodal: cfg.DisplayConfig.ToolResponseMultimodal,
			EntitlementCosts:       costs,
			Pricing:                []ModelPrice{parseModelPrice(cfg.Contact, a.Variant)},
		})
	}
	return out, nil
}

type CheckinStatusResult struct {
	CheckedIn     bool   `json:"checked_in"`
	DidCheckedIn  bool   `json:"did_checked_in"`
	Credits       int64  `json:"credits"`
	ExtraCredits  int64  `json:"extra_credits"`
	Enable        bool   `json:"enable"`
	Code          int32  `json:"code"` // 业务码：0=成功，9074=活动校验拒绝，其他=会话类失败
	Message       string `json:"message"`
	SchemeUsed    string `json:"-"`
	ReqSourceUsed string `json:"-"`
}

const (
	UgSchemeCloudIDEJWT = "Cloud-IDE-JWT"
	UgSchemeBearer      = "Bearer"
)

func ugCheckinReqSourcesFor(variant string) []string {
	if strings.ToLower(strings.TrimSpace(variant)) == "solo" {
		return []string{
			`{"req_source":2}`,
			`{"req_source":1}`,
			`{}`,
		}
	}
	return []string{
		`{"req_source":1}`,
		`{}`,
		`{"req_source":2}`,
	}
}
func ugBodyLabel(body string) string {
	switch body {
	case `{"req_source":1}`:
		return "req_source=1"
	case `{"req_source":2}`:
		return "req_source=2"
	case `{}`:
		return "empty"
	}
	return body
}
func ugProbeDetail(a *auth.Auth, attempted []string) string {
	if len(attempted) == 0 {
		return fmt.Sprintf("[无已尝试组合; variant=%s device_id_set=%v]",
			strings.TrimSpace(a.Variant), strings.TrimSpace(a.DeviceID) != "")
	}
	return fmt.Sprintf("[已尝试 %d 组合 %s 全部被拒; variant=%s device_id_set=%v]",
		len(attempted), strings.Join(attempted, ","),
		strings.TrimSpace(a.Variant), strings.TrimSpace(a.DeviceID) != "")
}
func ugCheckinSchemes() []string {
	return []string{UgSchemeCloudIDEJWT, UgSchemeBearer}
}

const checkinDeviceDigits = 16

func NewCheckinDeviceID() string {
	max := new(big.Int).Exp(big.NewInt(10), big.NewInt(checkinDeviceDigits), nil)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return fmt.Sprintf("%0*d", checkinDeviceDigits, time.Now().UnixMilli())
	}
	return fmt.Sprintf("%0*d", checkinDeviceDigits, n.Int64())
}
func checkinDeviceArg(a *auth.Auth, deviceID []string) string {
	if len(deviceID) > 0 && deviceID[0] != "" {
		return deviceID[0]
	}
	return CheckinDeviceFor(a)
}
func isNumericIDShape(s string) bool {
	if len(s) < 8 || len(s) > 24 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
func CheckinDeviceFor(a *auth.Auth) string {
	if a != nil {
		if d := strings.TrimSpace(a.DeviceID); isNumericIDShape(d) {
			return d
		}
	}
	return NewCheckinDeviceID()
}
func RotateCheckinDevice(a *auth.Auth) bool {
	if a == nil {
		return false
	}
	a.Lock()
	defer a.Unlock()
	a.DeviceID = NewCheckinDeviceID()
	return true
}
func ugCheckinRequest(a *auth.Auth, method, url, body, scheme, deviceID string) (*http.Request, error) {
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		return nil, err
	}
	ugBaseHeaders(req, a)
	if deviceID != "" {
		req.Header.Set("X-Device-Id", deviceID)
	}
	if scheme == UgSchemeBearer {
		req.Header.Set("Authorization", "Bearer "+a.JWT()) // 读锁快照
	} else {
		req.Header.Set("Authorization", UgSchemeCloudIDEJWT+" "+a.JWT()) // 读锁快照
	}
	return req, nil
}
func (c *Client) ugCheckinOnce(a *auth.Auth, method, url, body, scheme, deviceID string) (int32, string, []byte, error) {
	req, err := ugCheckinRequest(a, method, url, body, scheme, deviceID)
	if err != nil {
		return 0, "", nil, err
	}
	data, err := c.doJSON(req)
	if err != nil {
		return 0, "", nil, err
	}
	var reply struct {
		Code    int32  `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &reply); err != nil {
		return 0, "", nil, fmt.Errorf("checkin %s parse: %w", strings.ToLower(method), err)
	}
	return reply.Code, reply.Message, data, nil
}

func (c *Client) CheckinStatus(a *auth.Auth, deviceID ...string) (*CheckinStatusResult, error) {
	did := checkinDeviceArg(a, deviceID)
	bodies := ugCheckinReqSourcesFor(a.Variant)
	attempted := make([]string, 0, len(bodies)*2) // v0.12.45: 实际尝试的 body×scheme
	var lastBiz *Error
	for _, body := range bodies {
		for _, scheme := range ugCheckinSchemes() {
			code, msg, data, err := c.ugCheckinOnce(a, http.MethodPost, c.ugBase()+EpCheckinStatus, body, scheme, did)
			if err != nil {
				return nil, err
			}
			attempted = append(attempted, ugBodyLabel(body)+"×"+scheme)
			if code == 0 {
				var resp CheckinStatusResult
				if err := json.Unmarshal(data, &resp); err != nil {
					return nil, fmt.Errorf("checkin status parse: %w", err)
				}
				resp.SchemeUsed = scheme
				resp.ReqSourceUsed = body
				return &resp, nil
			}
			log.Printf("checkin status: %s scheme %s -> biz_code=%d msg=%q", ugBodyLabel(body), scheme, code, msg)
			lastBiz = bizError(code, "获取签到状态失败", msg)
			if code == 9074 {
				break // 活动校验拒绝：换 req_source 再试（v0.12.41），同源换鉴权方案无意义
			}
		}
	}
	if lastBiz != nil {
		lastBiz.Msg += " " + ugProbeDetail(a, attempted)
	}
	return nil, lastBiz
}

type CheckinClaimResult struct {
	Code          int32  `json:"code"`
	Message       string `json:"message"`
	ClaimCredits  *int64 `json:"-"`
	SchemeUsed    string `json:"-"`
	ReqSourceUsed string `json:"-"`
}

func (c *Client) CheckinClaim(a *auth.Auth, deviceID ...string) (*CheckinClaimResult, error) {
	did := checkinDeviceArg(a, deviceID)
	bodies := ugCheckinReqSourcesFor(a.Variant)
	attempted := make([]string, 0, len(bodies)*2) // v0.12.45: 实际尝试的 body×scheme
	var lastBiz *Error
	for _, body := range bodies {
		for _, scheme := range ugCheckinSchemes() {
			code, msg, data, err := c.ugCheckinOnce(a, http.MethodPost, c.ugBase()+EpCheckinClaim, body, scheme, did)
			if err != nil {
				return nil, err
			}
			attempted = append(attempted, ugBodyLabel(body)+"×"+scheme)
			if code == 0 {
				var resp CheckinClaimResult
				if err := json.Unmarshal(data, &resp); err != nil {
					return nil, fmt.Errorf("checkin claim parse: %w", err)
				}
				resp.ClaimCredits = pickCreditField(data)
				resp.SchemeUsed = scheme
				resp.ReqSourceUsed = body
				return &resp, nil
			}
			log.Printf("checkin claim: %s scheme %s -> biz_code=%d msg=%q", ugBodyLabel(body), scheme, code, msg)
			lastBiz = bizError(code, "签到领取失败", msg)
			if code == 9074 {
				break // 活动校验拒绝：换 req_source 再试（v0.12.41），同源换鉴权方案无意义
			}
		}
	}
	if lastBiz != nil {
		lastBiz.Msg += " " + ugProbeDetail(a, attempted)
	}
	return nil, lastBiz
}
func pickCreditField(data []byte) *int64 {
	var probe map[string]any
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil
	}
	candidates := []string{"credits", "add_credits", "reward_credits", "credited", "award_credits", "obtain_credits"}
	for _, prefix := range []string{"", "data"} {
		var m map[string]any
		if prefix == "" {
			m = probe
		} else {
			inner, ok := probe[prefix].(map[string]any)
			if !ok {
				continue
			}
			m = inner
		}
		for _, k := range candidates {
			if v, ok := m[k].(float64); ok {
				n := int64(v)
				return &n
			}
		}
	}
	return nil
}

type EntitlementPack struct {
	IsHide              bool `json:"is_hide"`
	Status              *int `json:"status"`
	EntitlementBaseInfo struct {
		ProductType  int       `json:"product_type"` // 0=Free, 1=Pro, 4=Pro+, 5=Pro+CN, 6=Ultra, 8=Lite, 9=Trial, 100=CNExpress
		EndTime      int64     `json:"end_time"`
		IsHide       bool      `json:"is_hide"`
		Status       *int      `json:"status"` // nil 或 1=active，3=cancelled
		Quota        PackQuota `json:"quota"`
		ProductExtra struct {
			SubscriptionExtra struct {
				Quota PackQuota `json:"quota"`
			} `json:"subscription_extra"`
			PackageExtra struct {
				Quota PackQuota `json:"quota"`
			} `json:"package_extra"`
		} `json:"product_extra"`
	} `json:"entitlement_base_info"`
	Usage       PackUsage `json:"usage"`
	DisplayDesc string    `json:"display_desc"` // 上游 identityStr 优先取 display_desc（CN 选中包）
}

// CN currently puts visibility/status on the pack; older responses used base info.
func (p *EntitlementPack) Excluded() bool {
	return p.EntitlementBaseInfo.ProductType == 3 || p.IsHide || p.EntitlementBaseInfo.IsHide ||
		p.Status != nil && *p.Status == 3 || p.EntitlementBaseInfo.Status != nil && *p.EntitlementBaseInfo.Status == 3
}

type PackQuota struct {
	BasicUsageLimit              *int64 `json:"basic_usage_limit"`
	BonusUsageLimit              *int64 `json:"bonus_usage_limit"`
	PremiumModelFastRequestLimit *int64 `json:"premium_model_fast_request_limit"` // -1=unlimited
	CreditsLimit                 *int64 `json:"credits_limit"`
}
type PackUsage struct {
	BasicUsageAmount       *int64   `json:"basic_usage_amount"`
	BonusUsageAmount       *int64   `json:"bonus_usage_amount"`
	PremiumModelFastAmount *int64   `json:"premium_model_fast_amount"`
	IsFlashConsuming       bool     `json:"is_flash_consuming"`
	CreditsAmount          *float64 `json:"credits_amount"`
}

func (p *EntitlementPack) EffectiveQuota() PackQuota {
	base := p.EntitlementBaseInfo.Quota
	if base.hasAny() {
		return base
	}
	sub := p.EntitlementBaseInfo.ProductExtra.SubscriptionExtra.Quota
	if sub.hasAny() {
		return sub
	}
	return p.EntitlementBaseInfo.ProductExtra.PackageExtra.Quota
}

func (q PackQuota) hasAny() bool {
	return q.BasicUsageLimit != nil || q.BonusUsageLimit != nil || q.PremiumModelFastRequestLimit != nil || q.CreditsLimit != nil
}
func (p *EntitlementPack) PackRemain() (remain int64, ok bool) {
	q := p.EffectiveQuota()
	if q.BasicUsageLimit == nil {
		return 0, false
	}
	used := int64(0)
	if p.Usage.BasicUsageAmount != nil {
		used = *p.Usage.BasicUsageAmount
	}
	left := *q.BasicUsageLimit - used
	if q.BonusUsageLimit != nil {
		bonusUsed := int64(0)
		if p.Usage.BonusUsageAmount != nil {
			bonusUsed = *p.Usage.BonusUsageAmount
		}
		if bonusLeft := *q.BonusUsageLimit - bonusUsed; bonusLeft > 0 {
			left += bonusLeft
		}
	}
	if left < 0 {
		left = 0
	}
	return left, true
}

type EntUsageResult struct {
	IsCreditsBilling        bool              `json:"is_credits_billing"`
	UserEntitlementPackList []EntitlementPack `json:"user_entitlement_pack_list"`
}

func (c *Client) UserEntUsage(a *auth.Auth) (*EntUsageResult, error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpEntUsage, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp EntUsageResult
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("ent usage parse: %w", err)
	}
	return &resp, nil
}
func FastRequestUsage(packs []EntitlementPack, dashboardPayload bool) (available, limit, used int64, hasEvidence bool) {
	var filtered []EntitlementPack
	for _, p := range packs {
		if p.Excluded() {
			continue
		}
		filtered = append(filtered, p)
	}
	if len(filtered) == 0 {
		return 0, 0, 0, false
	}
	hasEvidence = dashboardPayload
	unlimited := false
	for _, p := range filtered {
		q := p.EffectiveQuota()
		if q.PremiumModelFastRequestLimit != nil {
			hasEvidence = true
			if *q.PremiumModelFastRequestLimit == -1 {
				unlimited = true
			} else {
				limit += *q.PremiumModelFastRequestLimit
			}
		}
		if p.Usage.PremiumModelFastAmount != nil {
			hasEvidence = true
			used += *p.Usage.PremiumModelFastAmount
		}
	}
	if !hasEvidence {
		return 0, 0, 0, false
	}
	if unlimited {
		return -1, -1, used, true
	}
	available = limit - used
	if available < 0 {
		available = 0
	}
	return available, limit, used, true
}

type CreditsPoolInfo struct {
	Remain    int64
	Known     bool
	Unlimited bool
}

func CreditsPoolUsage(packs []EntitlementPack, isCreditsBilling bool) CreditsPoolInfo {
	var filtered []EntitlementPack
	for _, p := range packs {
		if p.Excluded() {
			continue
		}
		filtered = append(filtered, p)
	}
	hasField := false
	for _, p := range filtered {
		if p.EffectiveQuota().CreditsLimit != nil {
			hasField = true
			break
		}
	}
	if !isCreditsBilling && !hasField {
		return CreditsPoolInfo{}
	}
	if len(filtered) == 0 {
		return CreditsPoolInfo{Known: isCreditsBilling}
	}
	unlimited := false
	var total int64
	for _, p := range filtered {
		q := p.EffectiveQuota()
		if q.CreditsLimit == nil {
			continue // 官方 `?? 0`：无字段贡献 0
		}
		if *q.CreditsLimit == -1 {
			unlimited = true
			continue
		}
		used := float64(0)
		if p.Usage.CreditsAmount != nil {
			used = *p.Usage.CreditsAmount
		}
		left := float64(*q.CreditsLimit) - used
		if left < 0 {
			left = 0
		}
		total += int64(left + 0.5) // 官方 Math.round
	}
	if unlimited {
		return CreditsPoolInfo{Remain: -1, Known: true, Unlimited: true}
	}
	return CreditsPoolInfo{Remain: total, Known: true}
}

type PayStatusResult struct {
	Code               int    `json:"code"`
	UserPayIdentityStr string `json:"user_pay_identity_str"`

	Detail          map[string]json.RawMessage `json:"detail"`
	Quota           map[string]json.RawMessage `json:"quota"`
	EntitlementInfo *PayStatusNested           `json:"entitlementInfo"`
	OriginPayStatus *PayStatusOrigin           `json:"originPayStatusData"`
}
type PayStatusNested struct {
	Detail map[string]json.RawMessage `json:"detail"`
	Quota  map[string]json.RawMessage `json:"quota"`
}
type PayStatusOrigin struct {
	Detail map[string]json.RawMessage `json:"detail"`
}

func (p *PayStatusResult) pickInt(quota bool, keys ...string) *int64 {
	maps := make([]map[string]json.RawMessage, 0, 3)
	if quota {
		if p.Quota != nil {
			maps = append(maps, p.Quota)
		}
		if p.EntitlementInfo != nil && p.EntitlementInfo.Quota != nil {
			maps = append(maps, p.EntitlementInfo.Quota)
		}
	} else {
		if p.Detail != nil {
			maps = append(maps, p.Detail)
		}
		if p.EntitlementInfo != nil && p.EntitlementInfo.Detail != nil {
			maps = append(maps, p.EntitlementInfo.Detail)
		}
		if p.OriginPayStatus != nil && p.OriginPayStatus.Detail != nil {
			maps = append(maps, p.OriginPayStatus.Detail)
		}
	}
	for _, m := range maps {
		for _, k := range keys {
			if raw, ok := m[k]; ok {
				var v int64
				if err := json.Unmarshal(raw, &v); err == nil {
					out := v
					return &out
				}
			}
		}
	}
	return nil
}
func (p *PayStatusResult) FastRequestPer() *int64 {
	return p.pickInt(false, "fast_request_per", "fastRequestPer")
}
func (p *PayStatusResult) CanGetExpressStatus() *int64 {
	return p.pickInt(false, "can_get_express_status", "canGetExpressStatus")
}
func (p *PayStatusResult) SoloParallelLimit() *int64 {
	return p.pickInt(true, "solo_agent_parallel_limit")
}
func (p *PayStatusResult) HasSoloPackage() bool {
	if p.Quota == nil && (p.EntitlementInfo == nil || p.EntitlementInfo.Quota == nil) {
		return false
	}
	maps := []map[string]json.RawMessage{}
	if p.Quota != nil {
		maps = append(maps, p.Quota)
	}
	if p.EntitlementInfo != nil && p.EntitlementInfo.Quota != nil {
		maps = append(maps, p.EntitlementInfo.Quota)
	}
	for _, key := range []string{"enable_solo_agent", "enable_solo_builder", "enable_solo_coder", "enable_solo_lite", "enable_solo_web"} {
		for _, m := range maps {
			if raw, ok := m[key]; ok {
				var v bool
				if err := json.Unmarshal(raw, &v); err == nil && v {
					return true
				}
			}
		}
	}
	return false
}
func (p *PayStatusResult) PlanIdentity() string {
	return strings.TrimSpace(p.UserPayIdentityStr)
}
func (c *Client) PayStatus(a *auth.Auth) (*PayStatusResult, error) {
	req, err := http.NewRequest(http.MethodPost, c.ugBase()+EpPayStatus, bytes.NewReader([]byte("{}")))
	if err != nil {
		return nil, err
	}
	UgHeaders(req, a)
	data, err := c.doJSON(req)
	if err != nil {
		return nil, err
	}
	var resp PayStatusResult
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("pay status parse: %w", err)
	}
	return &resp, nil
}

type UsageSummary struct {
	UsageModel     string // "fast" | "basic" | "unknown"
	Remain         int64  // fast: 可用次数(-1 无限)；basic: 套餐剩余
	RemainKnown    bool
	FastLimit      int64
	FastUsed       int64
	Used           int64  // basic 模型的已用量（展示"已用"）
	Total          int64  // basic 模型的额度池（展示"额度池"）
	FastRequestPer *int64 // detail.fast_request_per（快请求/月）
	SoloParallel   *int64 // quota.solo_agent_parallel_limit（SOLO 并发）
	SoloPackage    bool   // quota.enable_solo_* 任一为 true
	PlanType       string // user_pay_identity_str（"free" 等，Free 判定用）
	CreditsPool    CreditsPoolInfo
}

func IsFreePlan(plan, planType string) bool {
	p := strings.ToLower(strings.TrimSpace(plan))
	t := strings.ToLower(strings.TrimSpace(planType))
	return strings.Contains(p, "free") || strings.Contains(p, "免费") ||
		strings.Contains(t, "free") || strings.Contains(t, "免费")
}
func SummarizeUsage(packs []EntitlementPack, isCN bool) UsageSummary {
	fastAvail, fastLimit, fastUsed, hasFast := FastRequestUsage(packs, false)
	if isCN && hasFast {
		return UsageSummary{
			UsageModel:  "fast",
			Remain:      fastAvail,
			RemainKnown: true,
			FastLimit:   fastLimit,
			FastUsed:    fastUsed,
		}
	}
	selected := SelectActivePack(packs, isCN)
	if selected != nil {
		if remain, ok := selected.PackRemain(); ok {
			q := selected.EffectiveQuota()
			var used, total int64
			if q.BasicUsageLimit != nil {
				total = *q.BasicUsageLimit
			}
			if selected.Usage.BasicUsageAmount != nil {
				used = *selected.Usage.BasicUsageAmount
			}
			return UsageSummary{
				UsageModel:  "basic",
				Remain:      remain,
				RemainKnown: true,
				Used:        used,
				Total:       total,
			}
		}
	}
	if !isCN && hasFast {
		return UsageSummary{UsageModel: "fast", Remain: fastAvail, RemainKnown: true, FastLimit: fastLimit, FastUsed: fastUsed}
	}
	return UsageSummary{UsageModel: "unknown"}
}
func PackListRemain(packs []EntitlementPack, isCN bool) (score int64, known bool) {
	sum := SummarizeUsage(packs, isCN)
	if !sum.RemainKnown {
		return 0, false
	}
	if sum.Remain < 0 { // unlimited fast requests
		return 1 << 30, true
	}
	return sum.Remain, true
}
func SelectActivePack(packs []EntitlementPack, isCN bool) *EntitlementPack {
	var filtered []EntitlementPack
	for _, p := range packs {
		if p.Excluded() {
			continue
		}
		filtered = append(filtered, p)
	}
	if len(filtered) == 0 {
		return nil
	}
	priorityCN := []int{100, 6, 5, 4, 1, 9, 8, 0}
	priorityIntl := []int{6, 4, 1, 9, 8, 0}
	order := priorityIntl
	if isCN {
		order = priorityCN
	}
	for _, pt := range order {
		for i := range filtered {
			if filtered[i].EntitlementBaseInfo.ProductType == pt {
				return &filtered[i]
			}
		}
	}
	return &filtered[0] // fallback: first active pack
}
func ProductTypeIdentity(productType int, isCN bool) string {
	switch productType {
	case 100:
		if isCN {
			return "CNExpress"
		}
		return "Unknown"
	case 6:
		return "Ultra"
	case 5:
		if isCN {
			return "Pro+"
		}
		return "Pro+"
	case 4:
		return "Pro+"
	case 1, 9:
		return "Pro" // Trial/SoloInvite → Pro
	case 8:
		return "Lite"
	case 0:
		return "Free"
	default:
		return "Unknown"
	}
}
func IsRateLimit9074(code int32) bool {
	return code == 9074
}
func (c *Client) GetUserInfo(a *auth.Auth) (uid, nickname, enterpriseID string, err error) {
	var raw json.RawMessage
	uid, nickname, enterpriseID, raw, err = c.GetUserInfoFull(a)
	_ = raw
	return uid, nickname, enterpriseID, err
}
func (c *Client) GetUserInfoFull(a *auth.Auth) (uid, nickname, enterpriseID string, raw json.RawMessage, err error) {
	body := map[string]any{"ReqSource": "IDE", "IDEVersion": IdeVersion}
	rawBody, _ := json.Marshal(body)
	var data json.RawMessage
	var lastErr error
	for _, host := range exchangeHosts(a.APIHost, c.OAuthHost) {
		req, rerr := http.NewRequest(http.MethodPost, host+EpUserInfo, bytes.NewReader(rawBody))
		if rerr != nil {
			return "", "", "", nil, rerr
		}
		OAuthHeaders(req)
		req.Header.Set("X-Cloudide-Token", a.JWT()) // 读锁快照
		data, lastErr = c.doJSON(req)
		if lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		return "", "", "", nil, lastErr
	}
	var resp struct {
		Result struct {
			UserID       string `json:"UserID"`
			ScreenName   string `json:"ScreenName"`
			EnterpriseID string `json:"EnterpriseID"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", "", nil, fmt.Errorf("userinfo parse: %w", err)
	}
	return resp.Result.UserID, resp.Result.ScreenName, resp.Result.EnterpriseID, data, nil
}

type CheckLoginResult struct {
	IsLogin          bool
	BoundDeviceID    string
	DeviceBindStatus string
	Host             string
	Region           string
	AIRegion         string
	UserID           string
	ExpiredAt        int64 // ms epoch, 0 = absent
	Raw              json.RawMessage
}

func (c *Client) CheckLogin(a *auth.Auth) (*CheckLoginResult, error) {
	body := map[string]any{"IDEVersion": IdeVersion}
	rawBody, _ := json.Marshal(body)
	var data json.RawMessage
	var lastErr error
	for _, host := range exchangeHosts(a.APIHost, c.OAuthHost) {
		req, rerr := http.NewRequest(http.MethodPost, host+EpCheckLogin, bytes.NewReader(rawBody))
		if rerr != nil {
			return nil, rerr
		}
		OAuthHeaders(req)
		req.Header.Set("X-Cloudide-Token", a.JWT()) // 读锁快照
		data, lastErr = c.doJSON(req)
		if lastErr == nil {
			break
		}
	}
	if lastErr != nil {
		return nil, lastErr
	}
	var resp struct {
		Result struct {
			IsLogin          bool   `json:"IsLogin"`
			BoundDeviceID    string `json:"BoundDeviceID"`
			DeviceBindStatus string `json:"DeviceBindStatus"`
			Host             string `json:"Host"`
			Region           string `json:"Region"`
			AIRegion         string `json:"AIRegion"`
			UserID           string `json:"UserID"`
			ExpiredAt        any    `json:"ExpiredAt"`
		} `json:"Result"`
		TopAIRegion string `json:"AIRegion"`
		TopRegion   string `json:"storeRegion"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("checklogin parse: %w", err)
	}
	if resp.Result.AIRegion == "" {
		resp.Result.AIRegion = resp.TopAIRegion
	}
	if resp.Result.Region == "" {
		resp.Result.Region = resp.TopRegion
	}
	out := &CheckLoginResult{
		IsLogin:          resp.Result.IsLogin,
		BoundDeviceID:    resp.Result.BoundDeviceID,
		DeviceBindStatus: resp.Result.DeviceBindStatus,
		Host:             resp.Result.Host,
		Region:           resp.Result.Region,
		AIRegion:         resp.Result.AIRegion,
		UserID:           resp.Result.UserID,
		Raw:              data,
	}
	if n, ok := numericAnyToI64(resp.Result.ExpiredAt); ok && n > 0 {
		out.ExpiredAt = n
	}
	return out, nil
}
func numericAnyToI64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i, true
		}
	}
	return 0, false
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

func NativeEffortName(native string) string {
	switch native {
	case "light":
		return "low"
	case "medium", "high", "max":
		return native
	case "extra_high":
		return "xhigh"
	}
	return ""
}
func (m ModelInfo) ReasoningEfforts() []string {
	out := []string{}
	for _, option := range m.ReasoningOptions {
		if name := NativeEffortName(option); name != "" && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	if slices.Contains(out, "xhigh") && !slices.Contains(out, "max") {
		out = append(out, "max")
	}
	return out
}
func (m ModelInfo) NativeEffort(effort string) (string, bool) {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if native := NativeEffortName(effort); native != "" {
		effort = native
	}
	for _, option := range m.ReasoningOptions {
		if NativeEffortName(option) == effort {
			return option, true
		}
	}
	if effort == "max" && slices.Contains(m.ReasoningOptions, "extra_high") && !slices.Contains(m.ReasoningOptions, "max") {
		return "extra_high", true
	}
	return "", false
}
