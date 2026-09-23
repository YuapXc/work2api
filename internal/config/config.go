// Package config loads runtime configuration from flags, environment
// variables, and an optional .env file (in that precedence: flag > env > .env).
//
// It intentionally mirrors the knobs that the three upstream gateways exposed
// so operators migrating from any of them find familiar names.
package config

import (
	"bufio"
	"flag"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Config is the fully-resolved runtime configuration.
type Config struct {
	Host       string // listen host
	Port       int    // listen port
	DBPath     string // SQLite file path
	AdminToken string // admin/WebUI auth; empty => loopback-only
	DataDir    string // base dir for data (db, attachments, secrets)
	LogLevel   string
	LogToFile  bool

	// workbuddy provider knobs (ported from workbuddy_one/config.py)
	AllowExternalHost  bool
	Desensitize        bool
	Ratelimit          bool
	RatelimitInterval  float64
	CheckinHours       []int
	CreditRefreshMin   int
	ModelRefreshHour   int
	KeepaliveHour      int
	UsageRetentionDays int
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

	dd := resolvePath(*dataDir)
	dbp := *dbPath
	if dbp == "" {
		dbp = filepath.Join(dd, "work2api.db")
	} else {
		dbp = resolvePath(dbp)
	}

	return &Config{
		Host:       *host,
		Port:       *port,
		DataDir:    dd,
		DBPath:     dbp,
		AdminToken: *adminToken,
		LogLevel:   strings.ToUpper(*logLevel),
		LogToFile:  env("LOG_TO_FILE", "1") != "0",

		AllowExternalHost:  boolEnv("ALLOW_EXTERNAL_HOST", false),
		Desensitize:        boolEnv("DESENSITIZE", true),
		Ratelimit:          boolEnv("RATELIMIT", true),
		RatelimitInterval:  floatEnv("RATELIMIT_INTERVAL", 1.5),
		CheckinHours:       intListEnv("CHECKIN_HOURS", []int{9, 21}),
		CreditRefreshMin:   envInt("CREDIT_REFRESH_MIN", 30),
		ModelRefreshHour:   envInt("MODEL_REFRESH_HOUR", 6),
		KeepaliveHour:      envInt("KEEPALIVE_HOUR", 22),
		UsageRetentionDays: envInt("USAGE_RETENTION_DAYS", 90),
	}
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
		if n, err := strconv.Atoi(strings.TrimSpace(p)); err == nil {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
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

func envInt(k string, def int) int {
	if v, ok := os.LookupEnv(k); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
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
