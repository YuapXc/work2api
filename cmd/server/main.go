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

	"runtime"
	"sync"
	"work2api/internal/app"
	"work2api/internal/config"
	"work2api/internal/selfupdate"
	"work2api/internal/store"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

// Only the official release workflow sets this marker. A tag-shaped version
// alone never turns a source build into a replaceable release deployment.
var releaseBuild = "false"
var releaseRepository = "YuapXc/work2api"
var releaseCommit = ""

func currentBuild() selfupdate.Build {
	return selfupdate.Build{Application: "work2api", Version: version, Release: releaseBuild == "true", Repository: releaseRepository, Commit: releaseCommit, Schema: store.SchemaVersion, OS: runtime.GOOS, Arch: runtime.GOARCH, Build: buildFingerprint}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-apply-update" {
		if len(os.Args) != 3 {
			log.Fatal("更新助手需要本地事务文件")
		}
		if err := applyUpdate(os.Args[2]); err != nil {
			log.Fatal(err)
		}
		return
	}

	opt, args := launchArgs(os.Args[1:])
	if opt.info {
		_ = json.NewEncoder(os.Stdout).Encode(currentBuild())
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
	paths = append(paths, app.ProviderStateLockPaths()...)
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
	bootPending, bootUpdating := pendingStartup(cfg.DataDir, currentBuild())
	if bootUpdating && sameExistingPath(bootPending.DataDir, cfg.DataDir) && sameExistingPath(bootPending.DBPath, cfg.DBPath) && bootPending.BindPort > 0 && bootPending.BindPort < 65536 {
		cfg.Host = bootPending.BindHost
		cfg.Port = bootPending.BindPort
		cfg.AllowPortFallback = false
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
	defer orch.Close()

	// Start background scheduler (checkin / credit refresh / model refresh /
	// token keepalive / usage cleanup). Non-blocking: warming runs inside the
	// loop so HTTP startup is immediate.
	sched := app.NewScheduler(orch)
	// A candidate is kept closed to public writes until its updater verifies it.
	pending, updating := bootPending, bootUpdating
	if !updating {
		sched.Start()
	}
	srv := app.NewServer(orch)
	defer srv.CloseUsageExports()
	gate := &drainGate{draining: updating}
	executable, _ := os.Executable()
	executable, _ = filepath.EvalSymlinks(executable)
	deployment := updateDeployment(executable, currentBuild())
	updater := selfupdate.New(filepath.Join(cfg.DataDir, ".updates"), executable, currentBuild(), deployment, func(path string) error { return launchUpdateHelper(path, cfg.AdminToken) }, func() selfupdate.Plan {
		cwd, _ := os.Getwd()
		_, actualPort, _ := net.SplitHostPort(addr)
		portNumber, _ := strconv.Atoi(actualPort)
		canonicalArgs := []string{"-host", cfg.Host, "-port", actualPort, "-strict-port", "-data-dir", cfg.DataDir, "-db", cfg.DBPath, "-log-level", cfg.LogLevel}
		return selfupdate.Plan{BindHost: cfg.Host, BindPort: portNumber, DataDir: cfg.DataDir, DBPath: cfg.DBPath, CWD: cwd, Args: canonicalArgs, Identity: identity}
	})
	srv.SetUpdater(updater)
	updateCtx, cancelUpdates := context.WithCancel(context.Background())
	defer cancelUpdates()
	if !updating {
		go updater.Run(updateCtx)
	}
	var updateMu sync.Mutex
	maintenancePaused := false
	hooks := updateHooks{
		keepAliveOnUpdateStop: deployment.Kind == "systemd",
		resume: func(token string) error {
			updateMu.Lock()
			defer updateMu.Unlock()
			if updating {
				return errors.New("候选版本不可按旧实例恢复")
			}
			if err := updater.ValidateOriginal(token); err != nil {
				return err
			}
			if maintenancePaused {
				sched = app.NewScheduler(orch)
				sched.Start()
				maintenancePaused = false
			}
			gate.resume()
			return nil
		},
		abort: func(token string) error {
			updateMu.Lock()
			defer updateMu.Unlock()
			if !updating || token != pending.Token {
				return errors.New("无效的更新中止事务")
			}
			return nil
		},
		prepare: func(ctx context.Context, token string) error {
			updateMu.Lock()
			defer updateMu.Unlock()
			if err := sched.StopAndWait(ctx); err != nil {
				return err
			}
			maintenancePaused = true
			err := updater.Prepare(ctx, token, orch.UpdateBackup)
			if err != nil {
				sched = app.NewScheduler(orch)
				sched.Start()
				maintenancePaused = false
			}
			return err
		},
		readiness: func(ctx context.Context, token string) (any, error) {
			updateMu.Lock()
			defer updateMu.Unlock()
			if !updating || token != pending.Token {
				return nil, errors.New("无效的更新验证事务")
			}
			if err := orch.UpdateReady(ctx); err != nil {
				return nil, err
			}
			return currentBuild(), nil
		},
		accept: func(token string) error {
			updateMu.Lock()
			defer updateMu.Unlock()
			if !updating || token != pending.Token {
				return errors.New("无效的更新验证事务")
			}
			pending.Phase = "complete"
			if err := selfupdate.WriteJSON(filepath.Join(cfg.DataDir, ".updates", "pending.json"), pending); err != nil {
				return err
			}
			updating = false
			gate.resume()
			sched.Start()
			go updater.Run(updateCtx)
			return nil
		},
	}

	controlStop := make(chan struct{}, 1)
	httpSrv := &http.Server{
		Handler:           gate.wrap(srv.Handler()),
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    64 << 10,
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
	closeControl, err := startControl(cfg.DataDir, identity, adminURL, gate, controlStop, hooks)
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
	updateMu.Lock()
	stoppingScheduler := sched
	updateMu.Unlock()
	stoppingScheduler.Stop()
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
