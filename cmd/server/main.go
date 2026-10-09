// Command server is the work2api entrypoint: it loads configuration, builds the
// orchestrator, starts the HTTP server, and shuts down gracefully.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"work2api/internal/app"
	"work2api/internal/config"
	"work2api/internal/qoder/account"
	"work2api/internal/workbuddy/credentials"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	opt, args := launchArgs(os.Args[1:])
	if opt.info {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"application": "work2api", "build": buildFingerprint, "version": version})
		return
	}
	cfg := config.Load(args)

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		log.Fatalf("create data dir: %v", err)
	}
	if resolved, err := filepath.EvalSymlinks(cfg.DBPath); err == nil {
		cfg.DBPath = resolved
	}
	paths := []string{filepath.Join(cfg.DataDir, ".instance.lock"), cfg.DBPath + ".instance.lock"}
	paths = append(paths, filepath.Join(account.DataRoot(), ".instance.lock"))
	for _, dir := range credentials.AuthDirs("", filepath.Join(config.PackageRoot, "auths")) {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			paths = append(paths, filepath.Join(dir, ".instance.lock"))
		}
	}
	// The project credential directory must be reserved even before its creation.
	paths = append(paths, filepath.Join(config.PackageRoot, "auths", ".instance.lock"))
	identity, err := instanceIdentity(paths)
	if err != nil {
		log.Fatal(err)
	}
	release, err := lockInstance(paths)
	if err != nil {
		info, probeErr := contactInstance(cfg.DataDir, identity, opt.stop)
		if probeErr == nil {
			if opt.stop {
				fmt.Println("已有调用已结束，旧服务正在退出")
				return
			}
			fmt.Printf("服务已运行 (%s)，复用管理面板: %s\n", info.Version, info.AdminURL)
			if opt.open {
				if err := openAdmin(info.AdminURL); err != nil {
					log.Printf("打开浏览器失败: %v", err)
				}
			}
			return
		}
		log.Fatalf("init: %v；无法安全复用：%v", err, probeErr)
	}
	defer release()
	if opt.reuse || opt.stop {
		log.Fatal("没有可复用的运行实例，或旧实例不支持安全退出；未启动新服务")
	}
	ln, addr, err := listen(cfg.Host, cfg.Port, cfg.AllowPortFallback)
	if err != nil {
		log.Fatalf("%v", err)
	}
	defer ln.Close()

	orch, err := app.New(cfg)
	if err != nil {
		log.Fatalf("init: %v", err)
	}

	// Start background scheduler (checkin / credit refresh / model refresh /
	// token keepalive / usage cleanup). Non-blocking: warming runs inside the
	// loop so HTTP startup is immediate.
	sched := app.NewScheduler(orch)
	sched.Start()

	srv := app.NewServer(orch)
	gate := &drainGate{}
	controlStop := make(chan struct{}, 1)
	httpSrv := &http.Server{
		Handler:           gate.wrap(srv.Handler()),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		fmt.Printf("work2api %s listening on http://%s\n", version, addr)
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()
	host, port, _ := net.SplitHostPort(addr)
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	adminURL := "http://" + net.JoinHostPort(host, port) + "/admin-ui/"
	closeControl, err := startControl(cfg.DataDir, identity, adminURL, gate, controlStop)
	if err != nil {
		log.Fatalf("本地实例控制初始化失败: %v", err)
	}
	defer closeControl()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	if opt.open {
		if err := openAdmin(adminURL); err != nil {
			log.Printf("打开浏览器失败: %v", err)
		}
	}

	select {
	case <-stop:
	case <-controlStop:
	}

	fmt.Println("shutting down...")
	sched.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

// listen binds the HTTP listener. When the port is left at the default 8787
// and is occupied, it auto-avoids forward up to maxBumps times (8788, 8789,
// 8790), announcing each bump so clients aren't left guessing. A port the
// operator set explicitly (anything other than 8787) is never auto-avoided:
// if it is occupied we fail with an actionable hint, respecting their choice.
func listen(host string, port int, fallback ...bool) (net.Listener, string, error) {
	const defaultPort = 8787
	const maxBumps = 3
	autoAvoid := port == defaultPort && (len(fallback) == 0 || fallback[0])
	attempts := 1
	if autoAvoid {
		attempts += maxBumps
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		p := port + i
		addr := net.JoinHostPort(host, strconv.Itoa(p))
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			if i > 0 {
				fmt.Printf("端口 %d 被占用，已自动避让到 %d（第 %d/%d 次）。如需固定端口：-port <端口> 或在 .env 设 PORT=<端口>。\n", port, p, i, maxBumps)
			}
			return ln, addr, nil
		}
		if !isAddrInUse(err) {
			return nil, "", fmt.Errorf("监听 %s 失败：%w", addr, err)
		}
		lastErr = err
	}
	if autoAvoid {
		return nil, "", fmt.Errorf("端口 %d–%d 连续 %d 次尝试均被占用。换端口：-port <端口>，或在 .env 设 PORT=<端口>。最后错误：%w", port, port+maxBumps, attempts, lastErr)
	}
	return nil, "", fmt.Errorf("端口 %d 已被占用（你已显式指定该端口，不做自动避让）。换端口：-port <端口>，或在 .env 设 PORT=<端口>。原始错误：%w", port, lastErr)
}

// isAddrInUse reports whether err is an "address already in use" bind failure
// (EADDRINUSE on unix, WSAEADDRINUSE/10048 on Windows).
func isAddrInUse(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == syscall.EADDRINUSE || errno == 10048
	}
	return false
}
