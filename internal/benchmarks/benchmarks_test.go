package benchmarks

import (
	"testing"
	"time"
)

// fakeSettings feeds a fixed key so Configured()/Map() paths can run offline.
type fakeSettings struct{ key string }

func (f fakeSettings) GetSettings() (map[string]string, error) {
	return map[string]string{"aa_api_key": f.key}, nil
}

// storeWith preloads rows so match() runs without any network.
func storeWith(names ...string) *Store {
	s := New(fakeSettings{key: "x"})
	rows := make([]aaModel, 0, len(names))
	for _, n := range names {
		m := aaModel{Name: n}
		m.nID, m.nSlug, m.nName = "", "", normalize(n)
		rows = append(rows, m)
	}
	s.rows = rows
	s.fetchedAt = time.Now()
	return s
}

func TestMatchExact(t *testing.T) {
	s := storeWith("GPT-5.6 Sol", "GLM-5.3")
	if r := s.match(s.rows, "workbuddy", "glm-5.3", ""); r == nil || r.Name != "GLM-5.3" {
		t.Fatalf("want GLM-5.3, got %+v", r)
	}
}

// The dominant false positive from live calibration: a bare-prefix containment
// must not bind a newer version to an older AA entry.
func TestFuzzyRejectsVersionSplit(t *testing.T) {
	s := storeWith("Kimi K2") // only the old K2 exists
	if r := s.match(s.rows, "workbuddy", "kimi-k2.8-preview", ""); r != nil {
		t.Fatalf("kimi-k2.8-preview must not match %q", r.Name)
	}
}

func TestFuzzyAcceptsSameVersion(t *testing.T) {
	s := storeWith("Kimi K2.8")
	if r := s.match(s.rows, "workbuddy", "kimi-k2.8-preview", ""); r == nil {
		t.Fatal("kimi-k2.8-preview should match Kimi K2.8")
	}
}

func TestQoderMatchesOnName(t *testing.T) {
	// id is obfuscated; the DisplayName is the real model.
	s := storeWith("Qwen3.8 Max (0902)")
	if r := s.match(s.rows, "qoder", "qoder/qmodel_38max", "Qwen3.8-Max"); r == nil {
		t.Fatal("qoder should match via name")
	}
}

func TestOpencodeStripsSuffixes(t *testing.T) {
	s := storeWith("MiMo-V2.5")
	if r := s.match(s.rows, "opencode", "opencode/mimo-v2.5-free", ""); r == nil {
		t.Fatal("opencode should strip -free and match MiMo-V2.5")
	}
}

func TestKeywordMapRecoversHunyuan(t *testing.T) {
	s := storeWith("Hunyuan Hybrid Thinking")
	if r := s.match(s.rows, "workbuddy", "hy4-preview", ""); r == nil {
		t.Fatal("hy4-preview should map to a Hunyuan entry via keyword table")
	}
}

func TestRoleAliasNoMatch(t *testing.T) {
	s := storeWith("GPT-5.6 Sol", "GLM-5.3")
	if r := s.match(s.rows, "workbuddy", "default-model", ""); r != nil {
		t.Fatalf("role alias should not match, got %q", r.Name)
	}
}
