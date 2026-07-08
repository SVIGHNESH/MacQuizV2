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

	"macquiz/server/internal/analytics"
	"macquiz/server/internal/attempt"
	"macquiz/server/internal/authusers"
	"macquiz/server/internal/config"
	"macquiz/server/internal/db"
	"macquiz/server/internal/httpserver"
	"macquiz/server/internal/quiz"
	"macquiz/server/internal/realtime"
	"macquiz/server/internal/telemetry"
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

	// Grafana Cloud metrics export (docs/10-operations.md section 2). With no
	// MACQUIZ_OTEL_EXPORTER_ENDPOINT set (dev/test), every instrument below is
	// a no-op and this never dials out.
	tel, err := telemetry.Setup(ctx, cfg, "macquiz-api")
	if err != nil {
		return fmt.Errorf("set up telemetry: %w", err)
	}
	defer tel.Shutdown(context.Background())
	if err := tel.RegisterQueueLagGauge(func(ctx context.Context) (float64, error) {
		return httpserver.QueueLagSeconds(ctx, sqlDB)
	}); err != nil {
		return fmt.Errorf("register queue lag gauge: %w", err)
	}

	// Production's Compose stack (docs/09-deployment.md section 4) has no
	// standalone migrate service - only the dev stack does. So the app
	// entrypoint must apply pending migrations itself before accepting
	// traffic (docs/09 section 5, docs/10 section 6: "the app refuses to
	// start if migrations fail"). Idempotent and safe alongside the dev
	// compose's separate migrate one-shot: MigrateUp is a no-op at head, and
	// newProvider's Postgres session lock serializes it against any other
	// migrator racing at the same time.
	applied, err := db.MigrateUp(ctx, sqlDB)
	if err != nil {
		return fmt.Errorf("apply migrations: %w", err)
	}
	log.Info("migrations applied", "count", applied)

	// The realtime relay is the "publish second" half of docs/05: the attempt
	// service persists each event, then hands it here to fan out over Redis.
	// A bad URL fails boot; an unreachable Redis does not - publishes degrade
	// to best-effort, time-bounded drops (see realtime.publishTimeout) so a
	// Redis outage cannot stall the REST write path (docs/05 section 5).
	publisher, err := realtime.NewPublisher(cfg.RedisURL, log)
	if err != nil {
		return err
	}
	defer publisher.Close()

	// The subscribe side of docs/05: the gateway consumes the same
	// quiz:{id}:events channel the publisher writes to and fans each event out
	// to the authorized monitor sockets. A separate client from the publisher
	// (its sub-second timeouts are wrong for a blocking pub/sub receive).
	subscriber, err := realtime.NewRedisSubscriber(cfg.RedisURL)
	if err != nil {
		return err
	}
	defer subscriber.Close()

	// The register-import endpoint (docs/04-api.md: POST /quizzes/:id/imports)
	// writes uploaded files here; the worker process reads them back through
	// its own LocalImportStorage pointed at the same directory (docs/09
	// section 4: single-VM deployment shares one disk across containers).
	if err := os.MkdirAll(cfg.ImportDir, 0o755); err != nil {
		return fmt.Errorf("create import dir: %w", err)
	}

	authSvc := authusers.NewService(sqlDB, cfg.AuthSecret, log)
	authHandler := authusers.NewHandler(authSvc, cfg.Env == "production")
	quizSvc := quiz.NewService(sqlDB, log, quiz.LocalImportStorage{Dir: cfg.ImportDir})
	quizHandler := quiz.NewHandler(quizSvc, authSvc)
	// Coalesce serve-side attempt.progress to at most one relay per 2 s per
	// attempt (docs/05 section 5): every autosave still persists its event row,
	// only the best-effort Redis relay is thinned to the doc's events/s budget.
	// The worker emits no progress, so its publisher stays unwrapped.
	attemptSvc := attempt.NewService(sqlDB, log, attempt.NewProgressCoalescer(publisher))
	attemptHandler := attempt.NewHandler(attemptSvc, authSvc)
	attemptHandler.SetMetrics(tel.Metrics)
	analyticsHandler := analytics.NewHandler(analytics.NewService(sqlDB, log), authSvc)

	// The gateway's socket lifetime is bound to ctx (the SIGTERM signal
	// context): when the process is asked to stop, every open monitor socket's
	// pump returns, so graceful shutdown does not wait out the Timeout grace.
	gateway := realtime.NewGateway(ctx, subscriber, authSvc, quizSvc.OwnerOf, cfg.WSAllowedOrigins, log)
	gateway.SetMetrics(tel.Metrics)

	srv := &http.Server{
		Addr: cfg.Addr,
		Handler: httpserver.New(
			httpserver.BuildInfo{Version: version, Commit: commit},
			httpserver.Deps{DB: sqlDB, Redis: publisher, Auth: authHandler, Quiz: quizHandler, Attempt: attemptHandler, Analytics: analyticsHandler, Realtime: gateway},
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
