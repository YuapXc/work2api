package provider

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// Registry belongs to one gateway instance. In-flight calls retain their
// runtime when configuration is replaced; other gateways are unaffected.
type Registry struct {
	mu       sync.RWMutex
	runtimes map[string]Runtime
}

func NewRegistry() *Registry { return &Registry{runtimes: map[string]Runtime{}} }

func (r *Registry) Register(rt Runtime) error {
	if rt == nil || rt.Name() == "" || (rt.Prefix() != "" && !strings.HasSuffix(rt.Prefix(), "/")) {
		return fmt.Errorf("invalid provider runtime")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runtimes == nil {
		r.runtimes = map[string]Runtime{}
	}
	if _, exists := r.runtimes[rt.Name()]; exists {
		return fmt.Errorf("provider already registered: %s", rt.Name())
	}
	for _, other := range r.runtimes {
		if rt.Prefix() == other.Prefix() || (rt.Prefix() != "" && other.Prefix() != "" && (strings.HasPrefix(rt.Prefix(), other.Prefix()) || strings.HasPrefix(other.Prefix(), rt.Prefix()))) {
			return fmt.Errorf("overlapping provider prefixes")
		}
	}
	r.runtimes[rt.Name()] = rt
	return nil
}

// Replace only accepts the runtime currently owned by this registry. A stale
// configuration editor cannot replace a newer runtime or another instance.
func (r *Registry) Replace(previous, next Runtime) error {
	if previous == nil || next == nil || previous.Name() != next.Name() || previous.Prefix() != next.Prefix() {
		return fmt.Errorf("provider replacement identity changed")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.runtimes[previous.Name()] != previous {
		return fmt.Errorf("provider configuration changed; reload before saving")
	}
	r.runtimes[next.Name()] = next
	return nil
}

func (r *Registry) ByName(name string) (Runtime, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	rt, ok := r.runtimes[name]
	return rt, ok
}

func (r *Registry) Runtimes() []Runtime {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Runtime, 0, len(r.runtimes))
	for _, rt := range r.runtimes {
		out = append(out, rt)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Prefix() == "") != (out[j].Prefix() == "") {
			return out[i].Prefix() == ""
		}
		return out[i].Name() < out[j].Name()
	})
	return out
}

func (r *Registry) ForModel(model string) (Runtime, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rt := range r.runtimes {
		if rt.Prefix() != "" && strings.HasPrefix(model, rt.Prefix()) {
			return rt, true
		}
	}
	return nil, false
}

// IsCurrent is used before a serialized configuration write, so stale editors
// cannot overwrite the persisted settings before Replace detects the conflict.
func (r *Registry) IsCurrent(rt Runtime) bool {
	if r == nil || rt == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.runtimes[rt.Name()] == rt
}

// Close stops runtime-owned background work after the gateway has drained.
func (r *Registry) Close() error {
	var failures []error
	for _, rt := range r.Runtimes() {
		if closer, ok := rt.(io.Closer); ok {
			failures = append(failures, closer.Close())
		}
	}
	return errors.Join(failures...)
}

// Resolve gives namespace matches priority, then uses the explicitly registered
// default. ForModel remains namespace-only for legacy catalog callers.
func (r *Registry) Resolve(model string) (Runtime, bool) {
	if rt, ok := r.ForModel(model); ok {
		return rt, true
	}
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rt := range r.runtimes {
		if rt.Prefix() == "" {
			return rt, true
		}
	}
	return nil, false
}
