package quiz

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"macquiz/server/internal/authusers"
)

// This file owns the live-roster snapshot half of Milestone 5 (docs/05
// section 4). On connect the teacher dashboard first fetches GET
// /quizzes/:id/live - the current roster materialized from attempts - then
// applies streamed deltas, so late joins and reconnects stay consistent.
// This is the pure REST read; the WebSocket gateway, Redis pub/sub, and
// attempt_events fan-out are separate bricks that layer on top of it.

// LiveRow is one roster cell: exactly one assigned student and their latest
// attempt (docs/05 section 6 - a student is in exactly one roster state, so
// the snapshot collapses to max(attempt_no) rather than fanning out one row
// per attempt the way the results table does). Students who never started
// keep the attempt fields null and read as "not_started".
type LiveRow struct {
	StudentID string `json:"student_id"`
	FullName  string `json:"full_name"`
	Email     string `json:"email"`
	// State is the dashboard roster state derived from the attempt status:
	// not_started, in_progress, disconnected, submitted (covers graded), or
	// kicked. disconnected is in_progress plus one wrinkle: the attempt's most
	// recent attempt.disconnected/attempt.reconnected event (docs/05 section
	// 4's "materialized from attempts plus recent attempt_events") says the
	// heartbeat lapsed and nothing has cleared it since.
	State string `json:"state"`

	AttemptID  *string    `json:"attempt_id"`
	AttemptNo  *int       `json:"attempt_no"`
	Status     *string    `json:"status"`
	SubmitKind *string    `json:"submit_kind"`
	StartedAt  *time.Time `json:"started_at"`
	DeadlineAt *time.Time `json:"deadline_at"`
	// AnsweredCount is how many of the attempt's questions have a non-null
	// saved response; the client pairs it with QuestionCount for a progress
	// bar. CurrentQuestion is the 1-based ordinal of the last question the
	// student saved an answer for (attempts.current_question), the same
	// cursor the attempt.progress delta carries.
	AnsweredCount   *int     `json:"answered_count"`
	CurrentQuestion *int     `json:"current_question"`
	QuestionCount   *int     `json:"question_count"`
	ViolationCount  *int     `json:"violation_count"`
	Score           *float64 `json:"score"`
	MaxScore        *float64 `json:"max_score"`
}

// LiveRoster returns the owner-or-admin live view of a quiz. Authorization is
// ActionQuizWatchLive (docs/05 section 3: the owning teacher or any admin),
// so an admin can open the dashboard for a quiz they do not own. ServerTime
// is the database clock the row timestamps were read against, so the client
// computes every countdown and elapsed clock from one consistent origin.
func (s *Service) LiveRoster(ctx context.Context, actor authusers.User, quizID string) (Quiz, []LiveRow, time.Time, error) {
	q, err := scanQuiz(s.db.QueryRowContext(ctx,
		`SELECT `+quizColumns+` FROM quizzes WHERE id = $1`, quizID).Scan)
	if err == sql.ErrNoRows {
		return Quiz{}, nil, time.Time{}, ErrNotFound
	}
	if err != nil {
		return Quiz{}, nil, time.Time{}, fmt.Errorf("load quiz: %w", err)
	}
	if !authusers.Can(actor, authusers.ActionQuizWatchLive, authusers.Resource{OwnerID: q.OwnerID}) {
		return Quiz{}, nil, time.Time{}, ErrNotFound
	}

	var now time.Time
	if err := s.db.QueryRowContext(ctx, `SELECT now()`).Scan(&now); err != nil {
		return Quiz{}, nil, time.Time{}, fmt.Errorf("read database clock: %w", err)
	}
	q.Status = effectiveStatus(q.Status, q.StartsAt, q.EndsAt, now)

	// One row per assigned student via a LATERAL join to their latest
	// attempt; answered_count and the pinned max score are per-attempt
	// subqueries so a student mid-attempt scores against the snapshot they
	// actually pinned, matching the results table's per-version accounting.
	rows, err := s.db.QueryContext(ctx,
		`SELECT u.id, u.full_name, u.email,
		        a.id, a.attempt_no, a.status, a.submit_kind, a.started_at,
		        a.deadline_at, a.violation_count, a.score,
		        (SELECT count(*) FROM attempt_answers aa
		         WHERE aa.attempt_id = a.id AND aa.response IS NOT NULL),
		        a.current_question,
		        jsonb_array_length(v.questions),
		        (SELECT sum((qq->>'points')::float8)
		         FROM jsonb_array_elements(v.questions) qq),
		        ce.type
		 FROM quiz_assignments s
		 JOIN users u ON u.id = s.student_id
		 LEFT JOIN LATERAL (
		     SELECT a2.* FROM attempts a2
		     WHERE a2.quiz_id = s.quiz_id AND a2.student_id = s.student_id
		     ORDER BY a2.attempt_no DESC
		     LIMIT 1
		 ) a ON true
		 LEFT JOIN quiz_versions v ON v.quiz_id = a.quiz_id AND v.version = a.quiz_version
		 LEFT JOIN LATERAL (
		     SELECT type FROM attempt_events
		     WHERE attempt_id = a.id AND type IN ('attempt.disconnected', 'attempt.reconnected')
		     ORDER BY id DESC
		     LIMIT 1
		 ) ce ON true
		 WHERE s.quiz_id = $1
		 ORDER BY u.full_name, u.id`, quizID)
	if err != nil {
		return Quiz{}, nil, time.Time{}, fmt.Errorf("list live roster: %w", err)
	}
	defer rows.Close()

	roster := []LiveRow{}
	for rows.Next() {
		var r LiveRow
		var lastConnEvent *string
		if err := rows.Scan(&r.StudentID, &r.FullName, &r.Email,
			&r.AttemptID, &r.AttemptNo, &r.Status, &r.SubmitKind, &r.StartedAt,
			&r.DeadlineAt, &r.ViolationCount, &r.Score, &r.AnsweredCount,
			&r.CurrentQuestion, &r.QuestionCount, &r.MaxScore, &lastConnEvent); err != nil {
			return Quiz{}, nil, time.Time{}, fmt.Errorf("scan live row: %w", err)
		}
		disconnected := lastConnEvent != nil && *lastConnEvent == "attempt.disconnected"
		r.State = rosterState(r.Status, r.SubmitKind, disconnected)
		roster = append(roster, r)
	}
	return q, roster, now, rows.Err()
}

// OwnerOf resolves a quiz's owning teacher for the realtime gateway's
// subscribe-time authorization (docs/05 section 3). It is deliberately
// narrower than LiveRoster - the gateway needs only the owner id to run the
// same ActionQuizWatchLive Can() check, not the whole roster. found is false
// for an unknown quiz, which the gateway answers as 404 so existence is never
// leaked to a non-owner.
func (s *Service) OwnerOf(ctx context.Context, quizID string) (ownerID string, found bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT owner_id FROM quizzes WHERE id = $1`, quizID).Scan(&ownerID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("load quiz owner: %w", err)
	}
	return ownerID, true, nil
}

// rosterState collapses the attempt status enum to the dashboard roster
// vocabulary (docs/05 section 6). A null status is a student who never
// started; graded folds into submitted because the dashboard cell is the
// same ("submitted", score shown per policy) once work stops. A kick is
// permanent: it is read off submit_kind, not status, because a kicked
// attempt's work is still graded (its status advances to 'graded'), so only
// submit_kind = 'kicked' survives to mark the removal forever. disconnected
// only ever applies on top of in_progress (docs/05 section 5: "the clock
// keeps running") - a submitted, graded, or kicked attempt's last
// connectivity event is stale history, not a live state.
func rosterState(status, submitKind *string, disconnected bool) string {
	if status == nil {
		return "not_started"
	}
	if submitKind != nil && *submitKind == "kicked" {
		return "kicked"
	}
	switch *status {
	case "in_progress":
		if disconnected {
			return "disconnected"
		}
		return "in_progress"
	case "submitted", "graded":
		return "submitted"
	case "kicked":
		return "kicked"
	}
	return "not_started"
}
