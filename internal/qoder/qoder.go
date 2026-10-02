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
	"crypto/sha256"
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
	mu               sync.Mutex
	bridges          map[string]*bridge.Bridge
	bridgeKeys       map[string][32]byte
	bridgeGeneration map[string]uint64
	bridgeInflight   map[string]*bridgeInitialization
	modelCache       []provider.CatalogModel
	saltSet          bool

	localOnce  sync.Once
	localCreds []localcred.Credential

	oauthMu     sync.Mutex
	oauthStates map[string]*oauthState

	quotaMu       sync.Mutex
	quotaCache    map[string]quotaEntry
	quotaInflight map[string]bool
}

type bridgeInitialization struct {
	done   chan struct{}
	key    [32]byte
	bridge *bridge.Bridge
	err    error
}

// quotaEntry caches a per-account quota snapshot with a fetch timestamp so the
// admin views can show 额度 without a live upstream call on every page load.
type quotaEntry struct {
	remaining float64
	total     float64
	plan      string
	exceeded  bool
	expiresAt int64
	// packages 活动赠送的专属资源包明细（与 workbuddy credit_packages 同构，
	// WebUI 账号表可直接复用积分构成弹层）。
	packages []map[string]any
	ts       time.Time
}

// New constructs the qoder runtime. It applies the qoder2api install salt so
// device fingerprints match the account's existing qoder2api deployment. When
// the qoder2api data dir does not exist (user does not use qoder) it stays
// dormant and creates nothing on disk — the salt/settings are only touched once
// a real account is present.
//
// logLevel is the gateway-wide LOG_LEVEL ("debug"/"info"/"error"); when non-empty
// it drives the qoder logger and takes precedence over the qoder store's own
// setting, so a single LOG_LEVEL in .env controls console verbosity everywhere.
func New(logLevel string) *Runtime {
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
	// Gateway LOG_LEVEL wins over the qoder store fallback set above.
	logger.SetLevel(strings.ToLower(strings.TrimSpace(logLevel)))
	return &Runtime{
		bridges:          map[string]*bridge.Bridge{},
		bridgeKeys:       map[string][32]byte{},
		bridgeGeneration: map[string]uint64{},
		bridgeInflight:   map[string]*bridgeInitialization{},
		oauthStates:      map[string]*oauthState{},
		quotaCache:       map[string]quotaEntry{},
		quotaInflight:    map[string]bool{},
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
	var visible []localcred.Credential
	for _, c := range r.localCreds {
		if !account.IsGatewayHidden("qoder-local-" + c.Region) {
			visible = append(visible, c)
		}
	}
	return visible
}

// Ready reports whether at least one usable credential exists: a native
// ~/.qoder2api account with a secret, or a recoverable local Qoder desktop login.
func (r *Runtime) Ready() bool {
	if dataDirExists() {
		if accounts, err := account.List(); err == nil {
			for i := range accounts {
				if !account.IsGatewayHidden(accounts[i].ID) && account.HasSecret(accounts[i].ID) {
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
		if acct, _ := account.GetActive(); acct != nil && !account.IsGatewayHidden(acct.ID) && account.HasSecret(acct.ID) {
			if sec, err := account.GetSecret(acct.ID); err == nil {
				return acct, sec, nil
			}
		}
		if accounts, err := account.List(); err == nil {
			for i := range accounts {
				if !account.IsGatewayHidden(accounts[i].ID) && account.HasSecret(accounts[i].ID) {
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
func (r *Runtime) bridgeFor(ctx context.Context, acct *account.Account, secret string) (*bridge.Bridge, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	key := sha256.Sum256([]byte(string(acct.Region) + "\x00" + secret))
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		r.mu.Lock()
		// Ensure the install salt is applied before any fingerprint is derived. Done
		// lazily (not in New) so a data dir created after startup is still honored.
		if !r.saltSet {
			if salt, err := account.EnsureMachineSalt(); err == nil {
				cosy.SetInstallSalt(salt)
			}
			r.saltSet = true
		}
		if b, ok := r.bridges[acct.ID]; ok && b != nil {
			if r.bridgeKeys[acct.ID] == key {
				r.mu.Unlock()
				return b, nil
			}
		}
		if secret == "" {
			r.mu.Unlock()
			return nil, fmt.Errorf("empty qoder secret for %s", acct.ID)
		}
		if pending, ok := r.bridgeInflight[acct.ID]; ok {
			r.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-pending.done:
				if pending.key == key {
					return pending.bridge, pending.err
				}
				continue
			}
		}
		pending := &bridgeInitialization{done: make(chan struct{}), key: key}
		r.bridgeInflight[acct.ID] = pending
		generation := r.bridgeGeneration[acct.ID]
		r.mu.Unlock()
		tmpl := string(basePromptJSON)
		for _, ukey := range []string{"{UUID1}", "{UUID2}", "{UUID3}", "{UUID4}", "{UUID5}"} {
			tmpl = strings.ReplaceAll(tmpl, ukey, cosy.NewUUID())
		}
		tmpl = strings.ReplaceAll(tmpl, "{TIME1}", fmt.Sprintf("%d", cosy.UnixMs()))
		var templateBase map[string]interface{}
		_ = json.Unmarshal([]byte(tmpl), &templateBase)

		b, err := bridge.NewBridgeContext(ctx, secret, acct.Region, templateBase)
		// Re-read native credentials before publishing; external imports may change
		// the secret while the network initialization is in progress.
		if err == nil && !strings.HasPrefix(acct.ID, "qoder-local-") {
			current, readErr := account.GetSecret(acct.ID)
			if readErr != nil || current != secret {
				err = fmt.Errorf("qoder credentials changed during initialization")
			}
		}
		r.mu.Lock()
		if r.bridgeGeneration[acct.ID] != generation || account.IsGatewayHidden(acct.ID) {
			err = fmt.Errorf("qoder account removed or changed during initialization")
		}
		if err == nil {
			r.bridges[acct.ID] = b
			r.bridgeKeys[acct.ID] = key
		}
		delete(r.bridgeInflight, acct.ID)
		pending.bridge = b
		if err != nil {
			pending.bridge = nil
			pending.err = fmt.Errorf("create bridge: %w", err)
		}
		close(pending.done)
		r.mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("create bridge: %w", err)
		}
		return b, nil
	}
}

// Models returns the namespaced catalog. It queries the signed model/list
// endpoint for the active account and falls back to the static key list.
func (r *Runtime) Models(ctx context.Context) []provider.CatalogModel {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.modelCache) > 0 {
		return append([]provider.CatalogModel(nil), r.modelCache...)
	}
	return r.fallbackModels()
}

// RefreshModels performs network discovery only on explicit/scheduled refresh.
// Ordinary catalog and health reads use Models' cached/fallback snapshot.
func (r *Runtime) RefreshModels(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	acct, secret, err := r.pickAccount()
	if err != nil {
		return err
	}
	b, err := r.bridgeFor(ctx, acct, secret)
	if err != nil {
		logger.Error("qoder: models bridge build failed: %v", err)
		return err
	}
	models, err := b.ListAvailableModelsContext(ctx)
	if err != nil || len(models) == 0 {
		logger.Error("qoder: ListAvailableModels failed (%v), using fallback", err)
		return fmt.Errorf("qoder model discovery failed: %v", err)
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
			ID:         runtimePrefix + m.Key,
			Name:       m.DisplayName,
			Vision:     m.IsVL,
			Modalities: modelModalities(m.IsVL),
			MaxOutput:  m.MaxOutputTokens,
			Context:    ctxWin,
			Extra: map[string]any{
				"enable":       m.Enable,
				"is_default":   m.IsDefault,
				"is_reasoning": m.IsReasoning,
				"price_factor": m.PriceFactor,
				// 统一目录暴露标准模态字段：WebUI 的 ModelCard 按
				// modality/vision/supports_image 判定多模态标签，缺省即显示"文本"。
				"modality":       modalityOf(m.IsVL),
				"supportsImages": m.IsVL,
			},
		})
	}
	r.mu.Lock()
	r.modelCache = out
	r.mu.Unlock()
	return nil
}

// modalityOf / modelModalities 把上游 is_vl 布尔翻译成统一目录的标准模态字段，
// 语义与 workbuddy 目录（models.go extractCaps/withStandardFields）一致：
// multimodal = 文本 + 图片输入，输出恒为文本。
func modalityOf(isVL bool) string {
	if isVL {
		return "multimodal"
	}
	return "text"
}

func modelModalities(isVL bool) []string {
	if isVL {
		return []string{"text", "image"}
	}
	return []string{"text"}
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

	b, err := r.bridgeFor(ctx, acct, secret)
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
	report.TokensKnown = &res.UsageKnown
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
