// Command macquiz is the single MacQuiz backend binary.
//
// It runs in two modes, matching the two containers of the Compose stack
// (docs/09-deployment.md):
//
//	macquiz serve      - HTTP API + realtime gateway
//	macquiz worker     - River job consumers (scheduler, grading, imports, rollups)
//	macquiz migrate    - apply pending schema migrations, then exit
//	macquiz bootstrap  - idempotently create the first admin account, then exit
//
// All modes read the same environment configuration (internal/config).
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

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/config"
	"macquiz/server/internal/db"
	"macquiz/server/internal/httpserver"
	"macquiz/server/internal/quiz"
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
		return errors.New("usage: macquiz <serve|worker|migrate|bootstrap>")
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
	case "migrate":
		return migrate(ctx, cfg, log)
	case "bootstrap":
		return bootstrap(ctx, cfg, log)
	default:
		return fmt.Errorf("unknown mode %q (want serve, worker, migrate, or bootstrap)", os.Args[1])
	}
}

func migrate(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	sqlDB, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	applied, err := db.MigrateUp(ctx, sqlDB)
	if err != nil {
		return err
	}
	log.Info("migrations applied", "count", applied)
	return nil
}

// bootstrap creates the first admin account from MACQUIZ_BOOTSTRAP_ADMIN_*
// and exits. Safe to run on every deploy: it is a no-op once any admin exists.
func bootstrap(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	if cfg.BootstrapAdminEmail == "" || cfg.BootstrapAdminPassword == "" {
		return errors.New("bootstrap requires MACQUIZ_BOOTSTRAP_ADMIN_EMAIL and MACQUIZ_BOOTSTRAP_ADMIN_PASSWORD")
	}
	sqlDB, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	svc := authusers.NewService(sqlDB, cfg.AuthSecret, log)
	return svc.EnsureBootstrapAdmin(ctx,
		cfg.BootstrapAdminEmail, cfg.BootstrapAdminPassword, cfg.BootstrapAdminName)
}

func serve(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	sqlDB, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	authSvc := authusers.NewService(sqlDB, cfg.AuthSecret, log)
	authHandler := authusers.NewHandler(authSvc, cfg.Env == "production")
	quizSvc := quiz.NewService(sqlDB, log)
	quizHandler := quiz.NewHandler(quizSvc, authSvc)

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: httpserver.New(
			httpserver.BuildInfo{Version: version, Commit: commit},
			httpserver.Deps{DB: sqlDB, Auth: authHandler, Quiz: quizHandler},
		),
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
