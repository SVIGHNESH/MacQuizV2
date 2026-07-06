package quiz

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/riverqueue/river"
)

// This file owns the scheduler side of the quiz state machine (docs/06
// section 1): the River job argument types shared by the API (which enqueues
// them inside the publish transaction) and the worker (which consumes them),
// plus the due-transition sweep both sides execute.
//
// The jobs carry no state beyond the quiz id: every transition re-derives
// what is due from the row's own window, so a job firing for a rescheduled
// quiz - or twice, or a day late after an outage - is a harmless no-op or a
// repair, never a wrong flip.

// OpenQuizArgs is the scheduler job enqueued at publish to fire at
// starts_at and flip the quiz Scheduled -> Live.
type OpenQuizArgs struct {
	QuizID string `json:"quiz_id"`
}

// Kind names the job type in the queue ("open_quiz job" in docs/06).
func (OpenQuizArgs) Kind() string { return "open_quiz" }

// CloseQuizArgs is the scheduler job enqueued at publish to fire at ends_at
// and flip the quiz Live -> Closed.
type CloseQuizArgs struct {
	QuizID string `json:"quiz_id"`
}

// Kind names the job type in the queue ("close_quiz job" in docs/06).
func (CloseQuizArgs) Kind() string { return "close_quiz" }

// SweepQuizzesArgs is the periodic backstop job: the worker runs the same
// due-transition sweep on an interval, so even a job lost to manual queue
// surgery cannot leave a quiz stuck (docs/02 section 4.6).
type SweepQuizzesArgs struct{}

// Kind names the periodic sweep job type.
func (SweepQuizzesArgs) Kind() string { return "sweep_quizzes" }

// SweepDueQuizzes applies every due lifecycle transition in two idempotent
// statements: scheduled/live quizzes past ends_at close, then scheduled
// quizzes past starts_at (with the window still open) go live. The
// predicates make the sweep safe to run at any time, from any caller, for
// any quiz - the exact-timestamp jobs, the worker's boot re-scan, and the
// periodic backstop all funnel through here.
//
// Closing does not itself touch attempts: the worker runs
// attempt.SweepDueAttempts right after this sweep, so a quiz closed here has
// its open attempts force-submitted in the same pass.
func SweepDueQuizzes(ctx context.Context, db *sql.DB) (opened, closed int64, err error) {
	res, err := db.ExecContext(ctx,
		`UPDATE quizzes SET status = 'closed'
		 WHERE status IN ('scheduled', 'live') AND ends_at <= now()`)
	if err != nil {
		return 0, 0, fmt.Errorf("close due quizzes: %w", err)
	}
	if closed, err = res.RowsAffected(); err != nil {
		return 0, 0, fmt.Errorf("count closed quizzes: %w", err)
	}

	res, err = db.ExecContext(ctx,
		`UPDATE quizzes SET status = 'live'
		 WHERE status = 'scheduled' AND starts_at <= now() AND ends_at > now()`)
	if err != nil {
		return 0, closed, fmt.Errorf("open due quizzes: %w", err)
	}
	if opened, err = res.RowsAffected(); err != nil {
		return 0, closed, fmt.Errorf("count opened quizzes: %w", err)
	}
	return opened, closed, nil
}

// enqueueWindowJobs inserts the open_quiz/close_quiz jobs for the quiz's
// window inside the publish transaction, so a published quiz and its
// transitions commit or roll back together. Jobs from an earlier publish of
// a rescheduled quiz stay in the queue and no-op against the sweep
// predicates when they fire.
func (s *Service) enqueueWindowJobs(ctx context.Context, tx *sql.Tx, quizID string, startsAt, endsAt time.Time) error {
	_, err := s.jobs.InsertManyTx(ctx, tx, []river.InsertManyParams{
		{Args: OpenQuizArgs{QuizID: quizID}, InsertOpts: &river.InsertOpts{ScheduledAt: startsAt}},
		{Args: CloseQuizArgs{QuizID: quizID}, InsertOpts: &river.InsertOpts{ScheduledAt: endsAt}},
	})
	if err != nil {
		return fmt.Errorf("enqueue window jobs: %w", err)
	}
	return nil
}
