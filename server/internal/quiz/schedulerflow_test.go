package quiz_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/db"
	"macquiz/server/internal/httpserver"
	"macquiz/server/internal/itest"
	"macquiz/server/internal/quiz"
)

// TestSchedulerFlowE2E pins the scheduler half of Milestone 3: publish
// enqueues the open_quiz/close_quiz River jobs in its own transaction with
// scheduled_at at the exact window edges, a refused publish leaves no orphan
// jobs, republish adds a fresh pair while the stale pair no-ops, and the
// due-transition sweep flips the stored rows scheduled -> live -> closed
// (including the skipped-window path straight to closed).
//
// It runs in its own database (macquiz_schedulertest) - see itest.FreshDatabase.
func TestSchedulerFlowE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_schedulertest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	quizSvc := quiz.NewService(sqlDB, log, quiz.LocalImportStorage{Dir: t.TempDir()})
	router := httpserver.New(httpserver.BuildInfo{Version: "test"}, httpserver.Deps{
		DB:   sqlDB,
		Auth: authusers.NewHandler(authSvc, false),
		Quiz: quiz.NewHandler(quizSvc, authSvc),
	})
	server := httptest.NewServer(router)
	defer server.Close()

	if err := authSvc.EnsureBootstrapAdmin(ctx, "admin@school.test", "admin-password-1", "Root Admin"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	provision(t, ctx, sqlDB, "teacher", "owner@school.test")
	provision(t, ctx, sqlDB, "student", "pupil@school.test")
	teacher := login(t, server, "owner@school.test", "account-password")
	pupilID := userID(t, ctx, sqlDB, "pupil@school.test")

	quizID := authorMinimalQuiz(t, server, teacher, "Scheduler Checkpoint")

	window := func(startsIn, endsIn time.Duration) map[string]any {
		return map[string]any{
			"starts_at":    time.Now().Add(startsIn).UTC().Format(time.RFC3339),
			"ends_at":      time.Now().Add(endsIn).UTC().Format(time.RFC3339),
			"duration_sec": 600,
		}
	}

	t.Run("a refused publish enqueues nothing", func(t *testing.T) {
		// No audience yet, so publish fails its precondition; the window jobs
		// ride the publish transaction and must roll back with it.
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish",
			window(time.Hour, 2*time.Hour), teacher)
		if status != 422 {
			t.Fatalf("publish without audience = %d %v, want 422", status, body)
		}
		if got := countWindowJobs(t, ctx, sqlDB, quizID); got != 0 {
			t.Fatalf("jobs after refused publish = %d, want 0", got)
		}
	})

	assign(t, server, teacher, quizID, pupilID)

	t.Run("publish enqueues both window jobs at the exact timestamps", func(t *testing.T) {
		w := window(time.Hour, 2*time.Hour)
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", w, teacher)
		if status != 200 {
			t.Fatalf("publish = %d %v", status, body)
		}
		for kind, at := range map[string]string{
			"open_quiz":  w["starts_at"].(string),
			"close_quiz": w["ends_at"].(string),
		} {
			var n int
			if err := sqlDB.QueryRowContext(ctx,
				`SELECT count(*) FROM river_job
				 WHERE kind = $1 AND args->>'quiz_id' = $2 AND scheduled_at = $3::timestamptz`,
				kind, quizID, at).Scan(&n); err != nil {
				t.Fatalf("count %s jobs: %v", kind, err)
			}
			if n != 1 {
				t.Fatalf("%s jobs at %s = %d, want 1", kind, at, n)
			}
		}
	})

	t.Run("republish adds a fresh pair for the new window", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish",
			window(3*time.Hour, 4*time.Hour), teacher)
		if status != 200 {
			t.Fatalf("republish = %d %v", status, body)
		}
		// The stale pair stays queued and will no-op against the sweep
		// predicates when it fires; the new pair matches the new window.
		if got := countWindowJobs(t, ctx, sqlDB, quizID); got != 4 {
			t.Fatalf("jobs after republish = %d, want 4 (stale pair + fresh pair)", got)
		}
	})

	t.Run("sweep leaves an undue quiz alone", func(t *testing.T) {
		opened, closed, err := quiz.SweepDueQuizzes(ctx, sqlDB)
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if opened != 0 || closed != 0 {
			t.Fatalf("sweep of undue quiz = %d opened, %d closed, want 0, 0", opened, closed)
		}
		if got := storedStatus(t, ctx, sqlDB, quizID); got != "scheduled" {
			t.Fatalf("stored status = %q, want scheduled", got)
		}
	})

	t.Run("sweep opens once starts_at passes", func(t *testing.T) {
		// Rewind the window instead of waiting: the sweep trusts only the
		// row's own timestamps, exactly as a fired job would find them.
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET starts_at = now() - interval '1 minute',
			                    ends_at = now() + interval '1 hour' WHERE id = $1`, quizID); err != nil {
			t.Fatalf("rewind starts_at: %v", err)
		}
		opened, closed, err := quiz.SweepDueQuizzes(ctx, sqlDB)
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if opened != 1 || closed != 0 {
			t.Fatalf("sweep = %d opened, %d closed, want 1, 0", opened, closed)
		}
		if got := storedStatus(t, ctx, sqlDB, quizID); got != "live" {
			t.Fatalf("stored status = %q, want live", got)
		}
	})

	t.Run("sweep closes once ends_at passes and stays settled", func(t *testing.T) {
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET ends_at = now() - interval '1 second' WHERE id = $1`, quizID); err != nil {
			t.Fatalf("rewind ends_at: %v", err)
		}
		opened, closed, err := quiz.SweepDueQuizzes(ctx, sqlDB)
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if opened != 0 || closed != 1 {
			t.Fatalf("sweep = %d opened, %d closed, want 0, 1", opened, closed)
		}
		if got := storedStatus(t, ctx, sqlDB, quizID); got != "closed" {
			t.Fatalf("stored status = %q, want closed", got)
		}

		// Idempotency: a second sweep (a late duplicate job) changes nothing.
		opened, closed, err = quiz.SweepDueQuizzes(ctx, sqlDB)
		if err != nil {
			t.Fatalf("second sweep: %v", err)
		}
		if opened != 0 || closed != 0 {
			t.Fatalf("second sweep = %d opened, %d closed, want 0, 0", opened, closed)
		}
	})

	t.Run("a fully elapsed window goes straight to closed", func(t *testing.T) {
		// The worker was down for the whole window: the quiz must never
		// read live on its way out.
		skippedID := authorMinimalQuiz(t, server, teacher, "Missed Window")
		assign(t, server, teacher, skippedID, pupilID)
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+skippedID+"/publish",
			window(time.Hour, 2*time.Hour), teacher)
		if status != 200 {
			t.Fatalf("publish = %d %v", status, body)
		}
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET starts_at = now() - interval '2 hours',
			                    ends_at = now() - interval '1 hour' WHERE id = $1`, skippedID); err != nil {
			t.Fatalf("rewind window: %v", err)
		}
		opened, closed, err := quiz.SweepDueQuizzes(ctx, sqlDB)
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if opened != 0 || closed != 1 {
			t.Fatalf("sweep = %d opened, %d closed, want 0, 1", opened, closed)
		}
		if got := storedStatus(t, ctx, sqlDB, skippedID); got != "closed" {
			t.Fatalf("stored status = %q, want closed", got)
		}
	})

	t.Run("publishing for today requests an exam-day backup, a future date does not", func(t *testing.T) {
		todayQuiz := authorMinimalQuiz(t, server, teacher, "Exam Today")
		assign(t, server, teacher, todayQuiz, pupilID)
		// A minute-scale offset, not window()'s usual hour scale: this
		// subtest's whole point is "starts_at falls on today's UTC date",
		// which an hours-long offset can silently violate (and flake) when
		// the suite happens to run within the last hour or two of the UTC
		// day - now()+2h would land after midnight UTC and no longer be
		// "today" by the time requestExamDayBackup checks it.
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+todayQuiz+"/publish",
			window(2*time.Minute, 5*time.Minute), teacher)
		if status != 200 {
			t.Fatalf("publish = %d %v", status, body)
		}
		if !backupTriggerExists(t, ctx, sqlDB, time.Now().UTC()) {
			t.Fatalf("expected a backup_triggers row for today's UTC date after publishing a quiz starting today")
		}

		// Republishing the same day-of quiz must not error on the trigger's
		// primary key - ON CONFLICT DO NOTHING keeps exactly one row.
		status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+todayQuiz+"/publish",
			window(3*time.Minute, 6*time.Minute), teacher)
		if status != 200 {
			t.Fatalf("republish = %d %v", status, body)
		}
		if got := backupTriggerCount(t, ctx, sqlDB, time.Now().UTC()); got != 1 {
			t.Fatalf("backup_triggers rows for today after republish = %d, want 1", got)
		}

		futureQuiz := authorMinimalQuiz(t, server, teacher, "Exam Next Month")
		assign(t, server, teacher, futureQuiz, pupilID)
		futureStart := time.Now().Add(30 * 24 * time.Hour)
		status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+futureQuiz+"/publish",
			map[string]any{
				"starts_at":    futureStart.UTC().Format(time.RFC3339),
				"ends_at":      futureStart.Add(time.Hour).UTC().Format(time.RFC3339),
				"duration_sec": 600,
			}, teacher)
		if status != 200 {
			t.Fatalf("publish future = %d %v", status, body)
		}
		if backupTriggerExists(t, ctx, sqlDB, futureStart.UTC()) {
			t.Fatalf("did not expect a backup_triggers row for a quiz starting 30 days out")
		}
	})
}

// authorMinimalQuiz creates a publishable one-question draft over HTTP.
func authorMinimalQuiz(t *testing.T, server *httptest.Server, teacher map[string]string, title string) string {
	t.Helper()
	status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
		map[string]string{"title": title}, teacher)
	if status != 201 {
		t.Fatalf("create quiz = %d %v", status, body)
	}
	id := body["quiz"].(map[string]any)["id"].(string)
	status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+id+"/questions", map[string]any{
		"type": "truefalse", "body": map[string]string{"text": "The scheduler is real now."},
		"correct": true,
	}, teacher)
	if status != 201 {
		t.Fatalf("add question = %d %v", status, body)
	}
	return id
}

func assign(t *testing.T, server *httptest.Server, teacher map[string]string, quizID, studentID string) {
	t.Helper()
	status, body, _ := itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
		map[string]any{"student_ids": []string{studentID}}, teacher)
	if status != 200 {
		t.Fatalf("assign = %d %v", status, body)
	}
}

func countWindowJobs(t *testing.T, ctx context.Context, sqlDB *sql.DB, quizID string) int {
	t.Helper()
	var n int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM river_job
		 WHERE kind IN ('open_quiz', 'close_quiz') AND args->>'quiz_id' = $1`, quizID).Scan(&n); err != nil {
		t.Fatalf("count window jobs: %v", err)
	}
	return n
}

func backupTriggerCount(t *testing.T, ctx context.Context, sqlDB *sql.DB, day time.Time) int {
	t.Helper()
	var n int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM backup_triggers WHERE trigger_date = $1::date`,
		day.Format("2006-01-02")).Scan(&n); err != nil {
		t.Fatalf("count backup_triggers: %v", err)
	}
	return n
}

func backupTriggerExists(t *testing.T, ctx context.Context, sqlDB *sql.DB, day time.Time) bool {
	t.Helper()
	return backupTriggerCount(t, ctx, sqlDB, day) > 0
}

func storedStatus(t *testing.T, ctx context.Context, sqlDB *sql.DB, quizID string) string {
	t.Helper()
	var status string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT status FROM quizzes WHERE id = $1`, quizID).Scan(&status); err != nil {
		t.Fatalf("read stored status: %v", err)
	}
	return status
}
