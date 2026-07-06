package attempt_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"macquiz/server/internal/attempt"
	"macquiz/server/internal/authusers"
	"macquiz/server/internal/db"
	"macquiz/server/internal/httpserver"
	"macquiz/server/internal/itest"
	"macquiz/server/internal/quiz"
)

// TestDeadlineFlowE2E pins the scheduler side of the attempt lifecycle
// (docs/06 sections 1-2): the deadline timer job enqueued inside the start
// transaction, the auto-submit of an attempt whose personal budget expired
// ("the disappearing student"), and the force-submit of attempts still open
// when their quiz closes - all through the idempotent SweepDueAttempts the
// worker's jobs, boot re-scan, and periodic backstop share.
//
// It runs in its own database (macquiz_deadlinetest) - see itest.FreshDatabase.
func TestDeadlineFlowE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_deadlinetest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	router := httpserver.New(httpserver.BuildInfo{Version: "test"}, httpserver.Deps{
		DB:      sqlDB,
		Auth:    authusers.NewHandler(authSvc, false),
		Quiz:    quiz.NewHandler(quiz.NewService(sqlDB, log), authSvc),
		Attempt: attempt.NewHandler(attempt.NewService(sqlDB, log), authSvc),
	})
	server := httptest.NewServer(router)
	defer server.Close()

	if err := authSvc.EnsureBootstrapAdmin(ctx, "admin@school.test", "admin-password-1", "Root Admin"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	provision(t, ctx, sqlDB, "teacher", "owner@school.test")
	provision(t, ctx, sqlDB, "student", "vanisher@school.test")
	provision(t, ctx, sqlDB, "student", "lingerer@school.test")

	teacher := login(t, server, "owner@school.test", "account-password")
	vanisher := login(t, server, "vanisher@school.test", "account-password")
	lingerer := login(t, server, "lingerer@school.test", "account-password")

	// The quiz under test: one question, a 2-minute per-attempt budget
	// inside a 2-hour window, assigned to both students.
	status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
		map[string]string{"title": "Deadline Drill"}, teacher)
	if status != 201 {
		t.Fatalf("create quiz = %d %v", status, body)
	}
	quizID := body["quiz"].(map[string]any)["id"].(string)
	status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", map[string]any{
		"type": "truefalse", "body": map[string]string{"text": "Time waits for no student."},
		"correct": true,
	}, teacher)
	if status != 201 {
		t.Fatalf("add question = %d %v", status, body)
	}
	questionID := body["question"].(map[string]any)["id"].(string)
	status, _, _ = itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
		map[string]any{"student_ids": []string{
			userID(t, ctx, sqlDB, "vanisher@school.test"),
			userID(t, ctx, sqlDB, "lingerer@school.test"),
		}}, teacher)
	if status != 200 {
		t.Fatalf("assign = %d", status)
	}
	status, _, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", map[string]any{
		"starts_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"ends_at":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		"duration_sec": 120,
	}, teacher)
	if status != 200 {
		t.Fatalf("publish = %d", status)
	}
	// Open the window: backdate starts_at so starts read live lazily.
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE quizzes SET starts_at = now() - interval '1 minute' WHERE id = $1`, quizID); err != nil {
		t.Fatalf("backdate starts_at: %v", err)
	}

	start := func(cookies map[string]string) string {
		t.Helper()
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, cookies)
		if status != 200 && status != 201 {
			t.Fatalf("start = %d %v", status, body)
		}
		return body["attempt"].(map[string]any)["id"].(string)
	}
	attemptState := func(id string) (status string, kind *string) {
		t.Helper()
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT status, submit_kind FROM attempts WHERE id = $1`, id).Scan(&status, &kind); err != nil {
			t.Fatalf("read attempt %s: %v", id, err)
		}
		return status, kind
	}

	vanisherID := start(vanisher)
	lingererID := start(lingerer)

	t.Run("start commits the deadline timer job with the attempt", func(t *testing.T) {
		var jobs int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM river_job j, attempts a
			 WHERE j.kind = 'attempt_deadline' AND j.args->>'attempt_id' = a.id::text
			   AND a.id = $1 AND j.scheduled_at = a.deadline_at + interval '5 seconds'`,
			vanisherID).Scan(&jobs); err != nil {
			t.Fatalf("count deadline jobs: %v", err)
		}
		if jobs != 1 {
			t.Fatalf("deadline jobs at deadline_at + grace = %d, want 1", jobs)
		}
		// An idempotent restart resumes; it must not enqueue a second timer.
		if again := start(vanisher); again != vanisherID {
			t.Fatalf("restart returned %s, want %s", again, vanisherID)
		}
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM river_job
			 WHERE kind = 'attempt_deadline' AND args->>'attempt_id' = $1`,
			vanisherID).Scan(&jobs); err != nil {
			t.Fatalf("recount deadline jobs: %v", err)
		}
		if jobs != 1 {
			t.Fatalf("deadline jobs after resume = %d, want 1", jobs)
		}
	})

	t.Run("the sweep is a no-op while everyone is inside their budget", func(t *testing.T) {
		auto, forced, err := attempt.SweepDueAttempts(ctx, sqlDB)
		if err != nil || auto != 0 || forced != 0 {
			t.Fatalf("sweep = (%d, %d, %v), want (0, 0, nil)", auto, forced, err)
		}
	})

	t.Run("the disappearing student is auto-submitted past deadline plus grace", func(t *testing.T) {
		// The student autosaved one answer, then closed the laptop; the
		// deadline (and its grace) passed with the row still in_progress.
		status, body, _ := itest.Call(t, server, "PUT",
			"/api/v1/attempts/"+vanisherID+"/answers/"+questionID,
			map[string]any{"response": true}, vanisher)
		if status != 200 {
			t.Fatalf("autosave = %d %v", status, body)
		}
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE attempts SET deadline_at = now() - interval '6 seconds' WHERE id = $1`, vanisherID); err != nil {
			t.Fatalf("expire attempt: %v", err)
		}
		auto, forced, err := attempt.SweepDueAttempts(ctx, sqlDB)
		if err != nil || auto != 1 || forced != 0 {
			t.Fatalf("sweep = (%d, %d, %v), want (1, 0, nil)", auto, forced, err)
		}
		if st, kind := attemptState(vanisherID); st != "submitted" || kind == nil || *kind != "auto" {
			t.Fatalf("vanisher attempt = %s/%v, want submitted/auto", st, kind)
		}
		if st, kind := attemptState(lingererID); st != "in_progress" || kind != nil {
			t.Fatalf("lingerer attempt = %s/%v, want untouched in_progress", st, kind)
		}
		// The autosaved answer survives for the grader; new writes and a
		// stale tab's manual submit are refused with the terminal code.
		status, body, _ = itest.Call(t, server, "PUT",
			"/api/v1/attempts/"+vanisherID+"/answers/"+questionID,
			map[string]any{"response": false}, vanisher)
		if status != 409 || body["code"] != "ATTEMPT_ALREADY_SUBMITTED" {
			t.Fatalf("save after auto-submit = %d %v, want 409 ATTEMPT_ALREADY_SUBMITTED", status, body)
		}
	})

	t.Run("quiz close force-submits the attempts still open", func(t *testing.T) {
		// The window ends while the lingerer's personal deadline is still
		// almost two minutes away; the close sweep flips the quiz, then the
		// attempt sweep force-submits what is left open.
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET ends_at = now() - interval '1 second' WHERE id = $1`, quizID); err != nil {
			t.Fatalf("backdate ends_at: %v", err)
		}
		if _, closed, err := quiz.SweepDueQuizzes(ctx, sqlDB); err != nil || closed != 1 {
			t.Fatalf("quiz sweep closed = %d (err %v), want 1", closed, err)
		}
		auto, forced, err := attempt.SweepDueAttempts(ctx, sqlDB)
		if err != nil || auto != 0 || forced != 1 {
			t.Fatalf("sweep = (%d, %d, %v), want (0, 1, nil)", auto, forced, err)
		}
		if st, kind := attemptState(lingererID); st != "submitted" || kind == nil || *kind != "forced" {
			t.Fatalf("lingerer attempt = %s/%v, want submitted/forced", st, kind)
		}
		// The already-terminated attempt keeps the kind its own timer wrote.
		if st, kind := attemptState(vanisherID); st != "submitted" || kind == nil || *kind != "auto" {
			t.Fatalf("vanisher attempt = %s/%v, want submitted/auto still", st, kind)
		}
	})

	t.Run("the sweep is idempotent once everything is terminal", func(t *testing.T) {
		auto, forced, err := attempt.SweepDueAttempts(ctx, sqlDB)
		if err != nil || auto != 0 || forced != 0 {
			t.Fatalf("repeat sweep = (%d, %d, %v), want (0, 0, nil)", auto, forced, err)
		}
	})
}
