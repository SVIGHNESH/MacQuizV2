package analytics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"macquiz/server/internal/apischema"
	"macquiz/server/internal/authusers"
)

// This file owns the list-shaped analytics reads behind the admin and
// teacher analytics tabs. Each endpoint is one query producing one row per
// subject, so a dashboard over the whole org never becomes one request (or
// one round trip) per teacher or student. Filtering stays client-side: the
// lists are role-bounded (an org's staff and students), not unbounded
// event data.

// TeacherOverview is one admin-analytics row per teacher: identity plus the
// same live aggregates TeacherStats computes for a single id.
type TeacherOverview = apischema.TeacherOverview

// StudentOverview is one admin-analytics row per student: identity, cohort
// memberships, and a summary of the student_stats rollup.
type StudentOverview = apischema.StudentOverview

// TeacherStudentPerformance is one student's performance on a single
// teacher's quizzes, with a per-quiz breakdown - the teacher-scoped view of
// docs/07 section 3.
type TeacherStudentPerformance = apischema.TeacherStudentPerformance

// ListTeacherAnalytics returns every teacher's activity-and-outcomes summary
// for the admin analytics tab (docs/07 section 4: teacher analytics are
// admin-only). The per-teacher subqueries mirror TeacherStats exactly, so a
// row here can never disagree with the drill-down for the same teacher.
func (s *Service) ListTeacherAnalytics(ctx context.Context, actor authusers.User) ([]TeacherOverview, error) {
	if !authusers.Can(actor, authusers.ActionAnalyticsOrg, authusers.Resource{}) {
		return nil, ErrNotFound
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT u.id, u.full_name, u.email, u.status,
		        (SELECT count(*) FROM quizzes WHERE owner_id = u.id),
		        (SELECT count(*) FROM quizzes WHERE owner_id = u.id AND published_at IS NOT NULL),
		        (SELECT count(*) FROM attempts a JOIN quizzes q ON q.id = a.quiz_id WHERE q.owner_id = u.id),
		        (SELECT avg(qs.participation) FROM quiz_stats qs JOIN quizzes q ON q.id = qs.quiz_id WHERE q.owner_id = u.id),
		        (SELECT avg(qs.mean) FROM quiz_stats qs JOIN quizzes q ON q.id = qs.quiz_id WHERE q.owner_id = u.id)
		 FROM users u
		 WHERE u.role = 'teacher'
		 ORDER BY u.full_name, u.id`)
	if err != nil {
		return nil, fmt.Errorf("list teacher analytics: %w", err)
	}
	defer rows.Close()

	out := []TeacherOverview{}
	for rows.Next() {
		var t TeacherOverview
		var status string
		var avgParticipation, avgClassScore sql.NullFloat64
		if err := rows.Scan(&t.TeacherId, &t.FullName, &t.Email, &status,
			&t.QuizzesCreated, &t.QuizzesConducted, &t.TotalAttempts,
			&avgParticipation, &avgClassScore); err != nil {
			return nil, fmt.Errorf("scan teacher overview: %w", err)
		}
		t.Status = apischema.TeacherOverviewStatus(status)
		t.AvgParticipation = float32ptr(avgParticipation)
		t.AvgClassScore = float32ptr(avgClassScore)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListStudentAnalytics returns every student's cross-quiz profile summary
// for the admin analytics tab. It reads the student_stats rollup (plus each
// student's cohort ids for client-side group filtering); a student with no
// terminal quiz yet reports zero quizzes and null averages rather than being
// absent, so the admin sees the whole roster, not just the active tail.
func (s *Service) ListStudentAnalytics(ctx context.Context, actor authusers.User) ([]StudentOverview, error) {
	if !authusers.Can(actor, authusers.ActionAnalyticsOrg, authusers.Resource{}) {
		return nil, ErrNotFound
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT u.id, u.full_name, u.email, u.status,
		        coalesce((SELECT jsonb_agg(gm.group_id ORDER BY gm.group_id)
		                  FROM group_members gm WHERE gm.student_id = u.id), '[]'::jsonb),
		        coalesce(jsonb_array_length(ss.accuracy_trend), 0),
		        (SELECT avg((e->>'accuracy')::float8)
		         FROM jsonb_array_elements(ss.accuracy_trend) e
		         WHERE e->>'accuracy' IS NOT NULL),
		        ss.completion_rate::float8,
		        ss.avg_time_per_question::float8,
		        ss.updated_at
		 FROM users u
		 LEFT JOIN student_stats ss ON ss.student_id = u.id
		 WHERE u.role = 'student'
		 ORDER BY u.full_name, u.id`)
	if err != nil {
		return nil, fmt.Errorf("list student analytics: %w", err)
	}
	defer rows.Close()

	out := []StudentOverview{}
	for rows.Next() {
		var st StudentOverview
		var status string
		var groupsJSON []byte
		var avgAccuracy, completionRate, avgTime sql.NullFloat64
		var updatedAt sql.NullTime
		if err := rows.Scan(&st.StudentId, &st.FullName, &st.Email, &status,
			&groupsJSON, &st.QuizzesTaken, &avgAccuracy, &completionRate,
			&avgTime, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan student overview: %w", err)
		}
		st.Status = apischema.StudentOverviewStatus(status)
		if err := json.Unmarshal(groupsJSON, &st.GroupIds); err != nil {
			return nil, fmt.Errorf("decode group ids: %w", err)
		}
		st.AvgAccuracy = float32ptr(avgAccuracy)
		st.CompletionRate = float32ptr(completionRate)
		st.AvgTimePerQuestion = float32ptr(avgTime)
		if updatedAt.Valid {
			at := updatedAt.Time
			st.UpdatedAt = &at
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// TeacherStudents returns every student assigned to any quiz this teacher
// owns, with their performance on those quizzes only (docs/07 section 3,
// "performance of the students on the quizzes created by that teacher").
// Authorization mirrors TeacherStats: an admin or that teacher themselves,
// every other caller reads ErrNotFound. Scores follow the quiz_stats
// convention - the best graded attempt per (student, quiz), as a percentage
// of the pinned snapshot's points, so a republished quiz can never distort
// an already-earned score.
func (s *Service) TeacherStudents(ctx context.Context, actor authusers.User, teacherID string) ([]TeacherStudentPerformance, error) {
	if !authusers.Can(actor, authusers.ActionAnalyticsTeacher,
		authusers.Resource{OwnerID: teacherID}) {
		return nil, ErrNotFound
	}
	var isTeacher bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE id = $1 AND role = 'teacher')`,
		teacherID).Scan(&isTeacher); err != nil {
		return nil, fmt.Errorf("check teacher: %w", err)
	}
	if !isTeacher {
		return nil, ErrNotFound
	}

	// One row per (student, assigned quiz); grouped into per-student rows
	// below. The violations lateral spans every attempt on the teacher's
	// quizzes, not just the best one - integrity evidence must never vanish
	// because a later attempt scored higher.
	rows, err := s.db.QueryContext(ctx,
		`SELECT u.id, u.full_name, u.email,
		        q.id, q.title, q.status,
		        b.score, b.max_score, b.submitted_at,
		        coalesce(v.violations, 0)
		 FROM quiz_assignments asg
		 JOIN quizzes q ON q.id = asg.quiz_id AND q.owner_id = $1
		 JOIN users u ON u.id = asg.student_id
		 LEFT JOIN LATERAL (
		     SELECT a.score::float8 AS score, a.submitted_at,
		            (SELECT sum((qq->>'points')::float8)
		             FROM quiz_versions qv, jsonb_array_elements(qv.questions) qq
		             WHERE qv.quiz_id = a.quiz_id AND qv.version = a.quiz_version) AS max_score
		     FROM attempts a
		     WHERE a.quiz_id = asg.quiz_id AND a.student_id = asg.student_id
		       AND a.status = 'graded'
		     ORDER BY a.score DESC NULLS LAST
		     LIMIT 1
		 ) b ON true
		 LEFT JOIN LATERAL (
		     SELECT sum(a.violation_count)::int AS violations
		     FROM attempts a JOIN quizzes oq ON oq.id = a.quiz_id
		     WHERE oq.owner_id = $1 AND a.student_id = asg.student_id
		 ) v ON true
		 ORDER BY u.full_name, u.id, q.title, q.id`,
		teacherID)
	if err != nil {
		return nil, fmt.Errorf("list teacher students: %w", err)
	}
	defer rows.Close()

	out := []TeacherStudentPerformance{}
	var current *TeacherStudentPerformance
	var percentSum float64
	var percentCount int
	flush := func() {
		if current == nil {
			return
		}
		if percentCount > 0 {
			avg := float32(percentSum / float64(percentCount))
			current.AvgScorePercent = &avg
		}
		out = append(out, *current)
		current, percentSum, percentCount = nil, 0, 0
	}
	for rows.Next() {
		var studentID uuid.UUID
		var fullName, email, quizTitle, quizStatus string
		var quizID uuid.UUID
		var score, maxScore sql.NullFloat64
		var submittedAt sql.NullTime
		var violations int
		if err := rows.Scan(&studentID, &fullName, &email,
			&quizID, &quizTitle, &quizStatus,
			&score, &maxScore, &submittedAt, &violations); err != nil {
			return nil, fmt.Errorf("scan teacher student row: %w", err)
		}

		if current == nil || current.StudentId != studentID {
			flush()
			current = &TeacherStudentPerformance{
				StudentId:       studentID,
				FullName:        fullName,
				Email:           email,
				TotalViolations: violations,
				Quizzes:         []apischema.TeacherStudentQuizScore{},
			}
		}

		entry := apischema.TeacherStudentQuizScore{
			QuizId: quizID,
			Title:  quizTitle,
			Status: apischema.TeacherStudentQuizScoreStatus(quizStatus),
		}
		current.AssignedQuizzes++
		if score.Valid {
			current.CompletedQuizzes++
			if maxScore.Valid && maxScore.Float64 > 0 {
				percent := float32(score.Float64 / maxScore.Float64 * 100)
				entry.ScorePercent = &percent
				percentSum += float64(percent)
				percentCount++
			}
		}
		if submittedAt.Valid {
			at := submittedAt.Time
			entry.SubmittedAt = &at
			if current.LastSubmittedAt == nil || at.After(*current.LastSubmittedAt) {
				last := at
				current.LastSubmittedAt = &last
			}
		}
		current.Quizzes = append(current.Quizzes, entry)
	}
	flush()
	return out, rows.Err()
}
