// Package provider defines the contract every upstream gateway implements to
// plug into the unified work2api server, plus a global registry.
//
// The design goal: each provider contributes only its *upstream-specific*
// logic (credential shape, request signing / identity headers, model
// discovery, optional check-in). Everything shared — protocol conversion, the
// account pool, persistence, application-key auth, scheduling — lives in
// internal/core and is reused across providers.
package provider

import (
	"context"
	"errors"
	"sort"
	"sync"
)

// ErrUnsupported is returned by optional capabilities a provider does not have
// (e.g. Checkin for a PAT-only account).
var ErrUnsupported = errors.New("operation not supported by provider")

// Account is an upstream credential owned by a provider. Providers store their
// own extra fields in Extra (persisted as JSON).
type Account struct {
	UID      string         // stable unique id, namespaced by provider
	Provider string         // provider name
	Label    string         // human label
	Enabled  bool           // operator toggle
	Site     string         // provider-specific site/region hint (e.g. cn-cli, zen)
	Extra    map[string]any // provider-specific fields
}

// Model is a discovered upstream model, namespaced for non-default providers.
type Model struct {
	ID         string   // exposed id, e.g. "opencode/grok-code" or "hy4-preview"
	Provider   string   // owning provider
	Vision     bool     // accepts image input
	Modalities []string // OpenAI-style modalities
	MaxOutput  int      // max output tokens, 0 if unknown
	Context    int      // context window, 0 if unknown
}

// Credit is a provider's quota snapshot for an account.
type Credit struct {
	Total     float64
	Remaining float64
	Unit      string // e.g. "credits", "usd"; empty if unknown
	Unknown   bool   // provider could not determine quota
}

// ChatRequest is the normalized (OpenAI Chat shaped) request handed to a
// provider after protocol conversion. Body is the raw upstream-bound JSON map.
type ChatRequest struct {
	Model  string
	Stream bool
	Body   map[string]any
}

// UpstreamRequest is what a provider produces: the concrete HTTP call to make.
type UpstreamRequest struct {
	Method string
	URL    string
	Header map[string]string
	Body   []byte
}

// Provider is the contract each upstream gateway implements.
type Provider interface {
	// Name is the stable registry key ("workbuddy" | "qoder" | "opencode").
	Name() string
	// ListModels returns the models routable through this provider's accounts.
	ListModels(ctx context.Context) ([]Model, error)
	// Prepare turns a normalized request + chosen account into a concrete
	// upstream HTTP call (signing, identity headers, upstream URL selection).
	Prepare(ctx context.Context, acc Account, req *ChatRequest) (*UpstreamRequest, error)
	// Checkin performs the provider's daily credit claim. Returns
	// ErrUnsupported when the account/provider cannot check in.
	Checkin(ctx context.Context, acc Account) error
	// RefreshCredit fetches the account's current quota.
	RefreshCredit(ctx context.Context, acc Account) (Credit, error)
}

// registry holds registered providers by name.
var (
	regMu    sync.RWMutex
	registry = map[string]Provider{}
)

// Register adds a provider to the global registry. Panics on duplicate name,
// since registration happens at startup and a dup is a programming error.
func Register(p Provider) {
	regMu.Lock()
	defer regMu.Unlock()
	if _, dup := registry[p.Name()]; dup {
		panic("provider already registered: " + p.Name())
	}
	registry[p.Name()] = p
}

// Get returns a registered provider by name.
func Get(name string) (Provider, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	p, ok := registry[name]
	return p, ok
}

// All returns registered providers sorted by name for stable iteration.
func All() []Provider {
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]Provider, 0, len(registry))
	for _, p := range registry {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}
