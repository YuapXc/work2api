// This file extends the provider package with an inference-runtime contract.
//
// The original Provider interface above models a "prepare one HTTP call" seam.
// That fits workbuddy (which speaks an OpenAI-ish upstream directly) but not the
// two Go upstreams we port next:
//   - qoder always streams a custom SSE envelope and must splice the prompt into
//     a request template, so a single prepared request/response does not capture it;
//   - opencode routes per-tier (zen/go), applies session affinity + anonymous
//     shaping, and can use a different wire protocol per tier for the same model.
//
// So non-default providers implement Runtime instead: a self-contained inference
// core that owns its own accounts/signing/upstream and writes the client response
// directly. The unified server dispatches <namespace>/<model> requests to the
// matching Runtime and logs usage from the returned UsageReport. workbuddy keeps
// its existing in-app pipeline and is the default (un-namespaced) provider.
package provider

import (
	"context"
	"net/http"
)

// Protocol is the client-facing wire protocol of an inference request.
type Protocol string

const (
	ProtocolChat      Protocol = "chat"
	ProtocolAnthropic Protocol = "anthropic"
	ProtocolResponses Protocol = "responses"
)

// CatalogModel is one namespaced model a provider contributes to /v1/models.
// ID already includes the namespace prefix (e.g. "qoder/qmodel").
type CatalogModel struct {
	ID         string
	Name       string
	Vision     bool
	Modalities []string
	MaxOutput  int
	Context    int
	Extra      map[string]any // merged verbatim into the exposed catalog entry
}

// UsageReport is what Serve returns for the shared usage logger + pool bookkeeping.
type UsageReport struct {
	AccountUID   string
	InputTokens  int
	OutputTokens int
	Status       string // "ok" | "error"
	Error        string
	Input        string
	Output       string
	Reasoning    string
	Effort       string
	Credits      *float64
}

// ServeRequest carries a single inference call to a Runtime.
type ServeRequest struct {
	Protocol Protocol
	Payload  map[string]any // parsed client body; Payload["model"] is still namespaced
	Writer   http.ResponseWriter
	AppName  string
}

// Runtime is the inference contract each non-default provider implements so the
// unified server can route "<namespace>/<model>" requests to it. Implementations
// own their own credential source, signing, upstream and streaming; they convert
// to/from the client Protocol themselves (reusing core/protocol where useful).
type Runtime interface {
	// Name is the registry key ("qoder" | "opencode").
	Name() string
	// Prefix is the model namespace this runtime owns, incl. trailing slash
	// (e.g. "qoder/"). A model id starting with Prefix routes here.
	Prefix() string
	// Ready reports whether the runtime has at least one usable credential.
	Ready() bool
	// Models returns the runtime's namespaced catalog entries (may be empty).
	Models(ctx context.Context) []CatalogModel
	// Serve handles one request end-to-end, writing the client response to
	// req.Writer. The returned UsageReport is logged by the caller.
	Serve(ctx context.Context, req ServeRequest) (UsageReport, error)
}

// runtimes holds registered inference runtimes by name.
var runtimeRegistry = map[string]Runtime{}

// RegisterRuntime adds an inference runtime. Panics on duplicate (startup bug).
func RegisterRuntime(rt Runtime) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := runtimeRegistry[rt.Name()]; dup {
		panic("runtime already registered: " + rt.Name())
	}
	runtimeRegistry[rt.Name()] = rt
}

// Runtimes returns all registered runtimes (unordered snapshot).
func Runtimes() []Runtime {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]Runtime, 0, len(runtimeRegistry))
	for _, rt := range runtimeRegistry {
		out = append(out, rt)
	}
	return out
}

// RuntimeForModel returns the runtime owning a namespaced model id, if any.
func RuntimeForModel(model string) (Runtime, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	for _, rt := range runtimeRegistry {
		if p := rt.Prefix(); p != "" && len(model) >= len(p) && model[:len(p)] == p {
			return rt, true
		}
	}
	return nil, false
}

// RuntimeByName returns a registered runtime by its Name(), if any.
func RuntimeByName(name string) (Runtime, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	rt, ok := runtimeRegistry[name]
	return rt, ok
}

// AdminData is a provider's self-describing management snapshot. Each provider
// fills only what it has: workbuddy has a rich account pool + credits + checkin;
// qoder has one active/local account + region + checkin + quota; opencode has no
// accounts (config key tiers) and no checkin/credits. The WebUI renders columns
// and actions from Capabilities rather than assuming one uniform shape.
type AdminData struct {
	DisplayName  string           // e.g. "Qoder"
	Ready        bool             // has a usable credential/config
	Default      bool             // the un-namespaced default provider (workbuddy)
	Capabilities []string         // subset of: accounts, models, checkin, credits, oauth, upload, config, local_detect, add_account
	Accounts     []map[string]any // provider-shaped account rows (may be empty)
	Models       []map[string]any // provider-shaped model rows (namespaced ids)
	Status       map[string]any   // summary numbers for the overview card
	Notes        string           // guidance when not ready (e.g. how to configure)
}

// AdminRuntime is an optional capability: a Runtime that exposes management data.
type AdminRuntime interface {
	Runtime
	AdminData(ctx context.Context) AdminData
}

// Checkiner is an optional capability: a runtime that can perform a checkin.
type Checkiner interface {
	AdminCheckin(ctx context.Context) (map[string]any, error)
}

// CreditRefresher is an optional capability: a runtime that can refresh quota.
type CreditRefresher interface {
	AdminRefreshCredits(ctx context.Context) (map[string]any, error)
}

// OAuthRuntime is an optional capability: a runtime that can add an account via a
// browser device-authorization (scan) login. The WebUI renders OAuthOptions as
// form fields (e.g. qoder's region), calls OAuthBegin to get a login URL/QR, then
// polls OAuthPoll until status is "ready" (account saved) or "error".
type OAuthRuntime interface {
	// OAuthOptions returns selectable begin options, e.g.
	// [{"key":"region","label":"区域","values":[{"value":"cn","label":"国内"},...]}].
	OAuthOptions() []map[string]any
	// OAuthBegin starts a login; returns {login_id, login_url}.
	OAuthBegin(opts map[string]any) (map[string]any, error)
	// OAuthPoll reports {status: "pending"|"ready"|"error", message?, account?}.
	OAuthPoll(loginID string) (map[string]any, error)
}

// AccountManager is an optional capability: a runtime that supports per-account
// management actions (matching workbuddy's activate/rename/delete). Each returns
// an error the caller surfaces; unsupported actions may return an error.
type AccountManager interface {
	// ActivateAccount makes the given account the one used for requests.
	ActivateAccount(id string) error
	// RenameAccount sets a display name/alias for the account.
	RenameAccount(id, name string) error
	// DeleteAccount removes the account.
	DeleteAccount(id string) error
}
