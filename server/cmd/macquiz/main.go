// Command macquiz is the single MacQuiz backend binary.
//
// It runs in two modes, matching the two containers of the Compose stack
// (docs/09-deployment.md):
//
//	macquiz serve   - HTTP API + realtime gateway
//	macquiz worker  - River job consumers (scheduler, grading, imports, rollups)
//
// Both modes read the same environment configuration (internal/config).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"macquiz/server/internal/config"
	"macquiz/server/internal/httpserver"
	"macquiz/server/internal/worker"
)

// Set via -ldflags at build time; see the Makefile and Dockerfile.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "macquiz:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: macquiz <serve|worker>")
	}

	cfg := config.Load()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With(
		"version", version, "commit", commit,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch os.Args[1] {
	case "serve":
		return serve(ctx, cfg, log)
	case "worker":
		return worker.Run(ctx, cfg, log)
	default:
		return fmt.Errorf("unknown mode %q (want serve or worker)", os.Args[1])
	}
}

func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: httpserver.New(httpserver.BuildInfo{Version: version, Commit: commit}),
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("api listening", "addr", cfg.Addr, "env", cfg.Env)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Info("shutting down", "grace", cfg.ShutdownGrace)
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
