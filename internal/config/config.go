// Package config loads runtime configuration from flags, environment
// variables, and an optional .env file (in that precedence: flag > env > .env).
//
// It intentionally mirrors the knobs that the three upstream gateways exposed
// so operators migrating from any of them find familiar names.
package config

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	Host                  string // listen host
	Port                  int    // listen port
	DBPath                string // SQLite file path
	AdminToken            string // admin/WebUI auth; empty => loopback-only
	AdminCookieSecure     bool   // force Secure cookies behind a TLS-terminating proxy
	TrustedProxyCIDRs     string // explicit peers permitted to supply overwritten X-Real-IP
	MaxRequestBytes       int64
	MaxConcurrentRequests int
	ModelQueueSize        int
	ModelQueueWaitSeconds int
	ModelKeyQueueSize     int
	ModelKeyLimits        string
	AdminConcurrency      int
	HeavyAdminConcurrency int
	QueryConcurrency      int
	BodyReadConcurrency   int
	RequestBodyBudget     int64
	MaxResponseBytes      int64
	MaxJSONItems          int
	MaxJSONDepth          int
	DataDir               string // base dir for data (db, attachments, secrets)
	LogLevel              string
	LogToFile             bool

	// 用户门户（HANDOFF §8 方案）：注册模式 invite|open|closed；贡献扫码任务
	// 上限（防批量任务耗尽内存）；用户级并发与每日用量（0 = 不启用该限制，
	// 但共享调用要求先配置模型范围）。
	PortalRegistrationMode  string
	PortalMaxTasksPerUser   int
	PortalUserConcurrency   int
	PortalSharedConcurrency int
	PortalDailyRequests     int
	PortalDailyInputTokens  int
	PortalDailyOutputTokens int
	PortalKeyMaxPerUser     int
	PortalEnabled           bool

	// workbuddy provider knobs (ported from workbuddy_one/config.py)
	AllowExternalHost  bool
	Desensitize        bool
	OptimizeContext    bool
	Ratelimit          bool
	RatelimitInterval  float64
	CheckinHours       []int
	CreditRefreshMin   int
	ModelRefreshHour   int
	KeepaliveHour      int
	UsageRetentionDays int
	// UsageMaxRows caps total usage_logs rows (0 = unlimited). Guards against a
	// burst of high-frequency calls bloating the DB within the retention window.
	UsageMaxRows int
	// UsageCheckIntervalMin is how often the row-count guard runs (cheap MIN/MAX
	// id precheck; only trims when actually over the cap).
	UsageCheckIntervalMin int
	// UsageContentMaxBytes truncates each stored input/output/reasoning content
	// field at write (0 = unlimited). The real DB-size driver is base64 image
	// payloads in request/response bodies; this bounds per-row bytes.
	UsageContentMaxBytes int
}

// PackageRoot is the directory containing the executable's working tree root.
// Relative paths (DB, data) anchor here so data does not drift with the
// process CWD.
var PackageRoot string

func init() {
	if wd, err := os.Getwd(); err == nil {
		PackageRoot = wd
	}
}

// Load resolves configuration. flags override env, env overrides .env defaults.
func Load(args []string) *Config {
	loadDotEnv(filepath.Join(PackageRoot, ".env"))

	fs := flag.NewFlagSet("work2api", flag.ContinueOnError)
	host := fs.String("host", env("HOST", "127.0.0.1"), "listen host")
	port := fs.Int("port", envInt("PORT", 8787), "listen port")
	dataDir := fs.String("data-dir", env("DATA_DIR", "data"), "data directory")
	dbPath := fs.String("db", env("DB_PATH", ""), "sqlite db path (default <data-dir>/work2api.db)")
	adminToken := fs.String("admin-token", env("ADMIN_TOKEN", ""), "admin/webui token; empty => loopback only")
	logLevel := fs.String("log-level", env("LOG_LEVEL", "INFO"), "log level")
	_ = fs.Parse(args)

	if *port < 1 || *port > 65535 {
		warnf("PORT=%d 超出 1–65535 范围，已回退默认 8787", *port)
		*port = 8787
	}

	dd := resolvePath(*dataDir)
	dbp := *dbPath
	if dbp == "" {
		dbp = filepath.Join(dd, "work2api.db")
	} else {
		dbp = resolvePath(dbp)
	}

	return &Config{
		Host:                  *host,
		Port:                  *port,
		DataDir:               dd,
		DBPath:                dbp,
		AdminToken:            *adminToken,
		AdminCookieSecure:     boolEnv("ADMIN_COOKIE_SECURE", boolEnv("ALLOW_EXTERNAL_HOST", false)),
		TrustedProxyCIDRs:     envOr("TRUSTED_PROXY_CIDRS", ""),
		MaxRequestBytes:       int64(positiveEnvInt("MAX_REQUEST_BYTES", 16*1024*1024)),
		MaxConcurrentRequests: positiveEnvInt("MAX_CONCURRENT_REQUESTS", 4),
		ModelQueueSize:        positiveEnvInt("MODEL_QUEUE_SIZE", 8),
		ModelQueueWaitSeconds: positiveEnvInt("MODEL_QUEUE_WAIT_SECONDS", 20),
		ModelKeyQueueSize:     positiveEnvInt("MODEL_KEY_QUEUE_SIZE", 8),
		ModelKeyLimits:        env("MODEL_KEY_CONCURRENCY_LIMITS", ""),
		AdminConcurrency:      positiveEnvInt("ADMIN_CONCURRENCY", 8),
		HeavyAdminConcurrency: positiveEnvInt("HEAVY_ADMIN_CONCURRENCY", 2),
		QueryConcurrency:      positiveEnvInt("QUERY_CONCURRENCY", 4),
		BodyReadConcurrency:   positiveEnvInt("BODY_READ_CONCURRENCY", 4),
		RequestBodyBudget:     int64(positiveEnvInt("REQUEST_BODY_BUDGET", 32<<20)),
		MaxResponseBytes:      int64(positiveEnvInt("MAX_RESPONSE_BYTES", 8<<20)),
		MaxJSONItems:          positiveEnvInt("MAX_JSON_ITEMS", 100000),
		MaxJSONDepth:          positiveEnvInt("MAX_JSON_DEPTH", 128),
		LogLevel:              strings.ToUpper(*logLevel),
		LogToFile:             env("LOG_TO_FILE", "1") != "0",

		PortalRegistrationMode:  strings.ToLower(strings.TrimSpace(envOr("PORTAL_REGISTRATION_MODE", envOr("PORTAL_INVITE_MODE", "invite")))),
		PortalMaxTasksPerUser:   positiveEnvInt("PORTAL_MAX_TASKS_PER_USER", 3),
		PortalUserConcurrency:   positiveEnvInt("PORTAL_USER_CONCURRENCY", 1),
		PortalSharedConcurrency: positiveEnvInt("PORTAL_SHARED_CONCURRENCY", 3),
		PortalDailyRequests:     envInt("PORTAL_DAILY_REQUESTS", 0),
		PortalDailyInputTokens:  envInt("PORTAL_DAILY_INPUT_TOKENS", 0),
		PortalDailyOutputTokens: envInt("PORTAL_DAILY_OUTPUT_TOKENS", 0),
		PortalKeyMaxPerUser:     positiveEnvInt("PORTAL_KEY_MAX_PER_USER", 2),
		PortalEnabled:           boolEnv("PORTAL_ENABLED", true),

		AllowExternalHost:  boolEnv("ALLOW_EXTERNAL_HOST", false),
		Desensitize:        boolEnv("DESENSITIZE", true),
		OptimizeContext:    boolEnv("OPTIMIZE_CONTEXT", false),
		Ratelimit:          boolEnv("RATELIMIT", true),
		RatelimitInterval:  floatEnv("RATELIMIT_INTERVAL", 1.5),
		CheckinHours:       intListEnv("CHECKIN_HOURS", []int{9, 21}),
		CreditRefreshMin:   envInt("CREDIT_REFRESH_MIN", 30),
		ModelRefreshHour:   envInt("MODEL_REFRESH_HOUR", 6),
		KeepaliveHour:      envInt("KEEPALIVE_HOUR", 22),
		UsageRetentionDays: envInt("USAGE_RETENTION_DAYS", 90),
		// 行数上限默认 10 万条（0=不限）：多数个人用量到不了，却能兜住失控增长。
		UsageMaxRows:          envInt("USAGE_MAX_ROWS", 100000),
		UsageCheckIntervalMin: envInt("USAGE_CHECK_INTERVAL_MIN", 30),
		// 单条 content 字段上限默认 32KB（0=不限）：纯文本远够（约 8k tokens），
		// 主要截断 base64 图片这类撑爆库的大 payload。
		UsageContentMaxBytes: envInt("USAGE_CONTENT_MAX_BYTES", 32768),
	}
}

func positiveEnvInt(k string, def int) int {
	v := envInt(k, def)
	if v <= 0 {
		warnf("%s=%d 必须大于 0，已回退默认值 %d", k, v, def)
		return def
	}
	return v
}

func boolEnv(k string, def bool) bool {
	v, ok := os.LookupEnv(k)
	if !ok {
		return def
	}
	return v != "0" && !strings.EqualFold(v, "false")
}

func floatEnv(k string, def float64) float64 {
	if v, ok := os.LookupEnv(k); ok {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
		warnf("%s=%q 不是合法数字，已回退默认值 %g", k, v, def)
	}
	return def
}

func intListEnv(k string, def []int) []int {
	v, ok := os.LookupEnv(k)
	if !ok || strings.TrimSpace(v) == "" {
		return def
	}
	var out []int
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if n, err := strconv.Atoi(p); err == nil {
			out = append(out, n)
		} else {
			warnf("%s 中的 %q 不是合法整数，已忽略该项", k, p)
		}
	}
	if len(out) == 0 {
		warnf("%s=%q 无任何合法整数，已回退默认值 %v", k, v, def)
		return def
	}
	return out
}

func resolvePath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(PackageRoot, p)
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return def
}

// envOr is env but treats an empty configured value as unset (alias chains).
func envOr(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v, ok := os.LookupEnv(k); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
		warnf("%s=%q 不是合法整数，已回退默认值 %d", k, v, def)
	}
	return def
}

// warnf prints a configuration warning to stderr. Bad values are non-fatal:
// the caller falls back to the documented default, but the operator is told
// which key was ignored and why (satisfies “遇到错误的变量配置在命令中提示”).
func warnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "⚠ 配置警告："+format+"\n", args...)
}

// loadDotEnv is a minimal .env reader (no external dependency). Existing env
// vars are never overwritten.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if k != "" {
			if _, ok := os.LookupEnv(k); !ok {
				_ = os.Setenv(k, v)
			}
		}
	}
}
