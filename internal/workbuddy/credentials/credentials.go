// Package credentials reads local CodeBuddy/WorkBuddy auth files and manages
// token refresh. Ported from workbuddy_one/credentials.py.
package credentials

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"work2api/internal/workbuddy/httpclient"
	"work2api/internal/workbuddy/profiles"
	"work2api/internal/workbuddy/siterouting"
)

// AuthDirsFunc resolves the directories scanned for *.info files. Overridable
// for tests. authDir, when non-empty, wins outright.
func AuthDirs(authDir, projectAuthsDir string) []string {
	if authDir != "" {
		return []string{authDir}
	}
	var dirs []string
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "darwin":
		dirs = append(dirs, filepath.Join(home, "Library", "Application Support", "CodeBuddyExtension", "Data", "Public", "auth"))
	case "windows":
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			local = filepath.Join(home, "AppData", "Local")
		}
		dirs = append(dirs, filepath.Join(local, "CodeBuddyExtension", "Data", "Public", "auth"))
	default:
		xdg := os.Getenv("XDG_DATA_HOME")
		if xdg == "" {
			xdg = filepath.Join(home, ".local", "share")
		}
		dirs = append(dirs, filepath.Join(xdg, "CodeBuddyExtension", "Data", "Public", "auth"))
	}
	if projectAuthsDir != "" {
		dirs = append(dirs, projectAuthsDir)
	}
	return dirs
}

// FindAuthFiles returns all *.info auth files across the auth dirs.
func FindAuthFiles(authDir, projectAuthsDir string) []string {
	var found []string
	for _, d := range AuthDirs(authDir, projectAuthsDir) {
		info, err := os.Stat(d)
		if err != nil || !info.IsDir() {
			continue
		}
		matches, _ := filepath.Glob(filepath.Join(d, "*.info"))
		sort.Strings(matches)
		found = append(found, matches...)
	}
	return found
}

// Session is the decoded auth file: {auth:{...}, account:{...}}.
type Session struct {
	Auth    map[string]any `json:"auth"`
	Account map[string]any `json:"account"`
	// raw preserves unknown top-level fields on write-back.
	raw map[string]any
}

// Manager manages one account's credentials: read, expiry, refresh, write-back.
type Manager struct {
	path   string
	client *http.Client
	mu     sync.Mutex
	cached *Session
	mtime  time.Time
}

// NewManager returns a credential manager for one *.info file.
func NewManager(path string) *Manager {
	return &Manager{
		path:   path,
		client: httpclient.New(15 * time.Second), // no proxy (mirrors trust_env=False)
	}
}

// Path returns the auth file path (used to classify account source).
func (m *Manager) Path() string { return m.path }

func (m *Manager) readRaw() (*Session, error) {
	data, err := os.ReadFile(m.path)
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	s := &Session{raw: raw}
	if a, ok := raw["auth"].(map[string]any); ok {
		s.Auth = a
	} else {
		s.Auth = map[string]any{}
	}
	if a, ok := raw["account"].(map[string]any); ok {
		s.Account = a
	} else {
		s.Account = map[string]any{}
	}
	return s, nil
}

func (m *Manager) loadIfStale() {
	info, err := os.Stat(m.path)
	if err != nil {
		return
	}
	if m.cached == nil || !info.ModTime().Equal(m.mtime) {
		if s, err := m.readRaw(); err == nil {
			m.cached = s
			m.mtime = info.ModTime()
		}
	}
}

func (m *Manager) session() (*Session, error) {
	m.loadIfStale()
	if m.cached == nil {
		return nil, errors.New("无法读取 auth 文件：" + m.path)
	}
	return m.cached, nil
}

func (m *Manager) isExpired() bool {
	s, err := m.session()
	if err != nil {
		return true
	}
	var expiresAt float64
	if v, ok := s.Auth["expiresAt"].(float64); ok {
		expiresAt = v
	}
	return float64(time.Now().UnixMilli()) >= (expiresAt - 60_000)
}

// Profile returns the current credential's profile (falls back to default).
func (m *Manager) Profile() string {
	s, err := m.session()
	if err != nil {
		return siterouting.DefaultProfile
	}
	p, err := siterouting.ProfileForAuth(s.Auth)
	if err != nil {
		return siterouting.DefaultProfile
	}
	return p
}

// Endpoint returns the current credential's upstream entry.
func (m *Manager) Endpoint() string {
	s, err := m.session()
	if err != nil {
		ep, _ := siterouting.EndpointForProfile(siterouting.DefaultProfile)
		return ep
	}
	ep, err := siterouting.EndpointForAuth(s.Auth)
	if err != nil {
		ep, _ = siterouting.EndpointForProfile(siterouting.DefaultProfile)
	}
	return ep
}

// refresh fetches a new token and writes it back atomically. Caller holds mu.
func (m *Manager) refresh() error {
	s, err := m.session()
	if err != nil {
		return err
	}
	headers := profiles.CredentialHeaders(s.Auth, profiles.Account(s.Account))
	refreshToken, _ := s.Auth["refreshToken"].(string)
	headers["X-Refresh-Token"] = refreshToken
	headers["X-Auth-Refresh-Source"] = "plugin"
	url, err := siterouting.RefreshURLForAuth(s.Auth)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body := readAll(resp.Body)
	var data map[string]any
	if err := json.Unmarshal(body, &data); err != nil {
		return errors.New("刷新 token 失败（非 JSON 响应）")
	}
	if resp.StatusCode >= 400 {
		return errors.New("刷新 token 失败（HTTP " + itoa(resp.StatusCode) + "）")
	}
	if code, _ := data["code"].(float64); code != 0 || data["data"] == nil {
		return errors.New("刷新 token 失败")
	}
	newAuth, _ := data["data"].(map[string]any)
	if newAuth == nil {
		return errors.New("刷新 token 失败：空 data")
	}
	if empty(newAuth["domain"]) {
		newAuth["domain"] = s.Auth["domain"]
	}
	now := time.Now().UnixMilli()
	newAuth["lastRefreshTime"] = now
	if empty(newAuth["expiresAt"]) {
		if in, ok := newAuth["expiresIn"].(float64); ok {
			newAuth["expiresAt"] = float64(now) + in*1000
		}
	}
	if empty(newAuth["refreshExpiresAt"]) {
		if in, ok := newAuth["refreshExpiresIn"].(float64); ok {
			newAuth["refreshExpiresAt"] = float64(now) + in*1000
		}
	}
	s.Auth = newAuth
	s.raw["auth"] = newAuth
	// atomic write-back
	out, err := json.MarshalIndent(s.raw, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.path); err != nil {
		return err
	}
	m.cached = s
	if info, err := os.Stat(m.path); err == nil {
		m.mtime = info.ModTime()
	}
	return nil
}

// encryptedToken reports whether auth stores accessToken as CodeBuddy's
// at-rest encrypted envelope ({"$wbEncrypted":1,"envelope":"..."}) rather than a
// plaintext string. The newer CodeBuddy desktop encrypts the local Tencent
// (domestic) token this way; we cannot decrypt it (the key lives in the
// extension), so such a credential is unusable until re-imported in plaintext
// (e.g. via WebUI OAuth login, which is how intl accounts already work).
func encryptedToken(auth map[string]any) bool {
	if m, ok := auth["accessToken"].(map[string]any); ok {
		if _, enc := m["$wbEncrypted"]; enc {
			return true
		}
	}
	return false
}

// GetHeaders returns request headers, refreshing the token if expired.
func (m *Manager) GetHeaders() (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, err := m.session(); err == nil && encryptedToken(s.Auth) {
		return nil, errors.New("该账号 token 被 CodeBuddy 加密存储（$wbEncrypted），无法直接读取；请在 WebUI 重新扫码登录导入该账号")
	}
	if m.isExpired() {
		if err := m.refresh(); err != nil {
			return nil, err
		}
	}
	s, err := m.session()
	if err != nil {
		return nil, err
	}
	return profiles.CredentialHeaders(s.Auth, profiles.Account(s.Account)), nil
}

// CatalogHeaders returns model-catalog request headers.
func (m *Manager) CatalogHeaders() (map[string]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.session()
	if err != nil {
		return nil, err
	}
	return profiles.CatalogHeaders(s.Auth, profiles.Account(s.Account)), nil
}

// Keepalive forces a token refresh. Returns true on success.
func (m *Manager) Keepalive() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refresh() == nil
}

// RawSession returns the full auth file content.
func (m *Manager) RawSession() (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.session()
}

// Summary returns display fields for the account.
func (m *Manager) Summary() map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, err := m.session()
	if err != nil {
		return map[string]any{}
	}
	return map[string]any{
		"uid":           s.Account["uid"],
		"nickname":      s.Account["nickname"],
		"enterprise_id": s.Account["enterpriseId"],
		"token_expired": m.isExpired(),
	}
}

func empty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	default:
		return false
	}
}

func readAll(r interface{ Read([]byte) (int, error) }) []byte {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
		}
		if err != nil {
			break
		}
	}
	return buf
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
