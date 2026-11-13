package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"work2api/internal/core/provider"
	"work2api/internal/statebackup"
	"work2api/internal/workbuddy/httpclient"
)

type refreshTransport func(*http.Request) (*http.Response, error)

func (f refreshTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func writeTestCredential(t *testing.T, path, token string) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"auth":    map[string]any{"accessToken": token, "refreshToken": "refresh-1", "expiresAt": 1, "domain": "www.workbuddy.cn"},
		"account": map[string]any{"uid": "test", "nickname": "alias"}, "preserved": "top-level",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestChatIdentityPreviewIsOfflineAndRedacted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "preview.info")
	writeTestCredential(t, path, "private-access-token")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture map[string]any
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	fixture["auth"].(map[string]any)["deviceToken"] = "private-device-token"
	data, err = json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(path)
	m.client = &http.Client{Transport: refreshTransport(func(*http.Request) (*http.Response, error) {
		t.Error("offline preview attempted a network request")
		return nil, errors.New("no network")
	})}
	preview, err := m.ChatIdentityPreview()
	if err != nil || preview["local_only"] != true || preview["endpoint"] == "" {
		t.Fatal(preview, err)
	}
	raw, err := json.Marshal(preview)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private-access-token", "private-device-token", "refresh-1", path, `"alias"`, `"test"`} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("identity preview leaked private data")
		}
	}
	if present, ok := preview["present"].(map[string]bool); !ok || !present["Authorization"] || !present["X-Device-Token"] {
		t.Fatal("credential presence missing", preview)
	}
	if err := m.Retire(false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.ChatIdentityPreview(); err == nil {
		t.Fatal("removed account still has an identity preview")
	}
}

func refreshResponse(r *http.Request, body string) *http.Response {
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}
}

func TestRefreshSharedCancellationAndBackup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.info")
	writeTestCredential(t, path, "old")
	m := NewManager(path)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	m.client = &http.Client{Transport: refreshTransport(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
			return refreshResponse(r, `{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	canceled := make(chan error, 1)
	go func() { _, err := m.GetChatHeadersContext(ctx); canceled <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	// The shared task must hold backup coordination after its caller leaves.
	cancel()
	select {
	case err := <-canceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter blocked on network mutex")
	}
	if unlock, ok := statebackup.TrySnapshot(); ok {
		unlock()
		t.Fatal("backup omitted running refresh")
	}
	var wg sync.WaitGroup
	results := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := m.GetHeadersContext(context.Background())
			if err == nil && h["Authorization"] != "Bearer new" {
				err = errors.New("old token returned")
			}
			results <- err
		}()
	}
	close(release)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("duplicate refresh", calls.Load())
	}
	s, err := m.RawSession()
	if err != nil || s.Auth["refreshToken"] != "refresh-1" || s.raw["preserved"] != "top-level" {
		t.Fatal("credential metadata lost", err)
	}
	// Deferred task cleanup may follow notification by a scheduler quantum.
	deadline := time.Now().Add(time.Second)
	for {
		if unlock, ok := statebackup.TrySnapshot(); ok {
			unlock()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("backup lease leaked")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRefreshDoesNotOverwriteReplacement(t *testing.T) {
	for _, retire := range []bool{false, true} {
		t.Run(map[bool]string{false: "reimport", true: "delete"}[retire], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.info")
			writeTestCredential(t, path, "old")
			m := NewManager(path)
			entered, release := make(chan struct{}), make(chan struct{})
			m.client = &http.Client{Transport: refreshTransport(func(r *http.Request) (*http.Response, error) {
				close(entered)
				select {
				case <-release:
					return refreshResponse(r, `{"code":0,"data":{"accessToken":"stale","expiresIn":3600}}`), nil
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
			})}
			result := make(chan error, 1)
			go func() { _, err := m.GetHeadersContext(context.Background()); result <- err }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("refresh did not start")
			}
			if retire {
				if err := m.Retire(true); err != nil {
					t.Fatal(err)
				}
			} else {
				writeTestCredential(t, path, "replacement")
			}
			close(release)
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("stale refresh succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("task failed to finish")
			}
			if retire {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("deleted credential recreated", err)
				}
			} else {
				raw, err := os.ReadFile(path)
				if err != nil || strings.Contains(string(raw), "stale") || !strings.Contains(string(raw), "replacement") {
					t.Fatal("replacement overwritten", err)
				}
			}
		})
	}
}

func TestRefreshRejectsMissingTokenWithoutChangingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.info")
	writeTestCredential(t, path, "old")
	before, _ := os.ReadFile(path)
	m := NewManager(path)
	m.client = &http.Client{Transport: refreshTransport(func(r *http.Request) (*http.Response, error) {
		return refreshResponse(r, `{"code":0,"data":{"expiresIn":3600}}`), nil
	})}
	if _, err := m.GetHeadersContext(context.Background()); err == nil {
		t.Fatal("missing access token accepted")
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(before) {
		t.Fatal("invalid response changed credential")
	}
}

func TestConfirmedAuthExpiryStopsAllCredentialConsumers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.info")
	writeTestCredential(t, path, "old")
	m := NewManager(path)
	g := provider.NewMaintenanceGuard(filepath.Join(t.TempDir(), "maintenance.json"))
	m.BindMaintenance(g, "account")
	var calls atomic.Int32
	m.client = &http.Client{Transport: refreshTransport(func(req *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
		}
		return refreshResponse(req, `{"code":0,"data":{"accessToken":"valid","expiresIn":3600}}`), nil
	})}
	if _, err := m.GetChatHeadersContext(context.Background()); !errors.Is(err, ErrLoginRequired) {
		t.Fatal(err)
	}
	if !m.AuthorizationInvalid() {
		t.Fatal("refresh 401 did not invalidate credential")
	}
	if _, err := m.CatalogHeadersContext(context.Background()); !errors.Is(err, ErrLoginRequired) {
		t.Fatal(err)
	}
	if err := m.KeepaliveErrorContext(context.Background()); !errors.Is(err, ErrLoginRequired) {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("known invalid credential was retried", calls.Load())
	}
	writeTestCredential(t, path, "new-credential")
	if _, err := m.GetChatHeadersContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.AuthorizationInvalid() || calls.Load() != 2 {
		t.Fatal("new credential could not recover")
	}
}

func TestEnterpriseRedirectCannotForwardCredentialsToAnotherHost(t *testing.T) {
	from, _ := http.NewRequest("GET", "https://team.sso.codebuddy.cn/v3/config", nil)
	for _, target := range []string{"https://evil.test/x", "http://team.sso.codebuddy.cn/x", "https://team.sso.codebuddy.cn:444/x"} {
		req, _ := http.NewRequest("GET", target, nil)
		if httpclient.GuardRedirect(nil)(req, []*http.Request{from}) != http.ErrUseLastResponse {
			t.Fatal("cross-origin redirect accepted", target)
		}
	}
	req, _ := http.NewRequest("GET", "https://team.sso.codebuddy.cn/next", nil)
	if err := httpclient.GuardRedirect(nil)(req, []*http.Request{from}); err != nil {
		t.Fatal(err)
	}
}

func TestEnterpriseRenewalRetainsAccountOrigin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "enterprise.info")
	writeTestCredential(t, path, "old")
	raw, _ := os.ReadFile(path)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	doc["auth"].(map[string]any)["domain"] = "team.sso.codebuddy.cn"
	raw, _ = json.Marshal(doc)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(path)
	m.client = &http.Client{Transport: refreshTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "team.sso.codebuddy.cn" || r.URL.Path != "/v2/plugin/auth/token/refresh" || r.Header.Get("X-Domain") != "team.sso.codebuddy.cn" {
			t.Error("renewal wrong origin", r.URL)
		}
		return refreshResponse(r, `{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`), nil
	})}
	if _, err := m.GetChatHeadersContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m.Endpoint() != "https://team.sso.codebuddy.cn" {
		t.Fatal("renewal lost enterprise", m.Endpoint())
	}
}
