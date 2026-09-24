package opencode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"work2api/internal/core/provider"
)

// saveMu serializes config saves so two concurrent WebUI writes can't both
// build a runtime and race on the registry swap (which would leak the
// overwritten runtime's background goroutines).
var saveMu sync.Mutex

// ConfigDoc returns the editable OpenCode config for the WebUI. It works even
// when the runtime is currently inert (no file yet): it falls back to the
// on-disk file, then to defaults, so the operator can add the first key.
func (rt *Runtime) ConfigDoc() (map[string]any, error) {
	cfg := rt.baseConfig()
	active := map[string]any{"zen": 0, "go": 0}
	if rt.ready {
		active["zen"] = rt.zenNodes.Len()
		active["go"] = rt.goNodes.Len()
	}
	return map[string]any{
		"config_path": rt.path,
		"ready":       rt.ready,
		"zen_keys":    cfg.ZenKeys,
		"go_keys":     cfg.GoKeys,
		"anonymous":   cfg.Anonymous,
		"prefer":      string(cfg.Prefer),
		"active":      active,
	}, nil
}

// SaveConfigDoc merges the patch (zen_keys/go_keys/anonymous/prefer) into the
// current config, validates it, writes opencode.json, then hot-reloads the
// runtime by swapping the registry entry and stopping this instance's loops.
func (rt *Runtime) SaveConfigDoc(patch map[string]any) error {
	saveMu.Lock()
	defer saveMu.Unlock()

	cfg := rt.baseConfig()
	if v, ok := patch["zen_keys"]; ok {
		cfg.ZenKeys = toStringSlice(v)
	}
	if v, ok := patch["go_keys"]; ok {
		cfg.GoKeys = toStringSlice(v)
	}
	if v, ok := patch["anonymous"].(bool); ok {
		cfg.Anonymous = v
	}
	if v, ok := patch["prefer"].(string); ok && v != "" {
		cfg.Prefer = Tier(v)
	}

	validated, err := Normalize(rt.path, cfg)
	if err != nil {
		return err
	}
	if err := writeConfigFile(rt.path, validated); err != nil {
		return fmt.Errorf("写入配置失败：%w", err)
	}

	newRt, err := newConfigured(validated, rt.path, rt.logger)
	if err != nil {
		return fmt.Errorf("重载失败：%w", err)
	}
	// Inherit the live catalog so the reloaded runtime routes correctly in the
	// window before its first async refresh (avoids the empty-catalog fallback
	// that would send Responses/Anthropic models as Chat).
	if rt.ready && rt.catalog != nil {
		newRt.catalog.CopyState(rt.catalog)
	}
	if rt.cancel != nil {
		rt.cancel() // stop the old instance's background loops
	}
	provider.ReplaceRuntime(newRt)
	return nil
}

// baseConfig picks the best starting point: the live config when ready, else
// the on-disk file if it parses, else opencode2api defaults.
func (rt *Runtime) baseConfig() Config {
	if rt.ready {
		return rt.cfg
	}
	if rt.path != "" {
		if cfg, err := LoadConfig(rt.path); err == nil {
			return cfg
		}
	}
	return defaultConfig()
}

func writeConfigFile(path string, cfg Config) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func toStringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}
