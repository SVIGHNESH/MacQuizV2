package quiz

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"macquiz/server/internal/audit"
	"macquiz/server/internal/authusers"
)

// This file implements the Milestone 3 lifecycle (docs/06 section 1): the
// audience (PUT /quizzes/:id/assignments), publish with its snapshot +
// version write (POST /quizzes/:id/publish), the student's assigned-quiz
// list, and the lazy status derivation that treats a scheduled quiz as live
// once starts_at has passed even before the scheduler job flips the row.

// PreconditionError carries publish/assignment rule violations that need
// database facts to detect (question count, audience size). The HTTP layer
// renders it as a 422 with field errors, same as body-level validation.
type PreconditionError struct {
	Fields map[string]string
}

func (e *PreconditionError) Error() string {
	return fmt.Sprintf("precondition failed: %v", e.Fields)
}

// Guardrails is the per-quiz anti-cheat config from docs/06 section 3,
// snapshotted with the question set at publish so rules cannot change under
// a student mid-window.
type Guardrails struct {
	Fullscreen      string `json:"fullscreen"`       // off | warn | count
	FocusTracking   string `json:"focus_tracking"`   // off | warn | count
	BlockClipboard  bool   `json:"block_clipboard"`  // on means blocked and logged
	MaxViolations   int    `json:"max_violations"`   // counted violations before the ladder fires
	ViolationAction string `json:"violation_action"` // flag | auto_submit | notify
}

// DefaultGuardrails is the documented default ladder: nothing enforced,
// flag at 3 (docs/06: "default: flag at 3").
func DefaultGuardrails() Guardrails {
	return Guardrails{
		Fullscreen:      "off",
		FocusTracking:   "off",
		BlockClipboard:  false,
		MaxViolations:   3,
		ViolationAction: "flag",
	}
}

// Validate returns per-field messages for the 422 envelope; empty means the
// config is acceptable.
func (g Guardrails) Validate() map[string]string {
	fields := map[string]string{}
	switch g.Fullscreen {
	case "off", "warn", "count":
	default:
		fields["guardrails.fullscreen"] = "must be off, warn, or count"
	}
	switch g.FocusTracking {
	case "off", "warn", "count":
	default:
		fields["guardrails.focus_tracking"] = "must be off, warn, or count"
	}
	switch g.ViolationAction {
	case "flag", "auto_submit", "notify":
	default:
		fields["guardrails.violation_action"] = "must be flag, auto_submit, or notify"
	}
	if g.MaxViolations < 1 || g.MaxViolations > 100 {
		fields["guardrails.max_violations"] = "must be between 1 and 100"
	}
	return fields
}

// PublishInput is the validated publish request: the live window, the
// per-attempt duration, the guardrail config, and the results-release
// policy ("auto" or "manual"; docs/01 open question 1's documented default
// is the per-quiz toggle defaulting to auto).
type PublishInput struct {
	StartsAt      time.Time
	EndsAt        time.Time
	DurationSec   int
	Guardrails    Guardrails
	ReleasePolicy string
}

// Publish transitions Draft -> Scheduled (docs/06 section 1): it snapshots
// the question set and guardrails into an immutable quiz_versions row, bumps
// the version, and stamps the window. Publishing an already-scheduled quiz
// reschedules it and writes version n+1; live and later states refuse with
// ErrNotEditable.
func (s *Service) Publish(ctx context.Context, actor authusers.User, id string, in PublishInput) (Quiz, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Quiz{}, fmt.Errorf("begin publish tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	q, err := s.ownedForUpdate(ctx, tx, actor, id)
	if err != nil {
		return Quiz{}, err
	}
	if q.Status != "draft" && q.Status != "scheduled" {
		return Quiz{}, ErrNotEditable
	}

	// Preconditions that need database facts (docs/04: "at least one
	// question ... at least one assigned student").
	fields := map[string]string{}
	var questionCount, audienceCount int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM questions WHERE quiz_id = $1`, id).Scan(&questionCount); err != nil {
		return Quiz{}, fmt.Errorf("count questions: %w", err)
	}
	if questionCount == 0 {
		fields["questions"] = "a quiz needs at least one question before publishing"
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM quiz_assignments WHERE quiz_id = $1`, id).Scan(&audienceCount); err != nil {
		return Quiz{}, fmt.Errorf("count assignments: %w", err)
	}
	if audienceCount == 0 {
		fields["assignments"] = "assign at least one student before publishing"
	}
	if len(fields) > 0 {
		return Quiz{}, &PreconditionError{Fields: fields}
	}

	guardrailsJSON, err := json.Marshal(in.Guardrails)
	if err != nil {
		return Quiz{}, fmt.Errorf("marshal guardrails: %w", err)
	}
	// The HTTP layer validates the policy; direct callers may leave it
	// empty and get the documented default (docs/01: "default auto-release").
	if in.ReleasePolicy == "" {
		in.ReleasePolicy = "auto"
	}

	// The snapshot is assembled in SQL on purpose: Question tags Correct
	// with json:"-", so a Go-side marshal would silently drop the answer key
	// and break grading. jsonb_build_object copies the raw rows.
	newVersion := q.Version + 1
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO quiz_versions (quiz_id, version, questions, guardrails)
		 SELECT $1, $2, jsonb_agg(jsonb_build_object(
		            'id', id, 'position', position, 'type', type, 'body', body,
		            'options', options, 'correct', correct, 'points', points)
		            ORDER BY position), $3::jsonb
		 FROM questions WHERE quiz_id = $1`,
		id, newVersion, guardrailsJSON); err != nil {
		return Quiz{}, fmt.Errorf("write snapshot: %w", err)
	}

	q, err = scanQuiz(tx.QueryRowContext(ctx,
		`UPDATE quizzes
		 SET status = 'scheduled', starts_at = $1, ends_at = $2, duration_sec = $3,
		     guardrails = $4, published_at = now(), version = $5, release_policy = $6
		 WHERE id = $7
		 RETURNING `+quizColumns,
		in.StartsAt, in.EndsAt, in.DurationSec, guardrailsJSON, newVersion,
		in.ReleasePolicy, id).Scan)
	if err != nil {
		return Quiz{}, fmt.Errorf("update quiz on publish: %w", err)
	}
	q.QuestionCount = questionCount

	// The exact-timestamp transitions ride the same transaction: a quiz is
	// never scheduled without its open/close jobs, and a failed publish
	// leaves no orphan jobs (docs/06: "open_quiz job enqueued at the exact
	// timestamp").
	if err := s.enqueueWindowJobs(ctx, tx, id, in.StartsAt, in.EndsAt); err != nil {
		return Quiz{}, err
	}

	if err := audit.Write(ctx, tx, actor.ID, "quizzes.published", "quiz", id, map[string]any{
		"version":      newVersion,
		"starts_at":    in.StartsAt,
		"ends_at":      in.EndsAt,
		"duration_sec": in.DurationSec,
	}); err != nil {
		return Quiz{}, err
	}
	if err := tx.Commit(); err != nil {
		return Quiz{}, fmt.Errorf("commit publish: %w", err)
	}
	return q, nil
}

// ForceClose ends a live or scheduled quiz immediately (docs/06 section 1:
// "Live -> Closed | Scheduler at ends_at, or teacher force-close"). It flips
// the row to 'closed' and brings ends_at forward to now() in one transaction,
// then enqueues an immediate close_quiz job so the exact chain a timed close
// would run - force-submit every still-open attempt (kind='forced'), grade,
// release per policy - fires now instead of at the original ends_at. The job
// re-derives everything from the rows, so it flips no quiz twice; the status
// flip alone is what SweepDueAttempts keys the force-submit on and what Start
// reads to refuse new attempts, so enforcement never depends on the job's
// timing. Force-closing an already-closed or archived quiz is an idempotent
// no-op (no second audit row, no second job); a draft answers ErrNotClosable.
func (s *Service) ForceClose(ctx context.Context, actor authusers.User, id string) (Quiz, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Quiz{}, fmt.Errorf("begin force-close tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	q, err := s.ownedForUpdate(ctx, tx, actor, id)
	if err != nil {
		return Quiz{}, err
	}
	// quizColumns/scanQuiz omit the question count (Iteration 1), so populate
	// it for parity with Publish's response - both return a QuizResponse.
	var questionCount int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM questions WHERE quiz_id = $1`, id).Scan(&questionCount); err != nil {
		return Quiz{}, fmt.Errorf("count questions: %w", err)
	}
	switch q.Status {
	case "closed", "archived":
		// Already terminal: a double-click or a retried request changes
		// nothing, matching the idempotence discipline of kick/readmit.
		q.QuestionCount = questionCount
		return q, nil
	case "scheduled", "live":
		// force-closable
	default: // draft: never opened, so there is nothing to close.
		return Quiz{}, ErrNotClosable
	}
	fromStatus := q.Status

	q, err = scanQuiz(tx.QueryRowContext(ctx,
		`UPDATE quizzes SET status = 'closed', ends_at = now()
		 WHERE id = $1
		 RETURNING `+quizColumns, id).Scan)
	if err != nil {
		return Quiz{}, fmt.Errorf("force-close quiz: %w", err)
	}
	q.QuestionCount = questionCount

	// Fire the close chain now rather than at the original ends_at. A nil
	// InsertOpts schedules the job immediately; the stale close_quiz job at
	// the original ends_at stays queued and no-ops when it fires (the sweep
	// predicate needs status IN (scheduled, live), and this quiz is closed).
	if _, err := s.jobs.InsertTx(ctx, tx, CloseQuizArgs{QuizID: id}, nil); err != nil {
		return Quiz{}, fmt.Errorf("enqueue force-close job: %w", err)
	}

	if err := audit.Write(ctx, tx, actor.ID, "quizzes.force_closed", "quiz", id, map[string]any{
		"from_status": fromStatus,
	}); err != nil {
		return Quiz{}, err
	}
	if err := tx.Commit(); err != nil {
		return Quiz{}, fmt.Errorf("commit force-close: %w", err)
	}
	return q, nil
}

// AssignedStudent is one member of a quiz's audience, as the authoring UI
// lists it.
type AssignedStudent struct {
	ID       string `json:"id"`
	FullName string `json:"full_name"`
	Email    string `json:"email"`
}

// SetAssignments replaces the quiz's audience (docs/04: PUT
// /quizzes/:id/assignments). Group ids are expanded to individual student
// rows at assignment time, so later group edits never silently revoke an
// already-assigned quiz (docs/03). The replacement is atomic: one bad id
// changes nothing. Allowed while draft or scheduled; a live quiz's audience
// is frozen.
func (s *Service) SetAssignments(ctx context.Context, actor authusers.User, quizID string, studentIDs, groupIDs []string) ([]AssignedStudent, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin assignments tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	q, err := s.ownedForUpdate(ctx, tx, actor, quizID)
	if err != nil {
		return nil, err
	}
	if q.Status != "draft" && q.Status != "scheduled" {
		return nil, ErrNotEditable
	}

	// Every directly-listed id must be a student account.
	audience := map[string]bool{}
	if len(studentIDs) > 0 {
		var studentCount int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM users WHERE id = ANY($1::uuid[]) AND role = 'student'`,
			uuidArray(studentIDs)).Scan(&studentCount); err != nil {
			return nil, fmt.Errorf("validate student ids: %w", err)
		}
		if studentCount != len(dedupe(studentIDs)) {
			return nil, &PreconditionError{Fields: map[string]string{
				"student_ids": "every id must be an existing student account"}}
		}
		for _, id := range studentIDs {
			audience[id] = true
		}
	}

	// Every group id must exist; its members join the audience.
	if len(groupIDs) > 0 {
		var groupCount int
		if err := tx.QueryRowContext(ctx,
			`SELECT count(*) FROM groups WHERE id = ANY($1::uuid[])`,
			uuidArray(groupIDs)).Scan(&groupCount); err != nil {
			return nil, fmt.Errorf("validate group ids: %w", err)
		}
		if groupCount != len(dedupe(groupIDs)) {
			return nil, &PreconditionError{Fields: map[string]string{
				"group_ids": "every id must be an existing group"}}
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT student_id FROM group_members WHERE group_id = ANY($1::uuid[])`,
			uuidArray(groupIDs))
		if err != nil {
			return nil, fmt.Errorf("expand groups: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan group member: %w", err)
			}
			audience[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	// A scheduled quiz must keep at least one assigned student, or the
	// publish invariant (docs/04 preconditions) would silently break.
	if q.Status == "scheduled" && len(audience) == 0 {
		return nil, &PreconditionError{Fields: map[string]string{
			"student_ids": "a scheduled quiz needs at least one assigned student"}}
	}

	ids := make([]string, 0, len(audience))
	for id := range audience {
		ids = append(ids, id)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM quiz_assignments WHERE quiz_id = $1`, quizID); err != nil {
		return nil, fmt.Errorf("clear assignments: %w", err)
	}
	if len(ids) > 0 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO quiz_assignments (quiz_id, student_id, assigned_by)
			 SELECT $1, s, $3 FROM unnest($2::uuid[]) AS s`,
			quizID, uuidArray(ids), actor.ID); err != nil {
			return nil, fmt.Errorf("insert assignments: %w", err)
		}
	}
	if err := audit.Write(ctx, tx, actor.ID, "quizzes.assignments_set", "quiz", quizID,
		map[string]any{
			"student_ids": len(studentIDs),
			"group_ids":   len(groupIDs),
			"total":       len(ids),
		}); err != nil {
		return nil, err
	}

	students, err := assignedStudents(ctx, tx, quizID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit assignments: %w", err)
	}
	return students, nil
}

// ListAssignments returns the quiz's current audience to its owner.
func (s *Service) ListAssignments(ctx context.Context, actor authusers.User, quizID string) ([]AssignedStudent, error) {
	q, err := scanQuiz(s.db.QueryRowContext(ctx,
		`SELECT `+quizColumns+` FROM quizzes WHERE id = $1`, quizID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load quiz: %w", err)
	}
	if !authusers.Can(actor, authusers.ActionQuizEdit, authusers.Resource{OwnerID: q.OwnerID}) {
		return nil, ErrNotFound
	}
	return assignedStudents(ctx, s.db, quizID)
}

// querier abstracts *sql.DB and *sql.Tx for read helpers.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func assignedStudents(ctx context.Context, db querier, quizID string) ([]AssignedStudent, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT u.id, u.full_name, u.email
		 FROM quiz_assignments a JOIN users u ON u.id = a.student_id
		 WHERE a.quiz_id = $1 ORDER BY u.full_name, u.id`, quizID)
	if err != nil {
		return nil, fmt.Errorf("list assignments: %w", err)
	}
	defer rows.Close()
	students := []AssignedStudent{}
	for rows.Next() {
		var s AssignedStudent
		if err := rows.Scan(&s.ID, &s.FullName, &s.Email); err != nil {
			return nil, fmt.Errorf("scan assignment: %w", err)
		}
		students = append(students, s)
	}
	return students, rows.Err()
}

// AssignedQuiz is the student-facing quiz shape: window, budget, and size -
// never guardrail internals, never the owner, and structurally never a
// question (let alone an answer key). Attempts carries the caller's own
// attempt history so the list can offer resume, count slots, and link to
// released results.
type AssignedQuiz struct {
	ID                string           `json:"id"`
	Title             string           `json:"title"`
	Status            string           `json:"status"` // derived: upcoming quizzes read scheduled, open ones live
	StartsAt          *time.Time       `json:"starts_at"`
	EndsAt            *time.Time       `json:"ends_at"`
	DurationSec       int              `json:"duration_sec"`
	MaxAttempts       int              `json:"max_attempts"`
	Version           int              `json:"version"`
	QuestionCount     int              `json:"question_count"`
	ResultsReleasedAt *time.Time       `json:"results_released_at"`
	Attempts          []AttemptSummary `json:"attempts"`
}

// AttemptSummary is one of the caller's own attempts as the assigned list
// shows it. Score stays null until the quiz's results are released - the
// gate is applied in SQL, so the value never reaches this struct early.
type AttemptSummary struct {
	ID          string     `json:"id"`
	AttemptNo   int        `json:"attempt_no"`
	Status      string     `json:"status"`
	StartedAt   time.Time  `json:"started_at"`
	SubmittedAt *time.Time `json:"submitted_at"`
	Score       *float64   `json:"score"`
}

// AssignedQuizzes lists the caller's upcoming, live, and past quizzes
// (docs/04: GET /quizzes/assigned). Visibility is the assignment row itself;
// drafts and archived quizzes never appear. The question count comes from
// the published snapshot, so it always matches what the student will see.
func (s *Service) AssignedQuizzes(ctx context.Context, actor authusers.User) ([]AssignedQuiz, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT z.id, z.title, z.status, z.starts_at, z.ends_at, z.duration_sec,
		        z.max_attempts, z.version, jsonb_array_length(v.questions),
		        z.results_released_at
		 FROM quiz_assignments a
		 JOIN quizzes z ON z.id = a.quiz_id
		 JOIN quiz_versions v ON v.quiz_id = z.id AND v.version = z.version
		 WHERE a.student_id = $1 AND z.status IN ('scheduled', 'live', 'closed')
		 ORDER BY z.starts_at, z.id`, actor.ID)
	if err != nil {
		return nil, fmt.Errorf("list assigned quizzes: %w", err)
	}
	defer rows.Close()

	now := time.Now()
	quizzes := []AssignedQuiz{}
	for rows.Next() {
		var q AssignedQuiz
		if err := rows.Scan(&q.ID, &q.Title, &q.Status, &q.StartsAt, &q.EndsAt,
			&q.DurationSec, &q.MaxAttempts, &q.Version, &q.QuestionCount,
			&q.ResultsReleasedAt); err != nil {
			return nil, fmt.Errorf("scan assigned quiz: %w", err)
		}
		q.Status = effectiveStatus(q.Status, q.StartsAt, q.EndsAt, now)
		q.Attempts = []AttemptSummary{}
		quizzes = append(quizzes, q)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// The caller's own attempt history, release-gated in SQL: score reads
	// NULL until results_released_at is set, so the wire shape cannot leak a
	// withheld score (the same structural stance as the answer key).
	attemptRows, err := s.db.QueryContext(ctx,
		`SELECT a.quiz_id, a.id, a.attempt_no, a.status, a.started_at, a.submitted_at,
		        CASE WHEN z.results_released_at IS NOT NULL THEN a.score END
		 FROM attempts a JOIN quizzes z ON z.id = a.quiz_id
		 WHERE a.student_id = $1
		 ORDER BY a.quiz_id, a.attempt_no`, actor.ID)
	if err != nil {
		return nil, fmt.Errorf("list own attempts: %w", err)
	}
	defer attemptRows.Close()

	byQuiz := map[string]int{}
	for i, q := range quizzes {
		byQuiz[q.ID] = i
	}
	for attemptRows.Next() {
		var quizID string
		var a AttemptSummary
		if err := attemptRows.Scan(&quizID, &a.ID, &a.AttemptNo, &a.Status,
			&a.StartedAt, &a.SubmittedAt, &a.Score); err != nil {
			return nil, fmt.Errorf("scan own attempt: %w", err)
		}
		if i, ok := byQuiz[quizID]; ok {
			quizzes[i].Attempts = append(quizzes[i].Attempts, a)
		}
	}
	return quizzes, attemptRows.Err()
}

// effectiveStatus derives the status a reader should see from the stored
// status and the window (docs/06: "the API also treats the quiz as live on
// read if starts_at has passed"). The scheduler jobs flip the stored row at
// the exact timestamps; this lazy derivation covers the gap between the
// moment passing and the job landing, so no reader ever sees a stale state.
func effectiveStatus(status string, startsAt, endsAt *time.Time, now time.Time) string {
	switch status {
	case "scheduled":
		if endsAt != nil && !now.Before(*endsAt) {
			return "closed"
		}
		if startsAt != nil && !now.Before(*startsAt) {
			return "live"
		}
	case "live":
		if endsAt != nil && !now.Before(*endsAt) {
			return "closed"
		}
	}
	return status
}

// ownedForUpdate locks the quiz row and verifies ownership via the central
// policy; unlike draftForUpdate it accepts any status, leaving the
// state-machine check to the caller. Ownership failures read as ErrNotFound.
func (s *Service) ownedForUpdate(ctx context.Context, tx *sql.Tx, actor authusers.User, id string) (Quiz, error) {
	q, err := scanQuiz(tx.QueryRowContext(ctx,
		`SELECT `+quizColumns+` FROM quizzes WHERE id = $1 FOR UPDATE`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Quiz{}, ErrNotFound
	}
	if err != nil {
		return Quiz{}, fmt.Errorf("load quiz: %w", err)
	}
	if !authusers.Can(actor, authusers.ActionQuizEdit, authusers.Resource{OwnerID: q.OwnerID}) {
		return Quiz{}, ErrNotFound
	}
	return q, nil
}

// dedupe returns the distinct values of ids, for comparing against database
// match counts.
func dedupe(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
