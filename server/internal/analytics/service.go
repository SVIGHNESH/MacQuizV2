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

// StudentStats is the student_stats row shaped for the wire. The jsonb columns
// pass through untouched (json.RawMessage); avg_time_per_question and
// completion_rate are null for a student who has never had a quiz close over
// them.
type StudentStats struct {
	StudentID          string          `json:"student_id"`
	AccuracyTrend      json.RawMessage `json:"accuracy_trend"`
	AvgTimePerQuestion *float64        `json:"avg_time_per_question"`
	CompletionRate     *float64        `json:"completion_rate"`
	TopicStrengths     json.RawMessage `json:"topic_strengths"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

// StudentStats returns one student's cross-quiz performance profile
// (docs/04 permission matrix: a student sees only themselves, a teacher sees
// their assigned students, an admin sees all). The audience test is the only
// fact Can needs beyond the actor: for a teacher it is whether the subject is
// assigned to any quiz the teacher owns, resolved with one EXISTS query; admin
// and student never consult it. Every "you cannot see this" outcome - not
// authorized, unknown student, not yet rolled up - collapses to ErrNotFound so
// a caller can never tell one student's existence from an authorization
// failure (docs/08 section 3), mirroring QuizStats.
func (s *Service) StudentStats(ctx context.Context, actor authusers.User, studentID string) (StudentStats, error) {
	assigned := false
	if actor.Role == "teacher" {
		if err := s.db.QueryRowContext(ctx,
			`SELECT EXISTS (
			   SELECT 1 FROM quiz_assignments a
			   JOIN quizzes q ON q.id = a.quiz_id
			   WHERE q.owner_id = $1 AND a.student_id = $2)`,
			actor.ID, studentID).Scan(&assigned); err != nil {
			return StudentStats{}, fmt.Errorf("check assignment: %w", err)
		}
	}
	if !authusers.Can(actor, authusers.ActionAnalyticsStudent,
		authusers.Resource{OwnerID: studentID, Assigned: assigned}) {
		return StudentStats{}, ErrNotFound
	}

	out := StudentStats{StudentID: studentID}
	var accuracyTrend, topicStrengths []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT accuracy_trend, avg_time_per_question, completion_rate, topic_strengths, updated_at
		 FROM student_stats WHERE student_id = $1`, studentID).Scan(
		&accuracyTrend, &out.AvgTimePerQuestion, &out.CompletionRate, &topicStrengths, &out.UpdatedAt)
	if err == sql.ErrNoRows {
		// The caller may see this student, but no quiz has closed over them yet
		// (or the id is not a student at all). Same 404 - nothing to show.
		return StudentStats{}, ErrNotFound
	}
	if err != nil {
		return StudentStats{}, fmt.Errorf("load student_stats: %w", err)
	}
	out.AccuracyTrend = json.RawMessage(accuracyTrend)
	out.TopicStrengths = json.RawMessage(topicStrengths)
	return out, nil
}

// TeacherStats is a teacher's activity-and-outcomes summary for the wire
// (docs/07 section 4, "Per teacher"). Unlike QuizStats and StudentStats it
// reads no rollup table - there is no teacher_stats - so it aggregates live
// from quizzes, attempts, and the per-quiz quiz_stats rows each close froze.
// The averages are null for a teacher whose quizzes have no rolled-up stats
// yet (or who has released no results), never for a real-but-idle teacher, who
// reports zero counts.
type TeacherStats struct {
	TeacherID        string   `json:"teacher_id"`
	QuizzesCreated   int      `json:"quizzes_created"`
	QuizzesConducted int      `json:"quizzes_conducted"`
	TotalAttempts    int      `json:"total_attempts"`
	AvgParticipation *float64 `json:"avg_participation"`
	// AvgClassScore is the mean of each rolled-up quiz's mean, which quiz_stats
	// stores in raw points; every fixture quiz is out of 10 here, but averaging
	// raw points across quizzes of differing max scores is points-not-percent by
	// design - quiz_stats keeps no percentage mean to average instead.
	AvgClassScore          *float64 `json:"avg_class_score"`
	AvgPublishToResultsSec *float64 `json:"avg_publish_to_results_sec"`
}

// TeacherStats returns one teacher's activity-and-outcomes summary for an admin
// or that teacher themselves (docs/07 section 4: teacher analytics are
// admin-only; a teacher sees their own). Authorization runs first via
// ActionAnalyticsTeacher with the subject as owner - a teacher can only ever
// pass it for their own id, so they can never probe another's - then an
// explicit "is this id a teacher" check 404s an admin aiming at a student or an
// unknown id. That existence check is what lets a real-but-idle teacher return
// zero counts (a legitimate 200) while keeping every "you cannot see this"
// outcome an existence-safe ErrNotFound, mirroring QuizStats.
func (s *Service) TeacherStats(ctx context.Context, actor authusers.User, teacherID string) (TeacherStats, error) {
	if !authusers.Can(actor, authusers.ActionAnalyticsTeacher,
		authusers.Resource{OwnerID: teacherID}) {
		return TeacherStats{}, ErrNotFound
	}
	var isTeacher bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE id = $1 AND role = 'teacher')`,
		teacherID).Scan(&isTeacher); err != nil {
		return TeacherStats{}, fmt.Errorf("check teacher: %w", err)
	}
	if !isTeacher {
		return TeacherStats{}, ErrNotFound
	}

	// One round trip: counts of created (all owned) and conducted (launched -
	// published_at set) quizzes, every attempt across them, the averages of the
	// frozen per-quiz participation and mean, and the mean publish-to-results
	// latency in seconds over quizzes whose results have actually been released.
	out := TeacherStats{TeacherID: teacherID}
	err := s.db.QueryRowContext(ctx,
		`SELECT
		   (SELECT count(*) FROM quizzes WHERE owner_id = $1),
		   (SELECT count(*) FROM quizzes WHERE owner_id = $1 AND published_at IS NOT NULL),
		   (SELECT count(*) FROM attempts a JOIN quizzes q ON q.id = a.quiz_id WHERE q.owner_id = $1),
		   (SELECT avg(qs.participation) FROM quiz_stats qs JOIN quizzes q ON q.id = qs.quiz_id WHERE q.owner_id = $1),
		   (SELECT avg(qs.mean) FROM quiz_stats qs JOIN quizzes q ON q.id = qs.quiz_id WHERE q.owner_id = $1),
		   (SELECT extract(epoch FROM avg(results_released_at - published_at))
		      FROM quizzes WHERE owner_id = $1 AND results_released_at IS NOT NULL AND published_at IS NOT NULL)`,
		teacherID).Scan(
		&out.QuizzesCreated, &out.QuizzesConducted, &out.TotalAttempts,
		&out.AvgParticipation, &out.AvgClassScore, &out.AvgPublishToResultsSec)
	if err != nil {
		return TeacherStats{}, fmt.Errorf("aggregate teacher stats: %w", err)
	}
	return out, nil
}
