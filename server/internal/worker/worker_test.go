package worker_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/config"
	"macquiz/server/internal/db"
	"macquiz/server/internal/itest"
	"macquiz/server/internal/quiz"
	"macquiz/server/internal/worker"
)

// TestWorkerOpensAndClosesQuiz drives the whole scheduler pipeline against a
// real Postgres: publish enqueues the window jobs, the running worker
// consumes them, and the STORED quiz row flips scheduled -> live -> closed
// at the window edges with no reader involved - the Milestone 3 exit
// criterion ("quiz goes live at starts_at with no manual action") at the
// process boundary.
//
// It runs in its own database (macquiz_workertest) - see itest.FreshDatabase.
func TestWorkerOpensAndClosesQuiz(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	const dbName = "macquiz_workertest"
	sqlDB := itest.FreshDatabase(t, ctx, baseURL, dbName)
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	if err := authSvc.EnsureBootstrapAdmin(ctx, "admin@school.test", "admin-password-1", "Root Admin"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}

	// Minimal fixture straight in SQL: a teacher-owned one-question quiz
	// with one assigned student. The HTTP publish path is already covered by
	// the quiz package's flow tests; this test is about the worker.
	var teacherID, studentID, quizID string
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO users (role, email, password_hash, full_name, created_by, must_change_password)
		 VALUES ('teacher', 'owner@school.test', 'x', 'Owner', (SELECT id FROM users WHERE role = 'admin'), false)
		 RETURNING id`).Scan(&teacherID); err != nil {
		t.Fatalf("insert teacher: %v", err)
	}
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO users (role, email, password_hash, full_name, created_by, must_change_password)
		 VALUES ('student', 'pupil@school.test', 'x', 'Pupil', (SELECT id FROM users WHERE role = 'admin'), false)
		 RETURNING id`).Scan(&studentID); err != nil {
		t.Fatalf("insert student: %v", err)
	}
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO quizzes (owner_id, title) VALUES ($1, 'Worker Checkpoint') RETURNING id`,
		teacherID).Scan(&quizID); err != nil {
		t.Fatalf("insert quiz: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO questions (quiz_id, position, type, body, correct)
		 VALUES ($1, 1, 'truefalse', '{"text": "Does the worker work?"}', 'true')`, quizID); err != nil {
		t.Fatalf("insert question: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO quiz_assignments (quiz_id, student_id, assigned_by)
		 VALUES ($1, $2, $3)`, quizID, studentID, teacherID); err != nil {
		t.Fatalf("insert assignment: %v", err)
	}

	// Publish through the real service so the window jobs are enqueued in
	// the publish transaction. The HTTP layer's future-window rule does not
	// apply at the service level, letting the test use a seconds-long window.
	quizSvc := quiz.NewService(sqlDB, log)
	startsAt := time.Now().Add(2 * time.Second).UTC()
	endsAt := startsAt.Add(3 * time.Second)
	teacher := authusers.User{ID: teacherID, Role: "teacher", Status: "active"}
	if _, err := quizSvc.Publish(ctx, teacher, quizID, quiz.PublishInput{
		StartsAt:    startsAt,
		EndsAt:      endsAt,
		DurationSec: 30,
		Guardrails:  quiz.DefaultGuardrails(),
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse database url: %v", err)
	}
	u.Path = "/" + dbName
	workerCtx, stopWorker := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- worker.Run(workerCtx, config.Config{
			DatabaseURL:   u.String(),
			ShutdownGrace: 10 * time.Second,
			Env:           "test",
		}, log)
	}()

	// River promotes scheduled jobs on a periodic maintenance tick, so the
	// flips land within a few seconds of the window edges - generous
	// deadlines keep the assertion about ordering, not latency.
	waitForStatus(t, ctx, sqlDB, quizID, "live", 45*time.Second)
	waitForStatus(t, ctx, sqlDB, quizID, "closed", 45*time.Second)

	stopWorker()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("worker run: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("worker did not stop after context cancel")
	}
}

// waitForStatus polls the STORED status column - not a derived read - until
// it reaches want, proving the worker (not lazy validation) flipped the row.
func waitForStatus(t *testing.T, ctx context.Context, sqlDB *sql.DB, quizID, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT status FROM quizzes WHERE id = $1`, quizID).Scan(&last); err != nil {
			t.Fatalf("read stored status: %v", err)
		}
		if last == want {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("stored status = %q after %s, want %q", last, timeout, want)
}
