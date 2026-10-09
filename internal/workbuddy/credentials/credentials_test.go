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

	"work2api/internal/statebackup"
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
