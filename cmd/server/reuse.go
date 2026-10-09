package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var buildFingerprint = ""

type launchOptions struct{ open, reuse, stop, info bool }

func launchArgs(args []string) (launchOptions, []string) {
	var opt launchOptions
	var remaining []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		// Preserve flag values verbatim, even when they resemble control flags.
		if arg == "--" {
			remaining = append(remaining, args[i:]...)
			break
		}
		switch arg {
		case "-host", "--host", "-port", "--port", "-data-dir", "--data-dir", "-db", "--db", "-admin-token", "--admin-token", "-log-level", "--log-level":
			remaining = append(remaining, arg)
			if i+1 < len(args) {
				i++
				remaining = append(remaining, args[i])
			}
		case "-open-browser":
			opt.open = true
		case "-reuse-only":
			opt.reuse = true
		case "-stop-running":
			opt.stop = true
		case "-build-info":
			opt.info = true
		default:
			remaining = append(remaining, arg)
		}
	}
	return opt, remaining
}

type instanceInfo struct {
	Application string `json:"application"`
	Identity    string `json:"identity"`
	Control     string `json:"control"`
	Token       string `json:"token,omitempty"`
	AdminURL    string `json:"admin_url"`
	Build       string `json:"build"`
	Version     string `json:"version"`
	PID         int    `json:"pid"`
}

func instanceIdentity(paths []string) (string, error) {
	ordered, err := canonicalLockPaths(paths)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(strings.Join(ordered, "\n")))
	return hex.EncodeToString(sum[:]), nil
}
func localURL(raw, path string) bool {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != path || u.Port() == "" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}

// A mutex closes the gate before observing idle; no Add/Wait race and no
// shutdown timeout that leaves the old listener permanently closed.
type drainGate struct {
	mu       sync.Mutex
	active   int
	draining bool
	idle     chan struct{}
}

func (g *drainGate) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		if g.draining {
			g.mu.Unlock()
			w.Header().Set("Retry-After", "2")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":{"message":"服务正在平滑更新，请稍后重试","type":"service_unavailable","code":"service_updating"}}`))
			return
		}
		g.active++
		g.mu.Unlock()
		defer func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			g.active--
			if g.draining && g.active == 0 {
				close(g.idle)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
func (g *drainGate) drain(ctx context.Context, wait time.Duration) error {
	g.mu.Lock()
	if g.draining {
		g.mu.Unlock()
		return errors.New("已在等待退出")
	}
	g.draining = true
	g.idle = make(chan struct{})
	idle := g.idle
	if g.active == 0 {
		close(idle)
	}
	g.mu.Unlock()
	timer := time.NewTimer(wait)
	defer timer.Stop()
	var err error
	select {
	case <-idle:
		err = ctx.Err()
	case <-ctx.Done():
		err = ctx.Err()
	case <-timer.C:
		err = errors.New("等待已有请求结束超时，旧服务已恢复接收请求")
	}
	if err != nil {
		g.mu.Lock()
		g.draining = false
		g.mu.Unlock()
	}
	return err
}

func startControl(dir, identity, adminURL string, g *drainGate, stop chan<- struct{}) (func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		ln.Close()
		return nil, err
	}
	info := instanceInfo{Application: "work2api", Identity: identity, Control: "http://" + ln.Addr().String() + "/instance", Token: hex.EncodeToString(secret[:]), AdminURL: adminURL, Build: buildFingerprint, Version: version, PID: os.Getpid()}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Host != ln.Addr().String() || r.URL.Path != "/instance" || r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" || !hmac.Equal([]byte(r.Header.Get("Authorization")), []byte("Bearer "+info.Token)) {
			http.Error(w, "forbidden", 403)
			return
		}
		switch r.Method {
		case "GET":
			g.mu.Lock()
			draining := g.draining
			g.mu.Unlock()
			if draining {
				http.Error(w, "服务正在等待退出", 409)
				return
			}
			public := info
			public.Token = ""
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(public)
		case "POST":
			if err := g.drain(r.Context(), 60*time.Second); err != nil {
				http.Error(w, err.Error(), 409)
				return
			}
			w.WriteHeader(202)
			_, _ = w.Write([]byte("drained"))
			_ = http.NewResponseController(w).Flush()
			select {
			case stop <- struct{}{}:
			default:
			}
		default:
			http.Error(w, "method not allowed", 405)
		}
	})
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 3 * time.Second, IdleTimeout: 3 * time.Second}
	path := filepath.Join(dir, ".instance.json")
	raw, _ := json.Marshal(info)
	if err = os.WriteFile(path, raw, 0600); err != nil {
		ln.Close()
		return nil, err
	}
	go func() { _ = srv.Serve(ln) }()
	return func() { _ = srv.Close(); _ = os.Remove(path) }, nil
}

func contactInstance(dir, identity string, stop bool) (instanceInfo, error) {
	var info instanceInfo
	raw, err := os.ReadFile(filepath.Join(dir, ".instance.json"))
	if err != nil {
		return info, err
	}
	if len(raw) > 4096 || json.Unmarshal(raw, &info) != nil || info.Application != "work2api" || info.Identity != identity || len(info.Token) != 64 || !localURL(info.Control, "/instance") || !localURL(info.AdminURL, "/admin-ui/") {
		return info, errors.New("无法确认运行实例与当前数据配置一致")
	}
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	defer client.CloseIdleConnections()
	req, _ := http.NewRequest("GET", info.Control, nil)
	req.Header.Set("Authorization", "Bearer "+info.Token)
	resp, err := client.Do(req)
	if err != nil {
		return info, err
	}
	defer resp.Body.Close()
	var live instanceInfo
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&live) != nil || live.Identity != identity || live.Application != "work2api" || live.PID != info.PID || live.Control != info.Control || live.AdminURL != info.AdminURL || live.Build != info.Build {
		return info, errors.New("运行实例身份校验失败")
	}
	if stop {
		client.Timeout = 65 * time.Second
		req, _ = http.NewRequest("POST", info.Control, nil)
		req.Header.Set("Authorization", "Bearer "+info.Token)
		resp, err = client.Do(req)
		if err != nil {
			return info, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 202 {
			return info, errors.New("旧服务未确认退出，保留原实例；请稍后再试")
		}
	}
	if !stop {
		health, err := client.Get(strings.TrimSuffix(info.AdminURL, "admin-ui/") + "health")
		if err != nil {
			return info, errors.New("已有服务尚未就绪，未启动第二个实例")
		}
		defer health.Body.Close()
		var status struct {
			Status string `json:"status"`
		}
		if health.StatusCode != 200 || json.NewDecoder(io.LimitReader(health.Body, 4096)).Decode(&status) != nil || status.Status != "ok" {
			return info, errors.New("已有服务健康检查未通过")
		}
	}
	return info, nil
}
func openAdmin(raw string) error {
	if !localURL(raw, "/admin-ui/") {
		return errors.New("管理地址无效")
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", raw)
	case "darwin":
		cmd = exec.Command("open", raw)
	default:
		cmd = exec.Command("xdg-open", raw)
	}
	return cmd.Run()
}
