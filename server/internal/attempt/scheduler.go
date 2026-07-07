package attempt

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/riverqueue/river"
)

// This file owns the scheduler side of the attempt lifecycle (docs/06
// section 2): the per-attempt deadline timer job the start transaction
// enqueues, and the due-attempt sweep the worker runs to terminate attempts
// whose time is up - "the disappearing student" auto-submit and the
// force-submit on quiz close.
//
// Like the quiz scheduler (internal/quiz/scheduler.go), the job carries no
// state beyond an id: the sweep re-derives what is due from the rows
// themselves, so a job firing twice, late, or for an already-submitted
// attempt is a harmless no-op, never a wrong flip.

// DeadlineArgs is the per-attempt timer job enqueued inside the start
// transaction to fire once the attempt's deadline (plus the autosave grace)
// has passed and auto-submit whatever was autosaved (docs/06 section 2:
// "a per-attempt timer job fires server-side at deadline_at").
type DeadlineArgs struct {
	AttemptID string `json:"attempt_id"`
}

// Kind names the job type in the queue.
func (DeadlineArgs) Kind() string { return "attempt_deadline" }

// SweepDueAttempts terminates every attempt whose time is up, in two
// idempotent statements sharing the funnel's status = 'in_progress' guard:
//
//  1. auto: attempts past their own deadline_at plus the autosave grace -
//     the personal budget expired, so the timer semantics apply even when a
//     late job or the periodic backstop does the flipping.
//  2. forced: attempts still open on a closed (or archived) quiz - the quiz
//     window ended before the personal deadline did (docs/06 section 1:
//     "close_quiz job force-submits all open attempts, kind='forced'").
//
// Auto runs first so an attempt that expired before its quiz closed keeps
// the kind its own timer would have written. The predicates make the sweep
// safe to run at any time, from any caller: the exact-timestamp deadline
// jobs, the close_quiz transition, the worker's boot re-scan, and the
// periodic backstop all funnel through here.
func SweepDueAttempts(ctx context.Context, db *sql.DB) (auto, forced int64, err error) {
	// The whole sweep runs in one transaction so each terminated attempt and
	// its submitted event (docs/05: persist first) commit together. Auto still
	// runs before forced within the transaction, so an attempt that expired
	// before its quiz closed keeps the kind its own timer would have written.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin sweep tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	auto, err = sweepSubmit(ctx, tx, "auto",
		`UPDATE attempts
		 SET status = 'submitted', submit_kind = 'auto', submitted_at = now()
		 WHERE status = 'in_progress' AND deadline_at + $1::interval <= now()
		 RETURNING id, (SELECT count(*) FROM attempt_answers aa
		                WHERE aa.attempt_id = attempts.id AND aa.response IS NOT NULL)`,
		writeGrace.String())
	if err != nil {
		return 0, 0, err
	}

	forced, err = sweepSubmit(ctx, tx, "forced",
		`UPDATE attempts a
		 SET status = 'submitted', submit_kind = 'forced', submitted_at = now()
		 FROM quizzes z
		 WHERE a.quiz_id = z.id AND a.status = 'in_progress'
		   AND z.status IN ('closed', 'archived')
		 RETURNING a.id, (SELECT count(*) FROM attempt_answers aa
		                  WHERE aa.attempt_id = a.id AND aa.response IS NOT NULL)`)
	if err != nil {
		return 0, 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit sweep: %w", err)
	}
	return auto, forced, nil
}

// sweepSubmit runs one set-based termination UPDATE ... RETURNING and appends a
// submitted event for every row it flipped, all in the caller's transaction.
// The status = 'in_progress' predicate means only rows this pass actually
// terminated are returned, so a late job, the boot re-scan, or the periodic
// backstop re-emit nothing - the events inherit the sweep's idempotence for
// free. RETURNING rows are drained fully before the first appendEvent because a
// transaction cannot interleave a new statement with an open result set.
func sweepSubmit(ctx context.Context, tx *sql.Tx, kind, query string, args ...any) (int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("%s-submit due attempts: %w", kind, err)
	}
	type flipped struct {
		id       string
		answered int
	}
	var swept []flipped
	for rows.Next() {
		var f flipped
		if err := rows.Scan(&f.id, &f.answered); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan %s-submitted attempt: %w", kind, err)
		}
		swept = append(swept, f)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("%s-submit due attempts: %w", kind, err)
	}
	rows.Close()

	for _, f := range swept {
		if err := appendEvent(ctx, tx, f.id, eventSubmitted, submittedPayload{
			SubmitKind: kind, AnsweredCount: f.answered,
		}); err != nil {
			return 0, err
		}
	}
	return int64(len(swept)), nil
}

// enqueueDeadlineJob inserts the attempt's timer job inside the start
// transaction, so an attempt is never created without its auto-submit and a
// failed start leaves no orphan job. It fires at deadline_at plus the
// autosave grace: the write gate already refuses everything after that
// moment, so the last in-flight autosave lands before the terminal flip.
func (s *Service) enqueueDeadlineJob(ctx context.Context, tx *sql.Tx, attemptID string, deadlineAt time.Time) error {
	_, err := s.jobs.InsertTx(ctx, tx, DeadlineArgs{AttemptID: attemptID},
		&river.InsertOpts{ScheduledAt: deadlineAt.Add(writeGrace)})
	if err != nil {
		return fmt.Errorf("enqueue deadline job: %w", err)
	}
	return nil
}
