package quiz

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"macquiz/server/internal/apischema"
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
// keep the attempt fields null and read as "not_started". It is a direct
// alias to the generated apischema.LiveRosterRow type (api/openapi.yaml,
// oapi-codegen - see internal/apischema), so this response can never
// silently drift from the spec.
type LiveRow = apischema.LiveRosterRow

// liveFloat32 narrows a nullable float64 score/max-score column to the
// *float32 the generated wire type expects. These are display-only scores
// (docs/07 section 3), never a financial computation, so float32's ~7
// significant digits lose nothing that matters on the wire.
func liveFloat32(v sql.NullFloat64) *float32 {
	if !v.Valid {
		return nil
	}
	f := float32(v.Float64)
	return &f
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
		var studentID, fullName, email string
		var attemptID, status, submitKind sql.NullString
		var attemptNo, answeredCount, currentQuestion, questionCount, violationCount sql.NullInt64
		var startedAt, deadlineAt sql.NullTime
		var score, maxScore sql.NullFloat64
		var lastConnEvent *string
		if err := rows.Scan(&studentID, &fullName, &email,
			&attemptID, &attemptNo, &status, &submitKind, &startedAt,
			&deadlineAt, &violationCount, &score, &answeredCount,
			&currentQuestion, &questionCount, &maxScore, &lastConnEvent); err != nil {
			return Quiz{}, nil, time.Time{}, fmt.Errorf("scan live row: %w", err)
		}
		studentUUID, err := uuid.Parse(studentID)
		if err != nil {
			return Quiz{}, nil, time.Time{}, fmt.Errorf("parse student id: %w", err)
		}
		r := LiveRow{
			StudentId: studentUUID,
			FullName:  fullName,
			Email:     openapi_types.Email(email),
			Score:     liveFloat32(score),
			MaxScore:  liveFloat32(maxScore),
		}
		if attemptID.Valid {
			id, err := uuid.Parse(attemptID.String)
			if err != nil {
				return Quiz{}, nil, time.Time{}, fmt.Errorf("parse attempt id: %w", err)
			}
			r.AttemptId = &id
		}
		if attemptNo.Valid {
			n := int(attemptNo.Int64)
			r.AttemptNo = &n
		}
		if status.Valid {
			st := apischema.LiveRosterRowStatus(status.String)
			r.Status = &st
		}
		if submitKind.Valid {
			sk := apischema.LiveRosterRowSubmitKind(submitKind.String)
			r.SubmitKind = &sk
		}
		if startedAt.Valid {
			r.StartedAt = &startedAt.Time
		}
		if deadlineAt.Valid {
			r.DeadlineAt = &deadlineAt.Time
		}
		if answeredCount.Valid {
			n := int(answeredCount.Int64)
			r.AnsweredCount = &n
		}
		if currentQuestion.Valid {
			n := int(currentQuestion.Int64)
			r.CurrentQuestion = &n
		}
		if questionCount.Valid {
			n := int(questionCount.Int64)
			r.QuestionCount = &n
		}
		if violationCount.Valid {
			n := int(violationCount.Int64)
			r.ViolationCount = &n
		}
		disconnected := lastConnEvent != nil && *lastConnEvent == "attempt.disconnected"
		r.State = rosterState(status, submitKind, disconnected)
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
func rosterState(status, submitKind sql.NullString, disconnected bool) apischema.LiveRosterRowState {
	if !status.Valid {
		return apischema.LiveRosterRowStateNotStarted
	}
	if submitKind.Valid && submitKind.String == "kicked" {
		return apischema.LiveRosterRowStateKicked
	}
	switch status.String {
	case "in_progress":
		if disconnected {
			return apischema.LiveRosterRowStateDisconnected
		}
		return apischema.LiveRosterRowStateInProgress
	case "submitted", "graded":
		return apischema.LiveRosterRowStateSubmitted
	case "kicked":
		return apischema.LiveRosterRowStateKicked
	}
	return apischema.LiveRosterRowStateNotStarted
}
