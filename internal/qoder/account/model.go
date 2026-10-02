package account

import "time"

type Account struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Email     string     `json:"email,omitempty"`
	UserType  string     `json:"user_type,omitempty"`
	Plan      string     `json:"plan,omitempty"`
	Region    Region     `json:"region,omitempty"` // "global" | "cn"，空值视为 global
	AuthMode  string     `json:"auth_mode"`        // "pat" | "oauth"
	APIMode   string     `json:"api_mode"`         // "openai" | "anthropic" | "gemini" | "claude-code"
	Tags      []string   `json:"tags"`
	CreatedAt time.Time  `json:"created_at"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
	Active    bool       `json:"active"`
	SortOrder int        `json:"sort_order"`
}

type OAuthSession struct {
	LoginID   string `json:"login_id"`
	LoginURL  string `json:"login_url"`
	ExpiresAt int64  `json:"expires_at"`
}

type Status struct {
	Running       bool   `json:"running"`
	Port          int    `json:"port"`
	ActiveAccount string `json:"active_account"`
	APIMode       string `json:"api_mode"`
}

type Settings struct {
	Port                 int                          `json:"port"`
	AutoStart            bool                         `json:"auto_start"`
	LogLevel             string                       `json:"log_level"`                  // "info" | "debug" | "error"
	QuotaRefreshInterval int                          `json:"quota_refresh_interval"`     // 秒，0=不自动刷新
	BridgeToken          string                       `json:"bridge_token,omitempty"`     // 自定义鉴权 token，空则使用默认值 "qccg"
	ConsolePassword      string                       `json:"console_password,omitempty"` // Web 控制台登录密码（类似 CPA management key）
	ModelMapping         map[string]string            `json:"model_mapping,omitempty"`    // [DEPRECATED] 旧扁平映射（向后兼容，MapModel 中作为兜底回退使用）
	ModelMappings        map[string]map[string]string `json:"model_mappings,omitempty"`   // agent (claude/codex/gemini) → 客户端模型名 → Qoder model.key
	AutoCheckin          bool                         `json:"auto_checkin"`               // 每日 10:00 自动签到（默认关闭）
	MachineSalt          string                       `json:"machine_salt,omitempty"`     // 本机设备指纹盐（首次启动自动生成；勿修改，变更即指纹漂移）
}

type QuotaInfo struct {
	Plan       string       `json:"plan"`
	UserQuota  *QuotaBucket `json:"user_quota,omitempty"`
	AddonQuota *QuotaBucket `json:"addon_quota,omitempty"`
	// DedicatedPackages 活动赠送的专属资源包（如「Qwen 专属积分」），与订阅
	// 额度/加油包**并存**，是账号总额度的一部分。上游
	// /api/v2/quota/usage 的 dedicatedResourcePackages 节点，此前整条丢弃会让
	// 管理台显示的积分比实际少一截（buddy-proxy #51 实测：2000+2000 只显示 2000）。
	DedicatedPackages []QuotaPackage `json:"dedicated_packages,omitempty"`
	IsQuotaExceeded   bool           `json:"is_quota_exceeded"`
	ExpiresAt         int64          `json:"expires_at,omitempty"`
}

// QuotaPackage 是一个专属资源包的额度切片。available/status 双信号判活：
// 失效时上游翻哪个字段没有真样本，只信一个可能把「少显示」翻成「多显示」。
type QuotaPackage struct {
	Label     string  `json:"label"`
	Used      float64 `json:"used"`
	Total     float64 `json:"total"`
	Remaining float64 `json:"remaining"`
	// ExpireAt 包自带的过期时间（毫秒 epoch），通常比账号级的更早；0 表示上游未给。
	ExpireAt int64 `json:"expire_at,omitempty"`
}

type QuotaBucket struct {
	Used      float64 `json:"used"`
	Total     float64 `json:"total"`
	Remaining float64 `json:"remaining"`
	ResetTime string  `json:"reset_time,omitempty"`
}
