// Command server is the work2api entrypoint: it loads configuration, builds the
// orchestrator, starts the HTTP server, and shuts down gracefully.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"work2api/internal/app"
	"work2api/internal/config"
)

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cfg := config.Load(os.Args[1:])

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatalf("create data dir: %v", err)
	}

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
	ln, addr, err := listen(cfg.Host, cfg.Port, cfg.AllowPortFallback)
	if err != nil {
		log.Fatalf("%v", err)
	}
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		fmt.Printf("work2api %s listening on http://%s\n", version, addr)
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

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
