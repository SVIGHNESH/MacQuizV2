package analytics

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"macquiz/server/internal/authusers"
)

// This file owns the read side of Milestone 8 (docs/04 section 2:
// "GET /analytics/quizzes/:id - Quiz stats + item analysis (from
// quiz_stats)"). It serves the row that RollupDue computed on close; it never
// computes on read, so a dashboard hit is one cheap primary-key lookup.

// ErrNotFound is returned for a quiz the caller may not see, a quiz that does
// not exist, and a quiz whose stats have not been rolled up yet. All three
// collapse to one 404 at the edge so a non-owner can never tell a private
// quiz's existence from an authorization failure (docs/08 section 3).
var ErrNotFound = errors.New("analytics: not found")

// Service reads the analytics rollups. It stays inside the analytics boundary
// (docs/02 section 3): it only reads quiz_stats plus the quiz's owner for the
// authorization decision, and writes nothing.
type Service struct {
	db  *sql.DB
	log *slog.Logger
}

// NewService wires the analytics read service.
func NewService(db *sql.DB, log *slog.Logger) *Service {
	return &Service{db: db, log: log}
}

// QuizStats is the quiz_stats row shaped for the wire. The jsonb columns pass
// through untouched (json.RawMessage), so the endpoint reports exactly what
// RollupDue stored without a decode/re-encode round trip. mean/median are
// null-scored for a quiz no student completed.
type QuizStats struct {
	QuizID        string          `json:"quiz_id"`
	Distribution  json.RawMessage `json:"distribution"`
	Mean          *float64        `json:"mean"`
	Median        *float64        `json:"median"`
	Participation *float64        `json:"participation"`
	ItemAnalysis  json.RawMessage `json:"item_analysis"`
	Integrity     json.RawMessage `json:"integrity"`
	ComputedAt    time.Time       `json:"computed_at"`
}

// QuizStats returns one quiz's rolled-up stats for the owning teacher or an
// admin (docs/04 permission matrix: teacher analytics is self-only). It loads
// the quiz's owner first so the policy resolves admin-OR-owning-teacher, and
// maps every "you cannot see this" outcome - not authorized, unknown quiz, not
// yet rolled up - to ErrNotFound, mirroring quiz.Service.Results.
func (s *Service) QuizStats(ctx context.Context, actor authusers.User, quizID string) (QuizStats, error) {
	var ownerID string
	err := s.db.QueryRowContext(ctx,
		`SELECT owner_id::text FROM quizzes WHERE id = $1`, quizID).Scan(&ownerID)
	if err == sql.ErrNoRows {
		return QuizStats{}, ErrNotFound
	}
	if err != nil {
		return QuizStats{}, fmt.Errorf("load quiz owner: %w", err)
	}
	if !authusers.Can(actor, authusers.ActionAnalyticsTeacher,
		authusers.Resource{OwnerID: ownerID}) {
		return QuizStats{}, ErrNotFound
	}

	out := QuizStats{QuizID: quizID}
	var distribution, itemAnalysis, integrity []byte
	err = s.db.QueryRowContext(ctx,
		`SELECT distribution, mean, median, participation, item_analysis, integrity, computed_at
		 FROM quiz_stats WHERE quiz_id = $1`, quizID).Scan(
		&distribution, &out.Mean, &out.Median, &out.Participation,
		&itemAnalysis, &integrity, &out.ComputedAt)
	if err == sql.ErrNoRows {
		// The quiz exists and the caller may see it, but the rollup has not run
		// yet (the quiz is still open, or it just closed and the sweep has not
		// reached it). Report the same 404 - there is nothing to show.
		return QuizStats{}, ErrNotFound
	}
	if err != nil {
		return QuizStats{}, fmt.Errorf("load quiz_stats: %w", err)
	}
	out.Distribution = json.RawMessage(distribution)
	out.ItemAnalysis = json.RawMessage(itemAnalysis)
	out.Integrity = json.RawMessage(integrity)
	return out, nil
}
