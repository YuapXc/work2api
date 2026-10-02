package account

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"work2api/internal/qoder/logger"
)

// OAuth 认证流程
// 1. StartLogin: 生成 PKCE 参数，返回登录 URL
// 2. WaitLogin: 轮询 deviceToken/poll 端点，等待用户授权
// 3. 获取 device token (dt-xxx)，用于后续 API 调用

const oauthClientID = "e883ade2-e6e3-4d6d-adf7-f92ceff5fdcb"

type pendingOAuth struct {
	loginID  string
	nonce    string
	verifier string
	region   Region
	ctx      context.Context
	cancel   context.CancelFunc
	deadline time.Time
}

var (
	pendingMu sync.Mutex
	pending   *pendingOAuth
)

// StartLogin 启动 OAuth 登录流程
func StartLogin(region Region) (*OAuthSession, error) {
	verifier, challenge, err := pkce()
	if err != nil {
		return nil, err
	}
	nonce := newSimpleID()
	loginID := newSimpleID()

	ep := GetEndpoints(region)
	params := url.Values{}
	params.Set("nonce", nonce)
	params.Set("challenge", challenge)
	params.Set("challenge_method", "S256")
	params.Set("client_id", oauthClientID)
	loginURL := ep.DeviceLoginBase + "?" + params.Encode()

	logger.Info("OAuth: StartLogin loginID=%s region=%s", loginID, region)

	pendingMu.Lock()
	if pending != nil {
		pending.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	deadline := time.Now().Add(10 * time.Minute)
	pending = &pendingOAuth{
		loginID:  loginID,
		nonce:    nonce,
		verifier: verifier,
		region:   region,
		ctx:      ctx,
		cancel:   cancel,
		deadline: deadline,
	}
	pendingMu.Unlock()

	return &OAuthSession{LoginID: loginID, LoginURL: loginURL, ExpiresAt: deadline.Unix()}, nil
}

// WaitLogin 等待用户完成 OAuth 授权
func WaitLogin(loginID string) (*Account, error) {
	logger.Debug("OAuth: WaitLogin called loginID=%s", loginID)

	pendingMu.Lock()
	p := pending
	pendingMu.Unlock()

	if p == nil || p.loginID != loginID {
		logger.Error("OAuth: no pending login for id %s", loginID)
		return nil, fmt.Errorf("no pending login for id %s", loginID)
	}

	ep := GetEndpoints(p.region)

	logger.Info("OAuth: Starting poll loop")
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if time.Now().After(p.deadline) {
			logger.Error("OAuth: Timeout reached")
			return nil, fmt.Errorf("oauth login timed out")
		}
		select {
		case <-p.ctx.Done():
			logger.Info("OAuth: Cancelled")
			return nil, fmt.Errorf("oauth login cancelled")
		case <-ticker.C:
			deviceToken, refreshToken, err := pollToken(p.nonce, p.verifier, ep)
			if err != nil {
				continue
			}
			logger.Info("OAuth: Got token, building account")
			acct, err := buildAccountFromToken(deviceToken, refreshToken, p.region, ep)
			if err != nil {
				logger.Error("OAuth: Build account error: %v", err)
				return nil, err
			}
			pendingMu.Lock()
			pending = nil
			pendingMu.Unlock()
			logger.Info("OAuth: Success! Account: %s", acct.Name)
			return acct, nil
		}
	}
}

func CancelLogin(loginID string) {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	if pending != nil && pending.loginID == loginID {
		pending.cancel()
		pending = nil
	}
}

// pollToken 轮询 deviceToken/poll 端点
func pollToken(nonce, verifier string, ep Endpoints) (string, string, error) {
	reqURL := fmt.Sprintf("%s?nonce=%s&verifier=%s&challenge_method=S256",
		ep.PollEndpoint,
		url.QueryEscape(nonce),
		url.QueryEscape(verifier))

	logger.Debug("OAuth: Polling %s", ep.PollEndpoint)

	resp, err := http.Get(reqURL)
	if err != nil {
		logger.Error("OAuth: Poll error: %v", err)
		return "", "", err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	logger.Debug("OAuth: Response status=%d", resp.StatusCode)

	// 404 表示还没授权，继续等待
	if resp.StatusCode == 404 {
		return "", "", fmt.Errorf("not authorized yet")
	}

	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("poll: HTTP %d", resp.StatusCode)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", "", err
	}

	deviceToken, _ := result["token"].(string)
	refreshToken, _ := result["refresh_token"].(string)

	if deviceToken == "" {
		return "", "", fmt.Errorf("no device token in response")
	}
	if refreshToken != "" {
		logger.Info("OAuth: Got device token and refresh token")
	} else {
		logger.Info("OAuth: Got device token")
	}
	return deviceToken, refreshToken, nil
}

// buildAccountFromToken 使用 device token 构建账号信息
func buildAccountFromToken(deviceToken, refreshToken string, region Region, ep Endpoints) (*Account, error) {
	info, err := fetchUserInfo(deviceToken, ep)
	if err != nil {
		return nil, fmt.Errorf("fetch user info: %w", err)
	}
	plan := fetchPlan(deviceToken, ep)

	id := SanitizeID(strGet(info, "userId") + strGet(info, "email"))
	if id == "" {
		id = newSimpleID()
	}
	now := time.Now()
	acct := &Account{
		ID:        id,
		Name:      strGet(info, "name"),
		Email:     strings.ToLower(strGet(info, "email")),
		UserType:  strGet(info, "userType"),
		Plan:      plan,
		Region:    region,
		AuthMode:  "oauth",
		APIMode:   "openai",
		Tags:      []string{},
		CreatedAt: now,
	}
	secretPayload, err := json.Marshal(map[string]string{
		"device_token":  deviceToken,
		"refresh_token": refreshToken,
	})
	if err != nil {
		return nil, err
	}
	if err := SaveSecret(id, string(secretPayload)); err != nil {
		return nil, err
	}
	return acct, nil
}

func fetchUserInfo(token string, ep Endpoints) (map[string]interface{}, error) {
	req, _ := http.NewRequest("GET", ep.UserinfoBase, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	raw, _ := io.ReadAll(resp.Body)
	json.Unmarshal(raw, &result)
	return result, nil
}

func fetchPlan(token string, ep Endpoints) string {
	req, _ := http.NewRequest("GET", ep.PlanEndpoint, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	raw, _ := io.ReadAll(resp.Body)
	json.Unmarshal(raw, &result)
	return strGet(result, "plan_tier_name")
}

func pkce() (verifier, challenge string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	h := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(h[:])
	return
}

func newSimpleID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

func strGet(m map[string]interface{}, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func FetchQuota(token string, region Region) (*QuotaInfo, error) {
	ep := GetEndpoints(region)
	req, _ := http.NewRequest("GET", ep.QuotaEndpoint, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	logger.Debug("FetchQuota %s raw: %s", ep.QuotaEndpoint, string(raw))

	var result map[string]interface{}
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}

	info := &QuotaInfo{
		Plan:            fetchPlan(token, ep),
		IsQuotaExceeded: result["isQuotaExceeded"] == true,
		ExpiresAt:       int64(toFloat(result, "expiresAt")),
	}

	if uq := extractBucket(result, "userQuota"); uq != nil {
		info.UserQuota = uq
		logger.Debug("FetchQuota userQuota: %+v", uq)
	} else {
		logger.Debug("FetchQuota userQuota missing")
	}

	if aq := extractBucket(result, "addOnQuota"); aq != nil {
		info.AddonQuota = aq
		logger.Debug("FetchQuota addonQuota: %+v", aq)
	} else {
		logger.Debug("FetchQuota addonQuota missing")
	}

	info.DedicatedPackages = extractDedicatedPackages(result)
	return info, nil
}

// pkgInactiveHints 是专属资源包 status 枚举里明确表示「不占额度」的片段。活跃态
// 实测是 QUOTA_DETAIL_STATUS_ACTIVE，失效态没有真样本，按「含这些词就算失效」
// 匹配；未知状态放行——宁可多显示一行，也不要把活跃包误杀（漏显额度正是这条
// 链路修过的老 bug）。
var pkgInactiveHints = []string{"EXPIRED", "INVALID", "INACTIVE", "DISABLED", "USED_UP", "DEPLETED"}

// extractDedicatedPackages 读 dedicatedResourcePackages：available + status 双
// 保险判活（失效时上游翻哪个字段没有真样本，只查一个会把过期包算进总额度）。
// 展示名从 displayLabels（dimension=title）取多语言，zh-CN → en-US → value 回退，
// 兜底 name，最后是通用名——name 常是 act-20260901-170 这类活动代号不适合人看。
func extractDedicatedPackages(result map[string]interface{}) []QuotaPackage {
	raw, ok := result["dedicatedResourcePackages"].([]interface{})
	if !ok {
		return nil
	}
	var out []QuotaPackage
	for _, item := range raw {
		pkg, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if avail, has := pkg["available"].(bool); has && !avail {
			continue
		}
		status := strings.ToUpper(strGet(pkg, "status"))
		inactive := false
		for _, hint := range pkgInactiveHints {
			if strings.Contains(status, hint) {
				inactive = true
				break
			}
		}
		if inactive {
			continue
		}
		used := toFloat(pkg, "used")
		total := toFloat(pkg, "total")
		remaining := toFloat(pkg, "remaining")
		if total == 0 && used == 0 && remaining == 0 {
			continue
		}
		out = append(out, QuotaPackage{
			Label:     pkgLabel(pkg),
			Used:      used,
			Total:     total,
			Remaining: remaining,
			ExpireAt:  int64(toFloat(pkg, "expiresAt")),
		})
	}
	return out
}

// pkgLabel 按上游 displayLabels 的 zh-CN → en-US → value → name 回退取展示名。
func pkgLabel(pkg map[string]interface{}) string {
	if labels, ok := pkg["displayLabels"].([]interface{}); ok {
		for _, li := range labels {
			entry, ok := li.(map[string]interface{})
			if !ok || strGet(entry, "dimension") != "title" {
				continue
			}
			i18n, _ := entry["valueI18n"].(map[string]interface{})
			if i18n != nil {
				for _, key := range []string{"zh-CN", "en-US"} {
					if t := strings.TrimSpace(strGet(i18n, key)); t != "" {
						return t
					}
				}
			}
			if t := strings.TrimSpace(strGet(entry, "value")); t != "" {
				return t
			}
		}
	}
	if t := strings.TrimSpace(strGet(pkg, "name")); t != "" {
		return t
	}
	return "专属积分"
}

func extractBucket(data map[string]interface{}, key string) *QuotaBucket {
	obj, ok := data[key].(map[string]interface{})
	if !ok {
		return nil
	}
	used := toFloat(obj, "used")
	total := toFloat(obj, "total")
	remaining := toFloat(obj, "remaining")
	if total == 0 && used == 0 && remaining == 0 {
		return nil
	}
	resetTime := ""
	if v, ok := obj["resetTime"].(string); ok {
		resetTime = v
	} else if v, ok := obj["reset_time"].(string); ok {
		resetTime = v
	}
	return &QuotaBucket{Used: used, Total: total, Remaining: remaining, ResetTime: resetTime}
}

func toFloat(m map[string]interface{}, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	}
	return 0
}
