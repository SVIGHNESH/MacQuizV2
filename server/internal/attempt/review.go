package attempt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"macquiz/server/internal/apischema"
	"macquiz/server/internal/authusers"
)

// This file owns the teacher's per-question review of one graded attempt
// (GET /attempts/:id/review) - the drill-down behind one row of the owner's
// results table. It shares the released student review's question assembly
// (results.go) but swaps the gate: quiz ownership instead of release, so the
// owner reads the detail as soon as grading lands, before deciding to
// release - the same pre-release visibility rationale as quiz.Results.

// Review is the teacher review payload for one attempt. It is a direct alias
// to the generated apischema.AttemptReview type (api/openapi.yaml,
// oapi-codegen - see internal/apischema), so this response can never
// silently drift from the spec.
type Review = apischema.AttemptReview

// Review serves GET /attempts/:id/review. Gated by Can(actor,
// quiz.edit, owner) exactly like the results table it drills into: the
// owning teacher only, everyone else (including admins, who cannot read the
// table either) answers 404 so an attempt's existence never leaks. Refused
// with ErrNotGraded until the attempt's grading has landed - before that
// there is no verdict to show, and the coalesced is_correct would read as a
// wall of wrong answers. Questions come back in the same per-attempt order
// the player showed.
func (s *Service) Review(ctx context.Context, actor authusers.User, id string) (Review, error) {
	a, err := scanAttempt(s.db.QueryRowContext(ctx,
		`SELECT `+attemptColumns+` FROM attempts WHERE id = $1`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Review{}, ErrNotFound
	}
	if err != nil {
		return Review{}, fmt.Errorf("load attempt: %w", err)
	}

	rev := Review{Attempt: a.Attempt}
	var ownerID string
	var shuffle bool
	var questionsJSON []byte
	if err := s.db.QueryRowContext(ctx,
		`SELECT z.title, z.owner_id, z.results_released_at, z.shuffle_questions, v.questions
		 FROM quizzes z JOIN quiz_versions v ON v.quiz_id = z.id AND v.version = $2
		 WHERE z.id = $1`, a.QuizId, a.QuizVersion).Scan(
		&rev.QuizTitle, &ownerID, &rev.ReleasedAt, &shuffle, &questionsJSON); err != nil {
		return Review{}, fmt.Errorf("load quiz for review: %w", err)
	}
	if !authusers.Can(actor, authusers.ActionQuizEdit, authusers.Resource{OwnerID: ownerID}) {
		return Review{}, ErrNotFound
	}
	if a.Status != "graded" {
		return Review{}, ErrNotGraded
	}

	studentID, err := uuid.Parse(a.StudentID)
	if err != nil {
		return Review{}, fmt.Errorf("parse student id: %w", err)
	}
	rev.StudentId = studentID
	var email string
	if err := s.db.QueryRowContext(ctx,
		`SELECT full_name, email, avatar FROM users WHERE id = $1`, a.StudentID).Scan(
		&rev.FullName, &email, &rev.Avatar); err != nil {
		return Review{}, fmt.Errorf("load student for review: %w", err)
	}
	rev.Email = openapi_types.Email(email)

	var score float64
	var overriddenAt sql.NullTime
	if err := s.db.QueryRowContext(ctx,
		`SELECT coalesce(score, 0), score_overridden_at FROM attempts WHERE id = $1`, id).Scan(
		&score, &overriddenAt); err != nil {
		return Review{}, fmt.Errorf("load score for review: %w", err)
	}
	rev.Score = resultFloat32(score)
	rev.ScoreOverridden = overriddenAt.Valid

	questions, err := decodeSnapshot(questionsJSON)
	if err != nil {
		return Review{}, err
	}
	if shuffle {
		shuffleForAttempt(questions, a.Id.String())
	}
	answers, err := loadGradedAnswers(ctx, s.db, id)
	if err != nil {
		return Review{}, err
	}
	questionsOut, maxScore, err := assembleResultQuestions(questions, answers)
	if err != nil {
		return Review{}, err
	}
	rev.Questions = questionsOut
	rev.MaxScore = resultFloat32(maxScore)
	return rev, nil
}
