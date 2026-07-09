package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/config"
	"macquiz/server/internal/db"
	"macquiz/server/internal/quiz"
)

// loadtestSeed provisions the fixtures the go-live-herd load test
// (docs/12-implementation-plan.md: "1,000 simulated starts in 60s, 2,000
// sockets, autosave p95 < 300ms") needs: a teacher, MACQUIZ_LOADTEST_STUDENTS
// student accounts (default 2000, matching docs/01's "2,000 concurrent
// students" peak assumption), and a quiz assigned to all of them and
// published with a wide-open live window. scripts/loadtest/herd.js then hits
// real accounts and a real live quiz instead of needing to drive the admin
// API itself, which would make the seeding part of the measured load rather
// than setup for it.
//
// Idempotent: reruns reuse the existing teacher/quiz/students (matched by
// email/title) rather than duplicating them, and top up the student count if
// asked for more than a prior run seeded - the same command works for the
// first seed and for scaling a rerun up.
func loadtestSeed(ctx context.Context, cfg config.Config, log *slog.Logger) error {
	studentCount := envInt("MACQUIZ_LOADTEST_STUDENTS", 2000)
	password := envOr("MACQUIZ_LOADTEST_PASSWORD", "LoadTest!Pass123")
	teacherEmail := envOr("MACQUIZ_LOADTEST_TEACHER_EMAIL", "loadtest-teacher@macquiz.load")
	quizTitle := envOr("MACQUIZ_LOADTEST_QUIZ_TITLE", "Go-Live Herd Load Test")

	sqlDB, err := db.Open(ctx, cfg.DatabaseURL, 0)
	if err != nil {
		return err
	}
	defer sqlDB.Close()

	var bootstrapAdminID string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT id FROM users WHERE role = 'admin' ORDER BY created_at LIMIT 1`).Scan(&bootstrapAdminID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errors.New("no admin account exists yet - run `macquiz bootstrap` first")
		}
		return fmt.Errorf("find bootstrap admin: %w", err)
	}

	hash, err := authusers.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash loadtest password: %w", err)
	}

	teacherID, err := upsertLoadtestUser(ctx, sqlDB, "teacher", teacherEmail, "Load Test Teacher", hash, bootstrapAdminID)
	if err != nil {
		return fmt.Errorf("seed teacher: %w", err)
	}

	studentIDs, err := upsertLoadtestStudents(ctx, sqlDB, studentCount, hash, bootstrapAdminID)
	if err != nil {
		return fmt.Errorf("seed students: %w", err)
	}

	importDir, err := os.MkdirTemp("", "macquiz-loadtest-import")
	if err != nil {
		return fmt.Errorf("scratch import dir: %w", err)
	}
	qsvc := quiz.NewService(sqlDB, log, quiz.NewImportFileStore(importDir, "", "", "", ""))
	teacher := authusers.User{ID: teacherID, Role: "teacher", Email: teacherEmail, FullName: "Load Test Teacher", Status: "active"}

	quizID, err := findOrCreateLoadtestQuiz(ctx, sqlDB, qsvc, teacher, quizTitle)
	if err != nil {
		return fmt.Errorf("seed quiz questions: %w", err)
	}

	// SetAssignments/Publish only accept a draft-or-scheduled quiz - once
	// the worker's open_quiz job has flipped it to live (which it will,
	// starts_at being in the past), a live quiz's audience and window are
	// frozen by design (docs/06 section 1). So a rerun after go-live is a
	// deliberate no-op here rather than an error: the fixtures are already
	// exactly what the first run left them as.
	var status string
	if err := sqlDB.QueryRowContext(ctx, `SELECT status FROM quizzes WHERE id = $1`, quizID).Scan(&status); err != nil {
		return fmt.Errorf("read quiz status: %w", err)
	}
	if status == "draft" || status == "scheduled" {
		if _, err := qsvc.SetAssignments(ctx, teacher, quizID, studentIDs, nil); err != nil {
			return fmt.Errorf("assign students: %w", err)
		}

		// starts_at a minute in the past: effectiveStatus already reads a
		// passed starts_at as live on every GET (docs/06 "lazy state
		// derivation"), so the herd script can start hitting POST
		// .../attempts immediately without waiting on the open_quiz job.
		// ends_at 30 days out and a 1h per-attempt duration comfortably
		// outlive any single load-test run.
		starts := time.Now().Add(-time.Minute)
		ends := time.Now().Add(30 * 24 * time.Hour)
		if _, err := qsvc.Publish(ctx, teacher, quizID, quiz.PublishInput{
			StartsAt:      starts,
			EndsAt:        ends,
			DurationSec:   3600,
			Guardrails:    quiz.DefaultGuardrails(),
			ReleasePolicy: "auto",
		}); err != nil {
			return fmt.Errorf("publish loadtest quiz: %w", err)
		}
	} else {
		log.Info("loadtest quiz already live, leaving assignments/window as-is", "status", status)
	}

	out := map[string]any{
		"teacher_email":         teacherEmail,
		"student_password":      password,
		"student_count":         len(studentIDs),
		"student_email_pattern": "loadtest-student-%05d@macquiz.load",
		"quiz_id":               quizID,
		"quiz_title":            quizTitle,
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	log.Info("loadtest fixtures ready", "quiz_id", quizID, "students", len(studentIDs))
	return enc.Encode(out)
}

// upsertLoadtestUser returns id, creating the account (active,
// must_change_password=false, so herd.js can log in with a fixed password
// with no first-login reset step in the way) only if the email is new.
func upsertLoadtestUser(ctx context.Context, sqlDB *sql.DB, role, email, fullName, hash, createdBy string) (string, error) {
	var id string
	err := sqlDB.QueryRowContext(ctx, `SELECT id FROM users WHERE email = $1`, email).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	err = sqlDB.QueryRowContext(ctx,
		`INSERT INTO users (role, email, password_hash, full_name, status, must_change_password, created_by)
		 VALUES ($1::user_role, $2, $3, $4, 'active', false, $5)
		 RETURNING id`,
		role, email, hash, fullName, createdBy).Scan(&id)
	if err != nil {
		return "", err
	}
	return id, nil
}

// upsertLoadtestStudents bulk-creates any missing loadtest-student-NNNNN
// accounts in one round trip (ON CONFLICT DO NOTHING on the unique email),
// then returns every matching id - not just the ones just inserted - so a
// rerun that lowers MACQUIZ_LOADTEST_STUDENTS still returns the full prior
// roster rather than silently shrinking the audience.
func upsertLoadtestStudents(ctx context.Context, sqlDB *sql.DB, count int, hash, createdBy string) ([]string, error) {
	if _, err := sqlDB.ExecContext(ctx, `
		INSERT INTO users (role, email, password_hash, full_name, status, must_change_password, created_by)
		SELECT 'student'::user_role,
		       'loadtest-student-' || lpad(gs::text, 5, '0') || '@macquiz.load',
		       $1, 'Load Test Student ' || lpad(gs::text, 5, '0'), 'active', false, $2
		FROM generate_series(1, $3) AS gs
		ON CONFLICT (email) DO NOTHING`,
		hash, createdBy, count); err != nil {
		return nil, fmt.Errorf("bulk insert students: %w", err)
	}

	rows, err := sqlDB.QueryContext(ctx,
		`SELECT id FROM users
		 WHERE role = 'student' AND email LIKE 'loadtest-student-%@macquiz.load'
		 ORDER BY email`)
	if err != nil {
		return nil, fmt.Errorf("list students: %w", err)
	}
	defer rows.Close()

	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan student id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// findOrCreateLoadtestQuiz returns the draft/scheduled quiz's id, creating it
// (and 10 single-choice questions, the minimum Publish needs) only the first
// time; a rerun against an already-scheduled quiz skips both since
// AddQuestion only accepts a draft-status quiz.
func findOrCreateLoadtestQuiz(ctx context.Context, sqlDB *sql.DB, qsvc *quiz.Service, teacher authusers.User, title string) (string, error) {
	var id string
	err := sqlDB.QueryRowContext(ctx,
		`SELECT id FROM quizzes WHERE owner_id = $1 AND title = $2`, teacher.ID, title).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		q, cerr := qsvc.CreateQuiz(ctx, teacher, title)
		if cerr != nil {
			return "", cerr
		}
		id = q.Id.String()
	case err != nil:
		return "", err
	}

	var questionCount int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM questions WHERE quiz_id = $1`, id).Scan(&questionCount); err != nil {
		return "", err
	}
	for i := questionCount; i < 10; i++ {
		in := loadtestQuestion(i)
		if fields := in.Validate(); len(fields) > 0 {
			return "", fmt.Errorf("loadtest question %d failed validation: %v", i, fields)
		}
		if _, err := qsvc.AddQuestion(ctx, teacher, id, in); err != nil {
			return "", fmt.Errorf("add question %d: %w", i, err)
		}
	}
	return id, nil
}

func loadtestQuestion(i int) quiz.QuestionInput {
	body, _ := json.Marshal(map[string]string{
		"text": fmt.Sprintf("Load test question %d: which option is correct?", i+1),
	})
	options, _ := json.Marshal([]map[string]string{
		{"key": "a", "text": "Option A"},
		{"key": "b", "text": "Option B"},
		{"key": "c", "text": "Option C"},
		{"key": "d", "text": "Option D"},
	})
	correct, _ := json.Marshal("a")
	points := 1.0
	return quiz.QuestionInput{Type: "single", Body: body, Options: options, Correct: correct, Points: &points}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
