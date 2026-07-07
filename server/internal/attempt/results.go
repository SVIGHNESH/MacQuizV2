package attempt

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"macquiz/server/internal/authusers"
)

// This file owns the student results view (docs/12 Milestone 4: "results per
// release policy" - this is the student half). The gate is
// quizzes.results_released_at: until the quiz closes and results are
// released (automatically or by the teacher), the score and the answer key
// stay server-internal (docs/08 section 3). After release, the review may
// expose the key - that is the documented exception to answer-key isolation.

// ErrResultsNotReleased marks a results read before the quiz's release
// moment, or against an attempt whose grading has not landed yet.
var ErrResultsNotReleased = errors.New("results are not released")

// ResultQuestion is one snapshot question in the released review: what was
// asked, what the student answered, the key, and what it earned. Unanswered
// questions keep Response and IsCorrect null.
type ResultQuestion struct {
	ID            string          `json:"id"`
	Position      int             `json:"position"`
	Type          string          `json:"type"`
	Body          json.RawMessage `json:"body"`
	Options       json.RawMessage `json:"options,omitempty"`
	Correct       json.RawMessage `json:"correct"`
	Points        float64         `json:"points"`
	Response      json.RawMessage `json:"response"`
	IsCorrect     *bool           `json:"is_correct"`
	PointsAwarded float64         `json:"points_awarded"`
}

// Result is the released results payload for one attempt. Attempt keeps its
// usual score-free wire shape; the released score rides at the top level, so
// no other payload embedding Attempt gains a score field by accident.
type Result struct {
	Attempt    Attempt          `json:"attempt"`
	QuizTitle  string           `json:"quiz_title"`
	Score      float64          `json:"score"`
	MaxScore   float64          `json:"max_score"`
	ReleasedAt time.Time        `json:"released_at"`
	Questions  []ResultQuestion `json:"questions"`
}

// Result serves GET /attempts/:id/result. Owner-only (anyone else reads
// 404); refused with ErrResultsNotReleased until the quiz's results are
// released and this attempt is graded. Questions come back in the same
// per-attempt order the player showed.
func (s *Service) Result(ctx context.Context, actor authusers.User, id string) (Result, error) {
	a, err := scanAttempt(s.db.QueryRowContext(ctx,
		`SELECT `+attemptColumns+` FROM attempts WHERE id = $1`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Result{}, ErrNotFound
	}
	if err != nil {
		return Result{}, fmt.Errorf("load attempt: %w", err)
	}
	if a.StudentID != actor.ID {
		return Result{}, ErrNotFound
	}

	res := Result{Attempt: a}
	var releasedAt *time.Time
	var shuffle bool
	var questionsJSON []byte
	if err := s.db.QueryRowContext(ctx,
		`SELECT z.title, z.results_released_at, z.shuffle_questions, v.questions
		 FROM quizzes z JOIN quiz_versions v ON v.quiz_id = z.id AND v.version = $2
		 WHERE z.id = $1`, a.QuizID, a.QuizVersion).Scan(
		&res.QuizTitle, &releasedAt, &shuffle, &questionsJSON); err != nil {
		return Result{}, fmt.Errorf("load quiz for result: %w", err)
	}
	// Grading is part of the gate: a manual release racing the grading sweep
	// must not show a scoreless "result" for a submitted-but-ungraded
	// attempt; the next worker pass grades it and the read succeeds.
	if releasedAt == nil || a.Status != "graded" {
		return Result{}, ErrResultsNotReleased
	}
	res.ReleasedAt = *releasedAt

	var score float64
	if err := s.db.QueryRowContext(ctx,
		`SELECT coalesce(score, 0) FROM attempts WHERE id = $1`, id).Scan(&score); err != nil {
		return Result{}, fmt.Errorf("load score: %w", err)
	}
	res.Score = score

	questions, err := decodeSnapshot(questionsJSON)
	if err != nil {
		return Result{}, err
	}
	if shuffle {
		shuffleForAttempt(questions, a.ID)
	}

	type gradedAnswer struct {
		response      json.RawMessage
		isCorrect     *bool
		pointsAwarded float64
	}
	answers := map[string]gradedAnswer{}
	rows, err := s.db.QueryContext(ctx,
		`SELECT question_id, response, coalesce(is_correct, false),
		        coalesce(points_awarded, 0)
		 FROM attempt_answers WHERE attempt_id = $1`, id)
	if err != nil {
		return Result{}, fmt.Errorf("load graded answers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var qid string
		var response []byte
		var isCorrect bool
		var awarded float64
		if err := rows.Scan(&qid, &response, &isCorrect, &awarded); err != nil {
			return Result{}, fmt.Errorf("scan graded answer: %w", err)
		}
		answers[qid] = gradedAnswer{response: response, isCorrect: &isCorrect, pointsAwarded: awarded}
	}
	if err := rows.Err(); err != nil {
		return Result{}, fmt.Errorf("load graded answers: %w", err)
	}

	res.Questions = make([]ResultQuestion, len(questions))
	for i, q := range questions {
		rq := ResultQuestion{
			ID: q.ID, Position: q.Position, Type: q.Type, Body: q.Body,
			Options: q.Options, Correct: q.Correct, Points: q.Points,
		}
		if ans, answered := answers[q.ID]; answered {
			rq.Response = ans.response
			rq.IsCorrect = ans.isCorrect
			rq.PointsAwarded = ans.pointsAwarded
		}
		res.Questions[i] = rq
		res.MaxScore += q.Points
	}
	return res, nil
}
