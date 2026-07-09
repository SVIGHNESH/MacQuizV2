package quiz

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"macquiz/server/internal/apischema"
	"macquiz/server/internal/audit"
	"macquiz/server/internal/authusers"
)

// This file owns the results-release side of Milestone 4 (docs/01 open
// question 1, resolved to its documented default: a per-quiz toggle
// defaulting to auto-release at close). results_released_at is the single
// fact every student-facing results read gates on; until it is set, scores
// and the answer key stay server-internal (docs/08 section 3).

// ReleaseResults is the teacher's explicit release (POST
// /quizzes/:id/release-results). It requires the quiz's window to have ended
// - judged on the database clock, with the same lazy derivation readers use,
// so a teacher clicking the moment ends_at passes never races the scheduler
// job. Releasing an already-released quiz is an idempotent no-op. Works for
// both policies: on an auto quiz it simply beats the worker to the same
// write.
func (s *Service) ReleaseResults(ctx context.Context, actor authusers.User, id string) (Quiz, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Quiz{}, fmt.Errorf("begin release tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	q, err := s.ownedForUpdate(ctx, tx, actor, id)
	if err != nil {
		return Quiz{}, err
	}
	if q.ResultsReleasedAt != nil {
		return q, nil
	}
	var now time.Time
	if err := tx.QueryRowContext(ctx, `SELECT now()`).Scan(&now); err != nil {
		return Quiz{}, fmt.Errorf("read database clock: %w", err)
	}
	// Archived is a superset-terminal of closed: a manual-release quiz archived
	// before its results were released can still be released ("analytics
	// retained"), so both terminal states pass this gate.
	if es := effectiveStatus(q.Status, q.StartsAt, q.EndsAt, now); es != "closed" && es != "archived" {
		return Quiz{}, ErrQuizNotClosed
	}

	// The status flip rides along for the derivation gap between ends_at
	// passing and the close job landing - the same idempotent predicate the
	// sweep uses, so the two paths can never disagree. An already-archived quiz
	// keeps its terminal status; only a not-yet-flipped row advances to closed.
	q, err = scanQuiz(tx.QueryRowContext(ctx,
		`UPDATE quizzes
		 SET status = CASE WHEN status = 'archived' THEN 'archived' ELSE 'closed' END::quiz_status,
		     results_released_at = now()
		 WHERE id = $1 RETURNING `+quizColumns, id).Scan)
	if err != nil {
		return Quiz{}, fmt.Errorf("release results: %w", err)
	}
	if err := audit.Write(ctx, tx, actor.ID, "quizzes.results_released", "quiz", id,
		map[string]any{"release_policy": q.ReleasePolicy}); err != nil {
		return Quiz{}, err
	}
	if err := tx.Commit(); err != nil {
		return Quiz{}, fmt.Errorf("commit release: %w", err)
	}
	return q, nil
}

// ReleaseDueResults applies the auto policy: every closed (or archived - a
// force-closed auto quiz may be archived before this sweep runs, and its
// results must still auto-release; "analytics retained") auto-release quiz
// whose attempts are all terminal and graded gets results_released_at
// stamped. The grading guard makes release atomic from the student's side -
// results never appear with half the class still ungraded - and makes the
// sweep safe to run at any time, from any caller, like every other sweep in
// the worker pass (which runs this right after GradeSubmitted, so a quiz
// closes, grades, and releases in one pass).
func ReleaseDueResults(ctx context.Context, db *sql.DB) (int64, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE quizzes z SET results_released_at = now()
		 WHERE z.status IN ('closed', 'archived') AND z.release_policy = 'auto'
		   AND z.results_released_at IS NULL
		   AND NOT EXISTS (
		       SELECT 1 FROM attempts a
		       WHERE a.quiz_id = z.id AND a.status IN ('in_progress', 'submitted'))`)
	if err != nil {
		return 0, fmt.Errorf("release due results: %w", err)
	}
	released, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("count released quizzes: %w", err)
	}
	return released, nil
}

// ResultRow is one line of the teacher's results table: an assigned student
// and one of their attempts. Students who never started keep the attempt
// fields null, so the table shows absence instead of hiding it. The owner
// sees scores as grading lands, before any release - releasing is their
// decision to make, so they need the numbers first. It is a direct alias to
// the generated apischema.ResultRow type (api/openapi.yaml, oapi-codegen -
// see internal/apischema), so this response can never silently drift from
// the spec.
type ResultRow = apischema.ResultRow

// resultFloat32 narrows a nullable float64 score/max-score column to the
// *float32 the generated wire type expects. These are display-only scores
// (docs/07 section 3), never a financial computation, so float32's ~7
// significant digits lose nothing that matters on the wire.
func resultFloat32(v sql.NullFloat64) *float32 {
	if !v.Valid {
		return nil
	}
	f := float32(v.Float64)
	return &f
}

// Results returns the owner's per-student results view (docs/12 Milestone 4:
// "results per release policy" - this is the teacher half). MaxScore comes
// from the snapshot version each attempt pinned, so a republish between
// attempts scores each against its own total.
func (s *Service) Results(ctx context.Context, actor authusers.User, quizID string) (Quiz, []ResultRow, error) {
	q, err := scanQuiz(s.db.QueryRowContext(ctx,
		`SELECT `+quizColumns+` FROM quizzes WHERE id = $1`, quizID).Scan)
	if err == sql.ErrNoRows {
		return Quiz{}, nil, ErrNotFound
	}
	if err != nil {
		return Quiz{}, nil, fmt.Errorf("load quiz: %w", err)
	}
	if !authusers.Can(actor, authusers.ActionQuizEdit, authusers.Resource{OwnerID: q.OwnerID}) {
		return Quiz{}, nil, ErrNotFound
	}
	q.Status = effectiveStatus(q.Status, q.StartsAt, q.EndsAt, time.Now())

	rows, err := s.db.QueryContext(ctx,
		`SELECT u.id, u.full_name, u.email,
		        a.id, a.attempt_no, a.status, a.submit_kind, a.started_at,
		        a.submitted_at, a.score, a.score_overridden_at,
		        (SELECT sum((q->>'points')::float8)
		         FROM jsonb_array_elements(v.questions) q)
		 FROM quiz_assignments s
		 JOIN users u ON u.id = s.student_id
		 LEFT JOIN attempts a ON a.quiz_id = s.quiz_id AND a.student_id = s.student_id
		 LEFT JOIN quiz_versions v ON v.quiz_id = a.quiz_id AND v.version = a.quiz_version
		 WHERE s.quiz_id = $1
		 ORDER BY u.full_name, u.id, a.attempt_no`, quizID)
	if err != nil {
		return Quiz{}, nil, fmt.Errorf("list results: %w", err)
	}
	defer rows.Close()

	results := []ResultRow{}
	for rows.Next() {
		var studentID, fullName, email string
		var attemptID, status, submitKind sql.NullString
		var attemptNo sql.NullInt64
		var startedAt, submittedAt sql.NullTime
		var score, maxScore sql.NullFloat64
		var overriddenAt *time.Time
		if err := rows.Scan(&studentID, &fullName, &email, &attemptID,
			&attemptNo, &status, &submitKind, &startedAt, &submittedAt,
			&score, &overriddenAt, &maxScore); err != nil {
			return Quiz{}, nil, fmt.Errorf("scan result row: %w", err)
		}
		studentUUID, err := uuid.Parse(studentID)
		if err != nil {
			return Quiz{}, nil, fmt.Errorf("parse student id: %w", err)
		}
		r := ResultRow{
			StudentId:       studentUUID,
			FullName:        fullName,
			Email:           openapi_types.Email(email),
			Score:           resultFloat32(score),
			MaxScore:        resultFloat32(maxScore),
			ScoreOverridden: overriddenAt != nil,
		}
		if attemptID.Valid {
			id, err := uuid.Parse(attemptID.String)
			if err != nil {
				return Quiz{}, nil, fmt.Errorf("parse attempt id: %w", err)
			}
			r.AttemptId = &id
		}
		if attemptNo.Valid {
			n := int(attemptNo.Int64)
			r.AttemptNo = &n
		}
		if status.Valid {
			st := apischema.ResultRowStatus(status.String)
			r.Status = &st
		}
		if submitKind.Valid {
			sk := apischema.ResultRowSubmitKind(submitKind.String)
			r.SubmitKind = &sk
		}
		if startedAt.Valid {
			r.StartedAt = &startedAt.Time
		}
		if submittedAt.Valid {
			r.SubmittedAt = &submittedAt.Time
		}
		results = append(results, r)
	}
	return q, results, rows.Err()
}
