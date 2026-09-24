// Package opencode ports the opencode2api Go gateway ("to-API" logic) into the
// work2api monolith as a provider.Runtime. It owns its own opencode-style
// config, transport/node/anonymous pools, session-affinity routing, identity
// derivation, and the two-source model catalog, and reuses work2api's shared
// core/protocol package for all wire conversion.
package opencode

import (
	"time"
)

// Tier names the two opencode upstream lanes.
type Tier string

const (
	TierZen Tier = "zen"
	TierGo  Tier = "go"
)

// Config mirrors opencode2api's config schema. ServerKeys is retained for
// JSON compatibility but is unused here: client auth is handled by work2api's
// app-key layer, not by this runtime.
type Config struct {
	Listen      string            `json:"listen"`
	ServerKeys  []string          `json:"server_keys"`
	ZenKeys     []string          `json:"zen_keys"`
	GoKeys      []string          `json:"go_keys"`
	Anonymous   bool              `json:"anonymous"`
	Proxies     []string          `json:"proxies"`
	ProxyFile   string            `json:"proxyfile"`
	Upstream    UpstreamConfig    `json:"upstream"`
	Retry       RetryConfig       `json:"retry"`
	Models      ModelsConfig      `json:"models"`
	Performance PerformanceConfig `json:"performance"`
	Logging     LoggingConfig     `json:"logging"`
	WebUI       WebUIConfig       `json:"webui"`
	Prefer      Tier              `json:"prefer"`
	Reasoning   ReasoningConfig   `json:"reasoning"`

	effectiveProxies []string
}

type ReasoningConfig struct {
	Effort        string            `json:"effort,omitempty"`
	EffortByModel map[string]string `json:"effort_by_model,omitempty"`
}

type UpstreamConfig struct {
	Zen string `json:"zen"`
	Go  string `json:"go"`
}

type RetryConfig struct {
	MaxAttempts    int `json:"max_attempts"`
	TimeoutSeconds int `json:"timeout_seconds"`
}

type ModelsConfig struct {
	RefreshSeconds int               `json:"refresh_seconds"`
	Protocols      map[string]string `json:"protocols"`
}

type LoggingConfig struct {
	Level             string `json:"level"`
	RingSize          int    `json:"ring_size"`
	DumpRequestBodies bool   `json:"dump_request_bodies"`
}

type WebUIConfig struct {
	Enabled           bool   `json:"enabled"`
	Listen            string `json:"listen"`
	Username          string `json:"username"`
	Password          string `json:"password,omitempty"`
	PasswordHash      string `json:"password_hash,omitempty"`
	SessionTTLMinutes int    `json:"session_ttl_minutes"`
}

type PerformanceConfig struct {
	MaxIdleConns           int `json:"max_idle_conns"`
	MaxIdleConnsPerHost    int `json:"max_idle_conns_per_host"`
	MaxConnsPerHost        int `json:"max_conns_per_host"`
	IdleConnTimeoutSeconds int `json:"idle_conn_timeout_seconds"`
	ConnectTimeoutSeconds  int `json:"connect_timeout_seconds"`
	FailureCooldownSeconds int `json:"failure_cooldown_seconds"`
	AttemptTimeoutSeconds  int `json:"attempt_timeout_seconds"`
}

// AttemptTimeout bounds a single upstream attempt's header wait; see upstream.
func (cfg PerformanceConfig) AttemptTimeout(requestTimeout time.Duration) time.Duration {
	if cfg.AttemptTimeoutSeconds > 0 {
		attempt := time.Duration(cfg.AttemptTimeoutSeconds) * time.Second
		if requestTimeout > 0 && attempt > requestTimeout {
			return requestTimeout
		}
		return attempt
	}
	return requestTimeout
}
