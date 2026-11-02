package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"work2api/internal/workbuddy/credentials"
	"work2api/internal/workbuddy/pool"
	"work2api/internal/workbuddy/upstream"
)

func TestConcurrentChat401Recovery(t *testing.T) {
	for _, mode := range []string{"success", "dead", "still401"} {
		t.Run(mode, func(t *testing.T) {
			dead := mode == "dead"
			var refresh, old, fresh atomic.Int32
			allOld := make(chan struct{})
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if strings.Contains(req.URL.Path, "refresh") {
					refresh.Add(1)
					time.Sleep(40 * time.Millisecond)
					w.Header().Set("Content-Type", "application/json")
					if dead {
						io.WriteString(w, `{"code":12153}`)
					} else {
						io.WriteString(w, `{"code":0,"data":{"accessToken":"new","expiresIn":3600}}`)
					}
					return
				}
				if req.Header.Get("Authorization") == "Bearer old" {
					if old.Add(1) == 8 {
						close(allOld)
					}
					<-allOld
					w.WriteHeader(401)
					io.WriteString(w, "<html>Authorization Required</html>")
					return
				}
				fresh.Add(1)
				if mode == "still401" {
					w.WriteHeader(401)
					io.WriteString(w, "private upstream HTML")
					return
				}
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`)
			}))
			defer srv.Close()
			transport := srv.Client().Transport.(*http.Transport).Clone()
			transport.TLSClientConfig.ServerName = "example.com"
			transport.DialContext = func(ctx context.Context, n, a string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, n, srv.Listener.Addr().String())
			}
			previous := http.DefaultTransport
			http.DefaultTransport = transport
			defer func() { http.DefaultTransport = previous; transport.CloseIdleConnections() }()
			path := filepath.Join(t.TempDir(), "auth.info")
			raw, _ := json.Marshal(map[string]any{"auth": map[string]any{"accessToken": "old", "refreshToken": "r", "domain": "www.workbuddy.cn", "expiresAt": time.Now().Add(time.Hour).UnixMilli()}, "account": map[string]any{"uid": "fixture"}})
			os.WriteFile(path, raw, 0600)
			mgr := credentials.NewManager(path)
			p := pool.New(map[string]pool.Credential{"fixture": mgr}, "")
			rt := &Runtime{Managers: map[string]*credentials.Manager{"fixture": mgr}, Pool: p, UpstreamClient: upstream.NewClient(&http.Client{Transport: transport})}
			var wg sync.WaitGroup
			errs := make(chan error, 8)
			for i := 0; i < 8; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_, err := rt.Stream(ctx, p.Get("fixture"), map[string]any{"model": "m", "messages": []any{}}, func(string) error { return nil }, AttemptOptions{})
					errs <- err
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if dead {
					e, ok := err.(*upstream.UpstreamError)
					if !ok || e.StatusCode != 401 || !strings.Contains(string(e.Raw), "重新授权") {
						t.Fatal("waiter lost login guidance", err)
					}
				} else if mode == "still401" {
					e, ok := err.(*upstream.UpstreamError)
					if !ok || e.StatusCode != 401 || !strings.Contains(string(e.Raw), "续期后") {
						t.Fatal(err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			if refresh.Load() != 1 || old.Load() != 8 || (!dead && fresh.Load() != 8) || (dead && fresh.Load() != 0) {
				t.Fatal(refresh.Load(), old.Load(), fresh.Load())
			}
		})
	}
}

func TestChat401LastRecoverySlotDoesNotRefreshOrReplay(t *testing.T) {
	var sends atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		w.WriteHeader(401)
		io.WriteString(w, "private upstream HTML")
	}))
	defer srv.Close()
	transport := srv.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = "example.com"
	transport.DialContext = func(ctx context.Context, n, a string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, n, srv.Listener.Addr().String())
	}
	defer transport.CloseIdleConnections()
	path := filepath.Join(t.TempDir(), "auth.info")
	raw, _ := json.Marshal(map[string]any{"auth": map[string]any{"accessToken": "old", "refreshToken": "r", "domain": "www.workbuddy.cn", "expiresAt": time.Now().Add(time.Hour).UnixMilli()}, "account": map[string]any{"uid": "fixture"}})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	mgr := credentials.NewManager(path)
	p := pool.New(map[string]pool.Credential{"fixture": mgr}, "")
	rt := &Runtime{Managers: map[string]*credentials.Manager{"fixture": mgr}, Pool: p, UpstreamClient: upstream.NewClient(&http.Client{Transport: transport})}
	ctx := context.WithValue(context.Background(), recoveryBudgetKey{}, &recoveryBudget{remaining: 1})
	_, err := rt.Stream(ctx, p.Get("fixture"), map[string]any{"model": "m", "messages": []any{}}, func(string) error { return nil }, AttemptOptions{})
	remote, ok := err.(*upstream.UpstreamError)
	if !ok || remote.StatusCode != 401 || !strings.Contains(string(remote.Raw), "授权") || strings.Contains(string(remote.Raw), "private upstream") || sends.Load() != 1 {
		t.Fatalf("lost final auth error or replayed: sends=%d err=%v", sends.Load(), err)
	}
}
