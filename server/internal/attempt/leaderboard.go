package attempt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"macquiz/server/internal/apischema"
	"macquiz/server/internal/authusers"
)

// This file owns the student leaderboard - the "dark island" of
// docs/11-frontend-design-system.md section 4, the one sanctioned dark
// surface in the product. It reads exactly as the design doc's St5 screen
// describes it: "ranks update as attempts are graded · ties broken by time
// taken".
//
// The read is gated identically to the released review in results.go, and
// deliberately so: a leaderboard is a list of other students' scores, and
// quizzes.results_released_at is the single moment that governs whether a
// score may leave the server (docs/08 section 3). Hanging the endpoint off
// an attempt rather than a quiz is what makes that reuse exact - the same
// owner-or-404 check, the same release-and-graded gate.

// Leaderboard is the ranked standings payload. Direct alias to the generated
// apischema type (api/openapi.yaml), like every other response in this
// package.
type Leaderboard = apischema.Leaderboard

// LeaderboardEntry is one student's row.
type LeaderboardEntry = apischema.LeaderboardEntry

// leaderboardTopN bounds how many leading rows travel over the wire. The
// reader's own row is always included on top of these, so a student ranked
// 300th still sees where they stand.
const leaderboardTopN = 10

// leaderboardQuery ranks each student's best graded attempt on one quiz.
//
// best: one row per student, their best attempt - highest score, and among
// equal scores the one they finished fastest. This mirrors
// analytics.rollupOne's "best graded attempt per student" rule, so a quiz's
// leaderboard and its score distribution can never disagree about which
// attempt counts. max_score is summed from the pinned snapshot the student
// actually saw (a republish can change it), exactly as the rollup does.
//
// scored: accuracy as a share of that snapshot's points. A snapshot carrying
// no points at all yields NULL, which sorts last.
//
// ranked: docs/11's ordering - accuracy first, ties broken by the time taken
// (submitted_at - started_at). rank() rather than dense_rank() so that two
// students tied at the top are both 1st and the next is 3rd, the ranking
// every scoreboard uses.
const leaderboardQuery = `
WITH best AS (
	SELECT DISTINCT ON (a.student_id)
	       a.student_id,
	       coalesce(a.score, 0)::float8 AS score,
	       (SELECT sum((q->>'points')::float8)
	        FROM quiz_versions v, jsonb_array_elements(v.questions) q
	        WHERE v.quiz_id = a.quiz_id AND v.version = a.quiz_version) AS max_score,
	       a.submitted_at - a.started_at AS taken
	FROM attempts a
	WHERE a.quiz_id = $1 AND a.status = 'graded'
	ORDER BY a.student_id, a.score DESC NULLS LAST, a.submitted_at - a.started_at ASC NULLS LAST
), scored AS (
	SELECT b.student_id, b.taken,
	       CASE WHEN b.max_score > 0 THEN b.score / b.max_score END AS accuracy
	FROM best b
)
SELECT rank() OVER (ORDER BY s.accuracy DESC NULLS LAST, s.taken ASC NULLS LAST) AS rank,
       s.student_id, u.full_name, s.accuracy
FROM scored s JOIN users u ON u.id = s.student_id
ORDER BY rank, u.full_name`

// Leaderboard serves GET /attempts/:id/leaderboard: the ranked standings for
// the quiz this attempt belongs to. Owner-only (anyone else reads 404) and
// refused with ErrResultsNotReleased until the quiz's results are released
// and this attempt is graded - the same gate Result applies, since the rows
// carry other students' scores.
//
// The reply carries the leading rows plus, always, the reader's own row: a
// leaderboard that cannot show you yourself has failed at the one thing the
// reader came for.
func (s *Service) Leaderboard(ctx context.Context, actor authusers.User, id string) (Leaderboard, error) {
	a, err := scanAttempt(s.db.QueryRowContext(ctx,
		`SELECT `+attemptColumns+` FROM attempts WHERE id = $1`, id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Leaderboard{}, ErrNotFound
	}
	if err != nil {
		return Leaderboard{}, fmt.Errorf("load attempt: %w", err)
	}
	if a.StudentID != actor.ID {
		return Leaderboard{}, ErrNotFound
	}

	var board Leaderboard
	var releasedAt *time.Time
	if err := s.db.QueryRowContext(ctx,
		`SELECT title, results_released_at FROM quizzes WHERE id = $1`, a.QuizId).
		Scan(&board.QuizTitle, &releasedAt); err != nil {
		return Leaderboard{}, fmt.Errorf("load quiz for leaderboard: %w", err)
	}
	// Same reasoning as Result: a manual release racing the grading sweep
	// must not answer for an attempt that has no grade yet.
	if releasedAt == nil || a.Status != "graded" {
		return Leaderboard{}, ErrResultsNotReleased
	}

	rows, err := s.db.QueryContext(ctx, leaderboardQuery, a.QuizId)
	if err != nil {
		return Leaderboard{}, fmt.Errorf("rank leaderboard: %w", err)
	}
	defer rows.Close()

	board.Entries = []LeaderboardEntry{}
	var self *LeaderboardEntry
	selfShown := false
	for rows.Next() {
		var e LeaderboardEntry
		var studentID uuid.UUID
		var accuracy sql.NullFloat64
		if err := rows.Scan(&e.Rank, &studentID, &e.FullName, &accuracy); err != nil {
			return Leaderboard{}, fmt.Errorf("scan leaderboard row: %w", err)
		}
		e.StudentId = studentID
		if accuracy.Valid {
			acc := resultFloat32(accuracy.Float64)
			e.Accuracy = &acc
		}
		e.IsSelf = studentID.String() == actor.ID
		board.Total++
		if e.IsSelf {
			row := e
			self = &row
		}
		if len(board.Entries) < leaderboardTopN {
			board.Entries = append(board.Entries, e)
			selfShown = selfShown || e.IsSelf
		}
	}
	if err := rows.Err(); err != nil {
		return Leaderboard{}, fmt.Errorf("rank leaderboard: %w", err)
	}
	// The reader is graded on this quiz by the gate above, so `self` is
	// always found; appending it only matters when they fall past the cut.
	// The test is "did their row make the cut", not "is their rank past
	// leaderboardTopN": a wide tie at the top can hand the 11th listed
	// student rank 1.
	if self != nil && !selfShown {
		board.Entries = append(board.Entries, *self)
	}
	return board, nil
}
