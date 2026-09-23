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
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}

	go func() {
		fmt.Printf("work2api %s listening on http://%s\n", version, addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
