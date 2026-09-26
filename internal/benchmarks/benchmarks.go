// Package benchmarks integrates Artificial Analysis (artificialanalysis.ai)
// third-party LLM evaluations into the model catalog. AA exposes a free API
// (GET /api/v2/data/llms/models, x-api-key header, ~1000 req/day) that the docs
// require callers to cache server-side and never expose the key to clients.
//
// So this package fetches + caches on the backend (24h success, 6h failure
// cooldown); the WebUI reads a derived, key-free view. AA data is keyed by
// standard vendor slugs/names, while our three providers name models very
// differently, so Lookup resolves a per-provider "seed" and matches it against
// AA in three layers: exact (id/slug/name) → curated keyword map → a
// version-safe fuzzy containment. The fuzzy layer refuses matches that split a
// numeric version token (so "kimi-k2.8-preview" never binds to AA "Kimi K2"),
// which calibration against the live catalog proved was the dominant false
// positive.
package benchmarks

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	endpoint     = "https://artificialanalysis.ai/api/v2/data/llms/models"
	cacheTTL     = 24 * time.Hour
	failCooldown = 6 * time.Hour
	reqTimeout   = 20 * time.Second
	aaURL        = "https://artificialanalysis.ai/models"
)

// SettingsSource reads the persisted settings (the AA key lives under aa_api_key).
type SettingsSource interface {
	GetSettings() (map[string]string, error)
}

// aaModel is one row of the AA response we care about.
type aaModel struct {
	ID      string `json:"id"`
	Slug    string `json:"slug"`
	Name    string `json:"name"`
	Creator struct {
		Name string `json:"name"`
	} `json:"model_creator"`
	Evaluations struct {
		Intelligence *float64 `json:"artificial_analysis_intelligence_index"`
		Coding       *float64 `json:"artificial_analysis_coding_index"`
		Math         *float64 `json:"artificial_analysis_math_index"`
	} `json:"evaluations"`

	nID, nSlug, nName string // normalized match keys (filled on ingest)
}

// Result is the key-free, WebUI-facing view of one model's evaluation.
type Result struct {
	Name         string   `json:"name"`
	Creator      string   `json:"creator,omitempty"`
	Intelligence *float64 `json:"intelligence_index"`
	Coding       *float64 `json:"coding_index"`
	Math         *float64 `json:"math_index"`
	Source       string   `json:"source"`
	AAURL        string   `json:"aa_url"`
}

// Store caches AA rows and resolves per-provider model ids to evaluations.
type Store struct {
	settings SettingsSource

	mu        sync.Mutex
	rows      []aaModel
	fetchedAt time.Time
	lastFail  time.Time
}

// New builds a Store that reads the AA key from the given settings source.
func New(settings SettingsSource) *Store { return &Store{settings: settings} }

// key returns the configured AA API key (trimmed), or "".
func (s *Store) key() string {
	if s.settings == nil {
		return ""
	}
	m, err := s.settings.GetSettings()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(m["aa_api_key"])
}

// Configured reports whether an AA key is set.
func (s *Store) Configured() bool { return s.key() != "" }

// HasCache reports whether any rows are cached (regardless of freshness). Lets
// request-path callers avoid triggering a blocking fetch.
func (s *Store) HasCache() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows) > 0
}

// fetch pulls the AA catalog. Returns nil (no error surfaced) on any failure so
// callers fall back to the existing cache; the outbound client honors the
// environment proxy since AA is a public third-party site (unlike the upstream
// provider APIs which must stay direct).
func (s *Store) fetch() []aaModel {
	key := s.key()
	if key == "" {
		return nil
	}
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("x-api-key", key)
	client := &http.Client{Timeout: reqTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil
	}
	var payload struct {
		Data []aaModel `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	for i := range payload.Data {
		payload.Data[i].nID = normalize(payload.Data[i].ID)
		payload.Data[i].nSlug = normalize(payload.Data[i].Slug)
		payload.Data[i].nName = normalize(payload.Data[i].Name)
	}
	return payload.Data
}

// ensure returns cached rows if fresh; otherwise refreshes (respecting the
// failure cooldown). Falls back to stale rows on fetch failure.
func (s *Store) ensure() []aaModel {
	now := time.Now()
	s.mu.Lock()
	if len(s.rows) > 0 && now.Sub(s.fetchedAt) < cacheTTL {
		rows := s.rows
		s.mu.Unlock()
		return rows
	}
	inFail := !s.lastFail.IsZero() && now.Sub(s.lastFail) < failCooldown
	stale := s.rows
	s.mu.Unlock()
	if inFail {
		return stale
	}
	rows := s.fetch()
	if rows == nil {
		s.mu.Lock()
		s.lastFail = time.Now()
		s.mu.Unlock()
		return stale
	}
	s.mu.Lock()
	s.rows = rows
	s.fetchedAt = time.Now()
	s.lastFail = time.Time{}
	s.mu.Unlock()
	return rows
}

// Refresh forces a fetch (for the scheduler / manual WebUI refresh). Keeps the
// existing cache on failure.
func (s *Store) Refresh() {
	rows := s.fetch()
	if rows == nil {
		s.mu.Lock()
		s.lastFail = time.Now()
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.rows = rows
	s.fetchedAt = time.Now()
	s.lastFail = time.Time{}
	s.mu.Unlock()
}

var (
	nonKey         = regexp.MustCompile(`[\s\-_.]+`)
	opencodeSuffix = regexp.MustCompile(`-(free|contributor|fin)\b`)
	qoderPrefix    = "qoder/"
	opencodePrefix = "opencode/"
)

// normalize strips separators and lowercases, so "GPT-5.6 Sol" and "gpt-5.6-sol"
// compare equal.
func normalize(s string) string {
	return nonKey.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "")
}

// keywordMap recovers models whose provider name doesn't resemble any AA
// slug/name, keyed by the normalized per-provider seed. Values are AA-name
// substrings tried in order. Curated from live calibration; keep small.
var keywordMap = map[string][]string{
	"hy4preview":         {"hunyuan hybrid", "hunyuan"}, // Tencent Hunyuan (workbuddy id)
	"hy4previewf":        {"hunyuan hybrid", "hunyuan"},
	"deepseekv41flashsg": {"deepseek v4.1 flash"}, // Singapore variant → base flash
	"kimik31":            {"kimi k3"},
	"deepseekflash":      {"deepseek v4 flash", "deepseek flash"}, // qoder dfmodel name
}

// seed picks the field to match on, per provider: workbuddy uses the (Tencent)
// id, qoder's id is obfuscated but its name is the real model, and opencode ids
// are real slugs behind the namespace + free/contributor suffixes.
func seed(providerName, id, name string) string {
	switch providerName {
	case "qoder":
		if strings.TrimSpace(name) != "" {
			return name
		}
		return strings.TrimPrefix(id, qoderPrefix)
	case "opencode":
		base := strings.TrimPrefix(id, opencodePrefix)
		for opencodeSuffix.MatchString(base) {
			base = opencodeSuffix.ReplaceAllString(base, "")
		}
		return base
	default: // workbuddy and any future provider: match on id
		return id
	}
}

// containsSafe reports whether needle occurs in hay without splitting a numeric
// version token — the char adjacent to the match on either side must not be a
// digit when the match boundary itself is a digit. This is what stops
// "kimik28preview" from matching "kimik2".
func containsSafe(hay, needle string) bool {
	if needle == "" || len(needle) < 4 {
		return false
	}
	from := 0
	for {
		i := strings.Index(hay[from:], needle)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(needle)
		beforeOK := i == 0 || !isDigit(hay[i-1]) || !isDigit(needle[0])
		afterOK := end == len(hay) || !isDigit(hay[end]) || !isDigit(needle[len(needle)-1])
		if beforeOK && afterOK {
			return true
		}
		from = i + 1
	}
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// match resolves one seed to an AA row (nil if none).
func (s *Store) match(rows []aaModel, providerName, id, name string) *aaModel {
	sd := normalize(seed(providerName, id, name))
	if sd == "" {
		return nil
	}
	// 1) exact id / slug / name
	for i := range rows {
		r := &rows[i]
		if sd == r.nID || sd == r.nSlug || sd == r.nName {
			return r
		}
	}
	// 2) curated keyword map (normalized-substring, version-safe)
	if kws := keywordMap[sd]; len(kws) > 0 {
		for _, kw := range kws {
			nkw := normalize(kw)
			for i := range rows {
				if rows[i].nName != "" && containsSafe(rows[i].nName, nkw) {
					return &rows[i]
				}
			}
		}
	}
	// 3) version-safe fuzzy containment on the name (both directions)
	for i := range rows {
		r := &rows[i]
		if r.nName == "" {
			continue
		}
		if containsSafe(r.nName, sd) || containsSafe(sd, r.nName) {
			return r
		}
	}
	return nil
}

// Lookup returns the raw AA row for a provider's model, or nil. Triggers a fetch
// only if the cache is empty (never on the request path via Map, which is
// read-only against ensure()).
func (s *Store) lookup(providerName, id, name string) *aaModel {
	rows := s.ensure()
	if len(rows) == 0 {
		return nil
	}
	return s.match(rows, providerName, id, name)
}

// Map returns the WebUI-facing evaluation for a provider's model, or nil.
func (s *Store) Map(providerName, id, name string) *Result {
	r := s.lookup(providerName, id, name)
	if r == nil {
		return nil
	}
	return &Result{
		Name:         r.Name,
		Creator:      r.Creator.Name,
		Intelligence: r.Evaluations.Intelligence,
		Coding:       r.Evaluations.Coding,
		Math:         r.Evaluations.Math,
		Source:       "aa",
		AAURL:        aaURL,
	}
}
