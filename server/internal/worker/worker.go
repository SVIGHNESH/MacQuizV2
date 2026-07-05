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
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverdatabasesql"

	"macquiz/server/internal/config"
	"macquiz/server/internal/db"
	"macquiz/server/internal/quiz"
)

// sweepInterval paces the periodic backstop sweep. The exact-timestamp jobs
// do the real work; the sweep only repairs a queue that was tampered with or
// drained, so a minute of staleness is fine - readers lazily derive the live
// status anyway (quiz.effectiveStatus).
const sweepInterval = time.Minute

// openQuizWorker fires at starts_at and flips Scheduled -> Live. It runs the
// shared sweep rather than a per-quiz statement: the predicates make that
// idempotent, and a late job repairs every overdue quiz, not just its own.
type openQuizWorker struct {
	river.WorkerDefaults[quiz.OpenQuizArgs]
	db  *sql.DB
	log *slog.Logger
}

func (w *openQuizWorker) Work(ctx context.Context, job *river.Job[quiz.OpenQuizArgs]) error {
	return sweepDue(ctx, w.db, w.log, "open_quiz job", job.Args.QuizID)
}

// closeQuizWorker fires at ends_at and flips Scheduled/Live -> Closed.
// Force-submitting open attempts hangs off this transition in Milestone 4.
type closeQuizWorker struct {
	river.WorkerDefaults[quiz.CloseQuizArgs]
	db  *sql.DB
	log *slog.Logger
}

func (w *closeQuizWorker) Work(ctx context.Context, job *river.Job[quiz.CloseQuizArgs]) error {
	return sweepDue(ctx, w.db, w.log, "close_quiz job", job.Args.QuizID)
}

// sweepQuizzesWorker is the periodic backstop behind the exact-timestamp
// jobs (docs/02 section 4.6).
type sweepQuizzesWorker struct {
	river.WorkerDefaults[quiz.SweepQuizzesArgs]
	db  *sql.DB
	log *slog.Logger
}

func (w *sweepQuizzesWorker) Work(ctx context.Context, _ *river.Job[quiz.SweepQuizzesArgs]) error {
	return sweepDue(ctx, w.db, w.log, "periodic sweep", "")
}

func sweepDue(ctx context.Context, sqlDB *sql.DB, log *slog.Logger, trigger, quizID string) error {
	opened, closed, err := quiz.SweepDueQuizzes(ctx, sqlDB)
	if err != nil {
		return err
	}
	if opened > 0 || closed > 0 {
		log.Info("quiz transitions applied",
			"trigger", trigger, "quiz_id", quizID, "opened", opened, "closed", closed)
	}
	return nil
}

// Run starts the worker loop and blocks until ctx is cancelled.
func Run(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	sqlDB, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	workers := river.NewWorkers()
	river.AddWorker(workers, &openQuizWorker{db: sqlDB, log: log})
	river.AddWorker(workers, &closeQuizWorker{db: sqlDB, log: log})
	river.AddWorker(workers, &sweepQuizzesWorker{db: sqlDB, log: log})

	client, err := river.NewClient(riverdatabasesql.New(sqlDB), &river.Config{
		Logger:  log,
		Workers: workers,
		Queues: map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: 10},
		},
		PeriodicJobs: []*river.PeriodicJob{
			river.NewPeriodicJob(
				river.PeriodicInterval(sweepInterval),
				func() (river.JobArgs, *river.InsertOpts) {
					return quiz.SweepQuizzesArgs{}, nil
				},
				nil,
			),
		},
	})
	if err != nil {
		return fmt.Errorf("new river client: %w", err)
	}

	// Boot re-scan: apply every transition that came due while no worker was
	// running, before the queue starts, so a restart heals the world first.
	if err := sweepDue(ctx, sqlDB, log, "boot re-scan", ""); err != nil {
		return fmt.Errorf("boot re-scan: %w", err)
	}

	if err := client.Start(ctx); err != nil {
		return fmt.Errorf("start river client: %w", err)
	}
	log.Info("worker started", "env", cfg.Env, "sweep_interval", sweepInterval)

	<-ctx.Done()
	stopCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	if err := client.Stop(stopCtx); err != nil {
		return fmt.Errorf("stop river client: %w", err)
	}
	log.Info("worker stopped")
	return nil
}
