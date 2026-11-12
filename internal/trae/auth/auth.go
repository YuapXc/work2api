// Adapted from mmqz/cpa-multi-plugins, MIT; see NOTICE.md.
package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func (a *Auth) MaintenanceIdentity() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.maintenanceIdentityLocked()
}
func (a *Auth) TryMaintenanceIdentity() (string, bool) {
	if !a.mu.TryRLock() {
		return "", false
	}
	defer a.mu.RUnlock()
	return a.maintenanceIdentityLocked(), true
}
func (a *Auth) maintenanceIdentityLocked() string {
	raw, _ := json.Marshal([]any{a.AccessToken, a.RefreshToken, a.Variant, a.UID, a.EnterpriseConsole, a.EnterpriseID})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type Auth struct {
	mu sync.RWMutex

	AccessToken       string // Cloud-IDE-JWT 头用
	RefreshToken      string // 每次 ExchangeToken 轮换
	ExpiresAt         int64  // Unix 秒（accessToken 过期时刻）
	Domain            string // "trae.cn"
	APIHost           string // "https://api.trae.com.cn"（ExchangeToken host）
	MachineID         string // x-machine-id
	DeviceID          string // x-device-id
	DevicePublicKey   string
	DevicePrivateKey  string
	Variant           string // cn | solo (merged trae-solo-cn); intl handled by intl parser
	UID               string
	EnterpriseID      string
	EnterpriseConsole bool
	Nickname          string
	FilePath          string // 落盘路径；refresh 后原子写回
}

func (a *Auth) Lock()          { a.mu.Lock() }
func (a *Auth) Unlock()        { a.mu.Unlock() }
func (a *Auth) RLock()         { a.mu.RLock() }
func (a *Auth) TryRLock() bool { return a.mu.TryRLock() }
func (a *Auth) RUnlock()       { a.mu.RUnlock() }
func (a *Auth) JWT() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.AccessToken
}
func (a *Auth) RefreshTokenValue() string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.RefreshToken
}
func (a *Auth) NeedsRefresh(within time.Duration) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.NeedsRefreshLocked(within)
}
func (a *Auth) NeedsRefreshLocked(within time.Duration) bool {
	if a.ExpiresAt <= 0 {
		return true
	}
	return time.Now().Add(within).Unix() >= a.ExpiresAt
}
func parseNested(raw []byte) (*Auth, error) {
	var n struct {
		Auth struct {
			AccessToken      string `json:"accessToken"`
			RefreshToken     string `json:"refreshToken"`
			ExpiresAt        int64  `json:"expiresAt"`
			Domain           string `json:"domain"`
			APIHost          string `json:"apiHost"`
			MachineID        string `json:"machineId"`
			DeviceID         string `json:"deviceId"`
			DevicePublicKey  string `json:"devicePublicKey"`
			DevicePrivateKey string `json:"devicePrivateKey"`
			Variant          string `json:"variant"`
		} `json:"auth"`
		Account struct {
			UID          string `json:"uid"`
			EnterpriseID string `json:"enterpriseId"`
			Nickname     string `json:"nickname"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &n); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	return &Auth{
		AccessToken:      n.Auth.AccessToken,
		RefreshToken:     n.Auth.RefreshToken,
		ExpiresAt:        n.Auth.ExpiresAt,
		Domain:           n.Auth.Domain,
		APIHost:          n.Auth.APIHost,
		MachineID:        n.Auth.MachineID,
		DeviceID:         n.Auth.DeviceID,
		DevicePublicKey:  n.Auth.DevicePublicKey,
		DevicePrivateKey: n.Auth.DevicePrivateKey,
		Variant:          n.Auth.Variant,
		UID:              n.Account.UID,
		EnterpriseID:     n.Account.EnterpriseID,
		Nickname:         n.Account.Nickname,
	}, nil
}
func parseFlat(raw []byte) (*Auth, error) {
	var f struct {
		AccessToken      string `json:"accessToken"`
		RefreshToken     string `json:"refreshToken"`
		ExpiresAt        int64  `json:"expiresAt"`
		Domain           string `json:"domain"`
		APIHost          string `json:"apiHost"`
		MachineID        string `json:"machineId"`
		DeviceID         string `json:"deviceId"`
		DevicePublicKey  string `json:"devicePublicKey"`
		DevicePrivateKey string `json:"devicePrivateKey"`
		Variant          string `json:"variant"`
		UID              string `json:"uid"`
		EnterpriseID     string `json:"enterpriseId"`
		Nickname         string `json:"nickname"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	return &Auth{
		AccessToken:      f.AccessToken,
		RefreshToken:     f.RefreshToken,
		ExpiresAt:        f.ExpiresAt,
		Domain:           f.Domain,
		APIHost:          f.APIHost,
		MachineID:        f.MachineID,
		DeviceID:         f.DeviceID,
		DevicePublicKey:  f.DevicePublicKey,
		DevicePrivateKey: f.DevicePrivateKey,
		Variant:          f.Variant,
		UID:              f.UID,
		EnterpriseID:     f.EnterpriseID,
		Nickname:         f.Nickname,
	}, nil
}
func Parse(raw []byte) (*Auth, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty auth storage")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("storage_parse_error: %w", err)
	}
	var (
		a   *Auth
		err error
	)
	if _, nested := probe["auth"]; nested {
		a, err = parseNested(raw)
	} else {
		a, err = parseFlat(raw)
	}
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("parse_error: missing accessToken")
	}
	var flat map[string]any
	_ = json.Unmarshal(raw, &flat)
	node := flat
	if nested, ok := flat["auth"].(map[string]any); ok {
		node = nested
	}
	host, _ := node["host"].(string)
	if host == "" {
		host, _ = node["apiHost"].(string)
	}
	parsed, _ := url.Parse(host)
	a.EnterpriseConsole = parsed != nil && parsed.Scheme == "https" && parsed.User == nil && strings.EqualFold(parsed.Hostname(), "console.enterprise.trae.cn") || node["enterpriseConsole"] == true
	return a, nil
}
func (a *Auth) SaveAtomic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.saveAtomicLocked()
}
func (a *Auth) saveAtomicLocked() error {
	if a.FilePath == "" {
		return fmt.Errorf("no FilePath set")
	}
	authDoc := map[string]any{
		"accessToken":       a.AccessToken,
		"refreshToken":      a.RefreshToken,
		"expiresAt":         a.ExpiresAt,
		"domain":            a.Domain,
		"apiHost":           a.APIHost,
		"enterpriseConsole": a.EnterpriseConsole,
		"machineId":         a.MachineID,
		"deviceId":          a.DeviceID,
	}
	if a.DevicePublicKey != "" {
		authDoc["devicePublicKey"] = a.DevicePublicKey
	}
	if a.DevicePrivateKey != "" {
		authDoc["devicePrivateKey"] = a.DevicePrivateKey
	}
	if a.Variant != "" {
		authDoc["variant"] = a.Variant
	}
	doc := map[string]any{
		"auth": authDoc,
		"account": map[string]any{
			"uid":          a.UID,
			"enterpriseId": a.EnterpriseID,
			"nickname":     a.Nickname,
		},
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(a.FilePath), ".trae-credentials-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(tmp, a.FilePath)
}
func LoadDir(dir string) ([]*Auth, error) {
	files, err := filepath.Glob(filepath.Join(dir, "trae-*.json"))
	if err != nil {
		return nil, err
	}
	var out []*Auth
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		a, err := Parse(raw)
		if err != nil {
			continue
		}
		a.FilePath = f
		out = append(out, a)
	}
	return out, nil
}
func TokenIssuedAt(jwtRaw string) (time.Time, bool) {
	parts := strings.Split(strings.TrimSpace(jwtRaw), ".")
	if len(parts) < 2 || parts[1] == "" {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		raw, err = base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return time.Time{}, false
		}
	}
	var payload struct {
		Iat int64 `json:"iat"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Iat <= 0 {
		return time.Time{}, false
	}
	return time.Unix(payload.Iat, 0), true
}

const issuedRotateMax = 15 * 24 * time.Hour

func (a *Auth) IssuedTooLong() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return IssuedTooLongLocked(a.AccessToken)
}
func IssuedTooLongLocked(jwtRaw string) bool {
	iat, ok := TokenIssuedAt(jwtRaw)
	if !ok {
		return false
	}
	return time.Since(iat) >= issuedRotateMax
}
