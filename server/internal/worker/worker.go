// Package worker is the job-processing side of the binary: River consumers
// for scheduler transitions (open/close quiz), per-attempt deadline timers,
// grading, bulk imports, and analytics rollups.
//
// It owns no HTTP surface. At boot it re-scans Postgres for due-but-unfired
// state transitions (the lazy-state-validation backstop from
// docs/02-architecture.md section 4.6), so a queue outage can never leave a
// quiz stuck.
package worker

import (
	"context"
	"log/slog"

	"macquiz/server/internal/config"
)

// Run starts the worker loop and blocks until ctx is cancelled.
//
// River wiring lands in Milestone 3 (scheduling); until then the worker is a
// well-behaved no-op so the Compose stack and deploy pipeline can be exercised
// end to end.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	log.Info("worker started", "env", cfg.Env)
	<-ctx.Done()
	log.Info("worker stopped")
	return nil
}
