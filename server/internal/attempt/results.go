package attempt

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"macquiz/server/internal/apischema"
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
// questions keep Response and IsCorrect null. It is a direct alias to the
// generated apischema.ResultQuestion type (api/openapi.yaml, oapi-codegen -
// see internal/apischema), so this response can never silently drift from
// the spec.
type ResultQuestion = apischema.ResultQuestion

// Result is the released results payload for one attempt. Attempt keeps its
// usual score-free wire shape; the released score rides at the top level, so
// no other payload embedding Attempt gains a score field by accident. It is
// a direct alias to the generated apischema.AttemptResult type.
type Result = apischema.AttemptResult

// resultFloat32 narrows a float64 score/points value to the float32 the
// generated wire type expects. These are display-only scores (docs/07
// section 3), never a financial computation, so float32's ~7 significant
// digits lose nothing that matters on the wire.
func resultFloat32(v float64) float32 {
	return float32(v)
}

// gradedAnswer is one attempt_answers row, keyed by question id in Result.
type gradedAnswer struct {
	response      json.RawMessage
	isCorrect     *bool
	pointsAwarded float64
}

// buildResultQuestion converts one snapshot Question plus its (possibly
// absent) graded answer into the generated wire shape. Body is a uniform
// shape across every question type (docs/03-data-model.md); Correct and
// Response are typed `interface{}` in the spec precisely because their shape
// depends on the question type, so decoding the stored raw JSON into `any`
// carries whatever shape it holds without needing a type switch.
func buildResultQuestion(q Question, ans gradedAnswer, answered bool) (ResultQuestion, error) {
	id, err := uuid.Parse(q.ID)
	if err != nil {
		return ResultQuestion{}, fmt.Errorf("parse question id: %w", err)
	}
	var body apischema.QuestionBody
	if err := json.Unmarshal(q.Body, &body); err != nil {
		return ResultQuestion{}, fmt.Errorf("decode question body: %w", err)
	}
	var options *[]apischema.QuestionOption
	if len(q.Options) > 0 {
		var opts []apischema.QuestionOption
		if err := json.Unmarshal(q.Options, &opts); err != nil {
			return ResultQuestion{}, fmt.Errorf("decode question options: %w", err)
		}
		options = &opts
	}
	var correct any
	if err := json.Unmarshal(q.Correct, &correct); err != nil {
		return ResultQuestion{}, fmt.Errorf("decode question correct: %w", err)
	}
	rq := ResultQuestion{
		Id: id, Position: q.Position, Type: apischema.ResultQuestionType(q.Type),
		Body: body, Options: options, Correct: correct, Points: resultFloat32(q.Points),
	}
	if answered {
		var response any
		if err := json.Unmarshal(ans.response, &response); err != nil {
			return ResultQuestion{}, fmt.Errorf("decode answer response: %w", err)
		}
		rq.Response = &response
		rq.IsCorrect = ans.isCorrect
		rq.PointsAwarded = resultFloat32(ans.pointsAwarded)
	}
	return rq, nil
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

	res := Result{Attempt: a.Attempt}
	var releasedAt *time.Time
	var shuffle bool
	var questionsJSON []byte
	if err := s.db.QueryRowContext(ctx,
		`SELECT z.title, z.results_released_at, z.shuffle_questions, v.questions
		 FROM quizzes z JOIN quiz_versions v ON v.quiz_id = z.id AND v.version = $2
		 WHERE z.id = $1`, a.QuizId, a.QuizVersion).Scan(
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
	var overriddenAt *time.Time
	if err := s.db.QueryRowContext(ctx,
		`SELECT coalesce(score, 0), score_overridden_at FROM attempts WHERE id = $1`, id).Scan(&score, &overriddenAt); err != nil {
		return Result{}, fmt.Errorf("load score: %w", err)
	}
	res.Score = resultFloat32(score)
	res.ScoreOverridden = overriddenAt != nil

	questions, err := decodeSnapshot(questionsJSON)
	if err != nil {
		return Result{}, err
	}
	if shuffle {
		shuffleForAttempt(questions, a.Id.String())
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
	var maxScore float64
	for i, q := range questions {
		ans, answered := answers[q.ID]
		rq, err := buildResultQuestion(q, ans, answered)
		if err != nil {
			return Result{}, err
		}
		res.Questions[i] = rq
		maxScore += q.Points
	}
	res.MaxScore = resultFloat32(maxScore)

	if pct := quizPercentile(ctx, s.db, a.QuizId.String(), score, maxScore); pct != nil {
		p := resultFloat32(*pct)
		res.Percentile = &p
	}
	return res, nil
}

// distributionBuckets mirrors analytics.distributionBuckets: quiz_stats.
// distribution is 10 equal-width percentage-of-max-score buckets, bucket i
// covering [i*10, (i+1)*10)% with a perfect 100% folded into the last one.
const distributionBuckets = 10

// quizPercentile answers docs/07 section 3's "score and percentile per quiz"
// (docs/07-authoring-imports-analytics.md line 37) from the already-computed
// quiz_stats.distribution histogram - no separate query against every other
// attempt, matching docs/07 section 4's "no separate analytics store, read
// the precomputed rows" discipline. It is necessarily bucket-granular (10
// buckets), not an exact rank, and returns nil whenever a precise answer
// isn't available yet: no points on the quiz, no quiz_stats row (results
// were released a moment before the same worker pass's rollup step ran), or
// zero attempts counted in it.
func quizPercentile(ctx context.Context, db querier, quizID string, score, maxScore float64) *float64 {
	if maxScore <= 0 {
		return nil
	}
	var raw []byte
	if err := db.QueryRowContext(ctx,
		`SELECT distribution FROM quiz_stats WHERE quiz_id = $1`, quizID).Scan(&raw); err != nil {
		return nil
	}
	var buckets []int
	if err := json.Unmarshal(raw, &buckets); err != nil || len(buckets) != distributionBuckets {
		return nil
	}
	total := 0
	for _, c := range buckets {
		total += c
	}
	if total == 0 {
		return nil
	}
	idx := int(score / maxScore * distributionBuckets)
	if idx < 0 {
		idx = 0
	}
	if idx >= distributionBuckets {
		idx = distributionBuckets - 1
	}
	below := 0
	for _, c := range buckets[:idx] {
		below += c
	}
	pct := (float64(below) + 0.5*float64(buckets[idx])) / float64(total) * 100
	return &pct
}
