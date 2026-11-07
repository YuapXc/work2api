// Package benchmarks integrates Artificial Analysis (artificialanalysis.ai)
// third-party LLM evaluations into the model catalog. The WebUI reads a
// derived, key-free view from the server-side cache. AA data is keyed by
// standard vendor slugs and names. Catalog matching preserves model versions
// and reasoning variants; ambiguous aliases require an administrator binding.
package benchmarks

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	endpoint     = "https://artificialanalysis.ai/api/v2/language/models/free"
	failCooldown = 6 * time.Hour
	reqTimeout   = 20 * time.Second
	modelURL     = "https://artificialanalysis.ai/models/"
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
	Performance struct {
		OutputTokensPerSecond *float64 `json:"median_output_tokens_per_second"`
		FirstAnswerSeconds    *float64 `json:"median_time_to_first_answer_token_seconds"`
	} `json:"performance"`
	Pricing struct {
		InputPerMillion  *float64 `json:"price_1m_input_tokens"`
		OutputPerMillion *float64 `json:"price_1m_output_tokens"`
	} `json:"pricing"`
}

// Result is the key-free, WebUI-facing view of one model's evaluation.
type Result struct {
	Name                  string   `json:"name"`
	Creator               string   `json:"creator,omitempty"`
	Intelligence          *float64 `json:"intelligence_index"`
	Coding                *float64 `json:"coding_index"`
	Math                  *float64 `json:"math_index"`
	Source                string   `json:"source"`
	AAURL                 string   `json:"aa_url,omitempty"`
	OutputTokensPerSecond *float64 `json:"median_output_tokens_per_second,omitempty"`
	FirstAnswerSeconds    *float64 `json:"median_time_to_first_answer_token_seconds,omitempty"`
	InputPerMillion       *float64 `json:"price_1m_input_tokens,omitempty"`
	OutputPerMillion      *float64 `json:"price_1m_output_tokens,omitempty"`
}

// Store caches AA rows and resolves per-provider model ids to evaluations.
type Store struct {
	settings SettingsSource
	client   *http.Client

	mu        sync.Mutex
	refreshMu sync.Mutex
	catalog   *Catalog
	lastFail  time.Time
}

// New builds a Store that reads the AA key from the given settings source.
func New(settings SettingsSource) *Store {
	return &Store{
		settings: settings,
		client: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

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

type catalogPage struct {
	Pagination *struct {
		Page    int  `json:"page"`
		HasMore bool `json:"has_more"`
	} `json:"pagination"`
	Data []aaModel `json:"data"`
}

func (s *Store) fetchPage(ctx context.Context, key string, page int) (catalogPage, error) {
	var payload catalogPage
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s?page=%d", endpoint, page), nil)
	if err != nil {
		return payload, errors.New("无法创建 Artificial Analysis 请求")
	}
	req.Header.Set("x-api-key", key)
	resp, err := s.client.Do(req)
	if err != nil {
		return payload, errors.New("连接 Artificial Analysis 失败或超时")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return payload, errors.New("Artificial Analysis API Key 无效（401）")
	case http.StatusTooManyRequests:
		return payload, errors.New("Artificial Analysis 请求额度已用尽（429）")
	default:
		return payload, fmt.Errorf("Artificial Analysis 返回 HTTP %d", resp.StatusCode)
	}
	const maxPageBytes = 16 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes+1))
	if err != nil || len(body) > maxPageBytes || json.Unmarshal(body, &payload) != nil ||
		payload.Pagination == nil || payload.Pagination.Page != page || len(payload.Data) == 0 {
		return payload, fmt.Errorf("Artificial Analysis 第 %d 页目录无效或为空", page)
	}
	return payload, nil
}

func (s *Store) fetch(ctx context.Context, key string) ([]aaModel, error) {
	ctx, cancel := context.WithTimeout(ctx, reqTimeout)
	defer cancel()
	var rows []aaModel
	for page := 1; ; page++ {
		payload, err := s.fetchPage(ctx, key, page)
		if err != nil {
			return nil, err
		}
		rows = append(rows, payload.Data...)
		if !payload.Pagination.HasMore {
			break
		}
	}
	return rows, nil
}

func (s *Store) RefreshIfEmpty(ctx context.Context) {
	_ = s.refresh(ctx, false)
}

// Refresh replaces the cache only after fetching all pages.
func (s *Store) Refresh(ctx context.Context) error {
	return s.refresh(ctx, true)
}

func (s *Store) refresh(ctx context.Context, force bool) error {
	if !s.refreshMu.TryLock() {
		return errors.New("Artificial Analysis 评测正在刷新，请稍后再试")
	}
	defer s.refreshMu.Unlock()
	if !force && (s.catalog != nil || time.Since(s.lastFail) < failCooldown) {
		return nil
	}
	key := s.key()
	if key == "" {
		return errors.New("未配置 Artificial Analysis API Key")
	}
	rows, err := s.fetch(ctx, key)
	if err != nil {
		if ctx.Err() == nil {
			s.lastFail = time.Now()
		}
		return err
	}
	catalog := newCatalog(rows)
	if len(catalog.families) == 0 {
		s.lastFail = time.Now()
		return errors.New("Artificial Analysis 未返回可用的模型条目")
	}
	s.mu.Lock()
	s.catalog = catalog
	s.mu.Unlock()
	s.lastFail = time.Time{}
	return nil
}

var (
	opencodeSuffix = regexp.MustCompile(`-(free|contributor)$`)
	validModelSlug = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

func seed(providerName, id string) string {
	base := strings.TrimPrefix(id, providerName+"/")
	if providerName == "opencode" {
		return opencodeSuffix.ReplaceAllString(base, "")
	}
	return base
}

func (r aaModel) result() Result {
	var url string
	if validModelSlug.MatchString(r.Slug) {
		url = modelURL + r.Slug
	}
	return Result{
		Name:                  r.Name,
		Creator:               r.Creator.Name,
		Intelligence:          r.Evaluations.Intelligence,
		Coding:                r.Evaluations.Coding,
		Math:                  r.Evaluations.Math,
		Source:                "aa",
		AAURL:                 url,
		OutputTokensPerSecond: r.Performance.OutputTokensPerSecond,
		FirstAnswerSeconds:    r.Performance.FirstAnswerSeconds,
		InputPerMillion:       r.Pricing.InputPerMillion,
		OutputPerMillion:      r.Pricing.OutputPerMillion,
	}
}
