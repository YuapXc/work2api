// Package qoder is a faithful in-process port of the standalone qoder2api Go
// gateway, exposed as a provider.Runtime for the work2api unified server.
//
// The cosy crypto (internal/qoder/cosy) is copied verbatim from upstream and the
// protocol bridge (internal/qoder/bridge) keeps the OpenAI-Chat / Anthropic-
// Messages / Codex-Responses <-> qoder-SSE conversion identical. Accounts are
// loaded from the qoder2api data dir (QODER2API_HOME, default ~/.qoder2api) so a
// user already running qoder2api needs no re-import.
package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	_ "embed"

	"work2api/internal/core/provider"
	"work2api/internal/qoder/account"
	"work2api/internal/qoder/bridge"
	"work2api/internal/qoder/cosy"
	"work2api/internal/qoder/localcred"
	"work2api/internal/qoder/logger"
)

//go:embed baseprompt.json
var basePromptJSON []byte

const (
	runtimeName   = "qoder"
	runtimePrefix = "qoder/"
)

// fallbackModelKeys mirrors qoder2api's static list used when the signed
// model/list call is unavailable (see bridge.HandleListModels).
var fallbackModelKeys = []string{
	"auto", "qmodel_38max", "qfmodel", "qmodel_latest", "qmodel", "q37fmodel",
	"dmodel", "dfmodel", "gmodel", "gfmodel", "gm51model", "kmodel_latest",
	"kmodel", "mmodel",
}

const fallbackContextWindow = 180000

// Runtime implements provider.Runtime for the qoder upstream. Bridges are
// expensive to build (session bootstrap + jobToken exchange), so one is cached
// per account id.
type Runtime struct {
	mu      sync.Mutex
	bridges map[string]*bridge.Bridge
	saltSet bool

	localOnce  sync.Once
	localCreds []localcred.Credential

	oauthMu     sync.Mutex
	oauthStates map[string]*oauthState

	quotaMu       sync.Mutex
	quotaCache    map[string]quotaEntry
	quotaInflight map[string]bool
}

// quotaEntry caches a per-account quota snapshot with a fetch timestamp so the
// admin views can show 额度 without a live upstream call on every page load.
type quotaEntry struct {
	remaining float64
	total     float64
	plan      string
	exceeded  bool
	expiresAt int64
	ts        time.Time
}

// New constructs the qoder runtime. It applies the qoder2api install salt so
// device fingerprints match the account's existing qoder2api deployment, and
// aligns the log level with qoder2api settings. When the qoder2api data dir does
// not exist (user does not use qoder) it stays dormant and creates nothing on
// disk — the salt/settings are only touched once a real account is present.
func New() *Runtime {
	if dataDirExists() {
		if salt, err := account.EnsureMachineSalt(); err == nil {
			cosy.SetInstallSalt(salt)
		} else {
			logger.Error("qoder: ensure machine salt: %v", err)
		}
		if st, err := account.LoadSettings(); err == nil && st != nil {
			logger.SetLevel(st.LogLevel)
		}
	}
	return &Runtime{
		bridges:       map[string]*bridge.Bridge{},
		oauthStates:   map[string]*oauthState{},
		quotaCache:    map[string]quotaEntry{},
		quotaInflight: map[string]bool{},
	}
}

// dataDirExists reports whether the qoder2api data dir is present WITHOUT
// creating it (account.List/EnsureMachineSalt would MkdirAll otherwise, which
// would litter ~/.qoder2api on every work2api startup for non-qoder users).
func dataDirExists() bool {
	info, err := os.Stat(account.DataRoot())
	return err == nil && info.IsDir()
}

func (r *Runtime) Name() string   { return runtimeName }
func (r *Runtime) Prefix() string { return runtimePrefix }

// detectLocal probes the local Qoder desktop safeStorage vault once and caches
// the result. This lets a user with Qoder installed use the gateway without a
// manual OAuth: the desktop device token (dt-/drt-) is DPAPI-recoverable.
func (r *Runtime) detectLocal() []localcred.Credential {
	r.localOnce.Do(func() {
		creds, err := localcred.Detect()
		if err != nil {
			logger.Info("qoder: 本地凭据探测未命中: %v", err)
			return
		}
		r.localCreds = creds
		if len(creds) > 0 {
			logger.Info("qoder: 探测到 %d 个本地 Qoder 桌面凭据", len(creds))
		}
	})
	return r.localCreds
}

// Ready reports whether at least one usable credential exists: a native
// ~/.qoder2api account with a secret, or a recoverable local Qoder desktop login.
func (r *Runtime) Ready() bool {
	if dataDirExists() {
		if accounts, err := account.List(); err == nil {
			for i := range accounts {
				if account.HasSecret(accounts[i].ID) {
					return true
				}
			}
		}
	}
	return len(r.detectLocal()) > 0
}

// pickAccount returns the account to serve plus its secret. It prefers a native
// ~/.qoder2api account (active first, else first with a secret), then falls back
// to a recovered local Qoder desktop credential.
func (r *Runtime) pickAccount() (*account.Account, string, error) {
	if dataDirExists() {
		if acct, _ := account.GetActive(); acct != nil && account.HasSecret(acct.ID) {
			if sec, err := account.GetSecret(acct.ID); err == nil {
				return acct, sec, nil
			}
		}
		if accounts, err := account.List(); err == nil {
			for i := range accounts {
				if account.HasSecret(accounts[i].ID) {
					if sec, err := account.GetSecret(accounts[i].ID); err == nil {
						return &accounts[i], sec, nil
					}
				}
			}
		}
	}
	if creds := r.detectLocal(); len(creds) > 0 {
		c := creds[0]
		acct := &account.Account{
			ID:     "qoder-local-" + c.Region,
			Name:   "本地 Qoder（" + c.Region + "）",
			Region: account.NormalizeRegion(c.Region),
		}
		return acct, c.DeviceToken, nil
	}
	return nil, "", fmt.Errorf("无可用 qoder 凭据（~/.qoder2api 无账号，且未探测到本地 Qoder 桌面登录）")
}

// bridgeFor builds (or returns the cached) bridge for an account. The base
// prompt template is rendered per-bridge exactly like qoder2api's
// startBridgeWithAccount (fresh UUIDs + timestamp spliced into the template).
func (r *Runtime) bridgeFor(acct *account.Account, secret string) (*bridge.Bridge, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Ensure the install salt is applied before any fingerprint is derived. Done
	// lazily (not in New) so a data dir created after startup is still honored.
	if !r.saltSet {
		if salt, err := account.EnsureMachineSalt(); err == nil {
			cosy.SetInstallSalt(salt)
		}
		r.saltSet = true
	}
	if b, ok := r.bridges[acct.ID]; ok && b != nil {
		return b, nil
	}
	if secret == "" {
		return nil, fmt.Errorf("empty qoder secret for %s", acct.ID)
	}
	tmpl := string(basePromptJSON)
	for _, ukey := range []string{"{UUID1}", "{UUID2}", "{UUID3}", "{UUID4}", "{UUID5}"} {
		tmpl = strings.ReplaceAll(tmpl, ukey, cosy.NewUUID())
	}
	tmpl = strings.ReplaceAll(tmpl, "{TIME1}", fmt.Sprintf("%d", cosy.UnixMs()))
	var templateBase map[string]interface{}
	_ = json.Unmarshal([]byte(tmpl), &templateBase)

	b, err := bridge.NewBridge(secret, acct.Region, templateBase)
	if err != nil {
		return nil, fmt.Errorf("create bridge: %w", err)
	}
	r.bridges[acct.ID] = b
	return b, nil
}

// Models returns the namespaced catalog. It queries the signed model/list
// endpoint for the active account and falls back to the static key list.
func (r *Runtime) Models(ctx context.Context) []provider.CatalogModel {
	acct, secret, err := r.pickAccount()
	if err != nil {
		return r.fallbackModels()
	}
	b, err := r.bridgeFor(acct, secret)
	if err != nil {
		logger.Error("qoder: models bridge build failed: %v", err)
		return r.fallbackModels()
	}
	models, err := b.ListAvailableModels()
	if err != nil || len(models) == 0 {
		logger.Error("qoder: ListAvailableModels failed (%v), using fallback", err)
		return r.fallbackModels()
	}
	out := make([]provider.CatalogModel, 0, len(models))
	for _, m := range models {
		ctxWin := m.ContextWindow
		if ctxWin == 0 {
			ctxWin = m.MaxInputTokens
		}
		if ctxWin == 0 {
			ctxWin = fallbackContextWindow
		}
		out = append(out, provider.CatalogModel{
			ID:        runtimePrefix + m.Key,
			Name:      m.DisplayName,
			MaxOutput: m.MaxOutputTokens,
			Context:   ctxWin,
			Extra: map[string]any{
				"enable":       m.Enable,
				"is_default":   m.IsDefault,
				"is_reasoning": m.IsReasoning,
				"price_factor": m.PriceFactor,
			},
		})
	}
	return out
}

func (r *Runtime) fallbackModels() []provider.CatalogModel {
	out := make([]provider.CatalogModel, 0, len(fallbackModelKeys))
	for _, key := range fallbackModelKeys {
		out = append(out, provider.CatalogModel{
			ID:      runtimePrefix + key,
			Name:    key,
			Context: fallbackContextWindow,
		})
	}
	return out
}

// Serve handles one inference request end-to-end, writing the client response
// to req.Writer via the ported bridge handlers.
func (r *Runtime) Serve(ctx context.Context, req provider.ServeRequest) (provider.UsageReport, error) {
	report := provider.UsageReport{Status: "ok"}

	acct, secret, err := r.pickAccount()
	if err != nil {
		writeServeError(req, err)
		report.Status = "error"
		report.Error = err.Error()
		return report, err
	}
	report.AccountUID = acct.ID

	b, err := r.bridgeFor(acct, secret)
	if err != nil {
		writeServeError(req, err)
		report.Status = "error"
		report.Error = err.Error()
		return report, err
	}

	// Strip the "qoder/" namespace so the bridge sees the bare upstream key.
	if m, ok := req.Payload["model"].(string); ok {
		req.Payload["model"] = strings.TrimPrefix(m, runtimePrefix)
	}

	var res bridge.ServeResult
	switch req.Protocol {
	case provider.ProtocolChat:
		res, err = b.ServeChat(ctx, req.Writer, req.Payload)
	case provider.ProtocolAnthropic:
		res, err = b.ServeClaude(ctx, req.Writer, req.Payload)
	case provider.ProtocolResponses:
		res, err = b.ServeCodex(ctx, req.Writer, req.Payload)
	default:
		err = fmt.Errorf("unsupported protocol %q", req.Protocol)
		writeServeError(req, err)
	}

	report.InputTokens = res.InputTokens
	report.OutputTokens = res.OutputTokens
	report.Output = res.Output
	report.Reasoning = res.Reasoning
	if err != nil {
		report.Status = "error"
		report.Error = err.Error()
		return report, err
	}
	return report, nil
}

// writeServeError emits a minimal OpenAI-style JSON error when the request
// cannot even reach the bridge handlers (which otherwise write their own errors).
func writeServeError(req provider.ServeRequest, err error) {
	if req.Writer == nil {
		return
	}
	req.Writer.Header().Set("Content-Type", "application/json")
	req.Writer.WriteHeader(http.StatusServiceUnavailable)
	body, _ := json.Marshal(map[string]interface{}{
		"error": map[string]interface{}{"message": err.Error(), "type": "qoder_error"},
	})
	_, _ = req.Writer.Write(body)
}

var _ provider.Runtime = (*Runtime)(nil)
