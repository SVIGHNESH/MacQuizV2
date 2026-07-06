package attempt_test

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"macquiz/server/internal/attempt"
	"macquiz/server/internal/authusers"
	"macquiz/server/internal/db"
	"macquiz/server/internal/httpserver"
	"macquiz/server/internal/itest"
	"macquiz/server/internal/quiz"
)

// TestPlayerFlowE2E drives the Milestone 4 attempt-player backend over real
// HTTP and a real Postgres: the start transaction (assignment + window +
// max_attempts with the deadline precompute), idempotent restart, the
// serializer boundary on served questions, autosave upserts with the
// deadline and status gates, resume, and the manual leg of the submit
// funnel.
//
// It runs in its own database (macquiz_playertest) - see itest.FreshDatabase.
func TestPlayerFlowE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_playertest")
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
	provision(t, ctx, sqlDB, "student", "taker@school.test")
	provision(t, ctx, sqlDB, "student", "outsider@school.test")

	teacher := login(t, server, "owner@school.test", "account-password")
	taker := login(t, server, "taker@school.test", "account-password")
	outsider := login(t, server, "outsider@school.test", "account-password")
	takerID := userID(t, ctx, sqlDB, "taker@school.test")

	// The quiz under test: two questions, two allowed attempts, a 2-minute
	// per-attempt budget inside a 1-hour window.
	var quizID, questionID string
	status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
		map[string]string{"title": "Kinematics Checkpoint"}, teacher)
	if status != 201 {
		t.Fatalf("create quiz = %d %v", status, body)
	}
	quizID = body["quiz"].(map[string]any)["id"].(string)
	status, _, _ = itest.Call(t, server, "PATCH", "/api/v1/quizzes/"+quizID,
		map[string]any{"max_attempts": 2}, teacher)
	if status != 200 {
		t.Fatalf("set max_attempts = %d", status)
	}
	status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", map[string]any{
		"type": "single", "body": map[string]string{"text": "v = ?"},
		"options": []map[string]string{{"key": "a", "text": "s/t"}, {"key": "b", "text": "s*t"}},
		"correct": "a",
	}, teacher)
	if status != 201 {
		t.Fatalf("add question 1 = %d %v", status, body)
	}
	questionID = body["question"].(map[string]any)["id"].(string)
	status, _, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", map[string]any{
		"type": "short", "body": map[string]string{"text": "Unit of force?"},
		"correct": map[string]any{"accepted": []string{"newton"}},
	}, teacher)
	if status != 201 {
		t.Fatalf("add question 2 = %d", status)
	}
	status, _, _ = itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
		map[string]any{"student_ids": []string{takerID}}, teacher)
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

	start := func(cookies map[string]string) (int, map[string]any) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, cookies)
		return status, body
	}

	t.Run("start is refused before the window opens", func(t *testing.T) {
		status, body := start(taker)
		if status != 409 || body["code"] != "QUIZ_NOT_LIVE" {
			t.Fatalf("early start = %d %v, want 409 QUIZ_NOT_LIVE", status, body)
		}
	})

	// Open the window: backdate starts_at so the quiz reads live lazily,
	// exactly as it would in the gap before the scheduler job lands.
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE quizzes SET starts_at = now() - interval '1 minute' WHERE id = $1`, quizID); err != nil {
		t.Fatalf("backdate starts_at: %v", err)
	}

	t.Run("unassigned students and non-students cannot start", func(t *testing.T) {
		if status, body := start(outsider); status != 404 {
			t.Fatalf("outsider start = %d %v, want 404", status, body)
		}
		status, _, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, teacher)
		if status != 403 {
			t.Fatalf("teacher start = %d, want 403", status)
		}
	})

	var attemptID string
	t.Run("start precomputes the deadline and strips the answer key", func(t *testing.T) {
		status, body := start(taker)
		if status != 201 {
			t.Fatalf("start = %d %v, want 201", status, body)
		}
		a := body["attempt"].(map[string]any)
		attemptID = a["id"].(string)
		if a["status"] != "in_progress" || a["attempt_no"].(float64) != 1 || a["quiz_version"].(float64) != 1 {
			t.Fatalf("attempt = %v, want in_progress no 1 at version 1", a)
		}
		started, err1 := time.Parse(time.RFC3339, a["started_at"].(string))
		deadline, err2 := time.Parse(time.RFC3339, a["deadline_at"].(string))
		if err1 != nil || err2 != nil {
			t.Fatalf("parse times: %v %v", err1, err2)
		}
		// The window has ~1 h 59 m left, so the 120 s budget wins.
		if got := deadline.Sub(started); got != 120*time.Second {
			t.Fatalf("deadline - started = %v, want 120s", got)
		}
		if body["now"] == nil || body["guardrails"] == nil || body["quiz_title"] != "Kinematics Checkpoint" {
			t.Fatalf("player payload incomplete: %v", body)
		}
		questions := body["questions"].([]any)
		if len(questions) != 2 {
			t.Fatalf("questions = %d, want 2", len(questions))
		}
		for _, q := range questions {
			if _, leaked := q.(map[string]any)["correct"]; leaked {
				t.Fatalf("player question leaks the answer key: %v", q)
			}
		}
	})

	t.Run("starting again resumes the open attempt", func(t *testing.T) {
		status, body := start(taker)
		if status != 200 {
			t.Fatalf("restart = %d %v, want 200", status, body)
		}
		if got := body["attempt"].(map[string]any)["id"].(string); got != attemptID {
			t.Fatalf("restart returned attempt %s, want %s", got, attemptID)
		}
		var count int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM attempts WHERE quiz_id = $1`, quizID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("attempt rows = %d (err %v), want 1", count, err)
		}
	})

	t.Run("autosave upserts one row per question", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "PUT",
			"/api/v1/attempts/"+attemptID+"/answers/"+questionID,
			map[string]any{"response": "b", "time_spent_ms": 4000}, taker)
		if status != 200 {
			t.Fatalf("first save = %d %v", status, body)
		}
		status, body, _ = itest.Call(t, server, "PUT",
			"/api/v1/attempts/"+attemptID+"/answers/"+questionID,
			map[string]any{"response": "a", "time_spent_ms": 9000}, taker)
		if status != 200 || body["deadline_at"] == nil || body["now"] == nil {
			t.Fatalf("second save = %d %v, want 200 with clock fields", status, body)
		}
		var response string
		var count int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*), min(response #>> '{}') FROM attempt_answers WHERE attempt_id = $1`,
			attemptID).Scan(&count, &response); err != nil {
			t.Fatalf("read answers: %v", err)
		}
		if count != 1 || response != "a" {
			t.Fatalf("answers = %d rows, response %q; want 1 row holding %q", count, response, "a")
		}
	})

	t.Run("autosave rejects garbage", func(t *testing.T) {
		// A question outside this attempt's snapshot.
		status, _, _ := itest.Call(t, server, "PUT",
			"/api/v1/attempts/"+attemptID+"/answers/00000000-0000-0000-0000-000000000001",
			map[string]any{"response": "a"}, taker)
		if status != 404 {
			t.Fatalf("save to foreign question = %d, want 404", status)
		}
		// An oversized response: not a quiz answer.
		status, _, _ = itest.Call(t, server, "PUT",
			"/api/v1/attempts/"+attemptID+"/answers/"+questionID,
			map[string]any{"response": strings.Repeat("x", 17*1024)}, taker)
		if status != 422 {
			t.Fatalf("oversized save = %d, want 422", status)
		}
		// The owner check answers 404, never 403.
		status, _, _ = itest.Call(t, server, "PUT",
			"/api/v1/attempts/"+attemptID+"/answers/"+questionID,
			map[string]any{"response": "a"}, outsider)
		if status != 404 {
			t.Fatalf("foreign save = %d, want 404", status)
		}
	})

	t.Run("resume returns saved answers and the server clock", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/attempts/"+attemptID, nil, taker)
		if status != 200 {
			t.Fatalf("resume = %d %v", status, body)
		}
		answers := body["answers"].([]any)
		if len(answers) != 1 {
			t.Fatalf("answers = %d, want 1", len(answers))
		}
		ans := answers[0].(map[string]any)
		if ans["question_id"] != questionID || ans["response"] != "a" || ans["time_spent_ms"].(float64) != 9000 {
			t.Fatalf("resumed answer = %v", ans)
		}
		if body["now"] == nil {
			t.Fatal("resume payload has no server clock")
		}
		status, _, _ = itest.Call(t, server, "GET", "/api/v1/attempts/"+attemptID, nil, outsider)
		if status != 404 {
			t.Fatalf("foreign resume = %d, want 404", status)
		}
	})

	t.Run("manual submit is idempotent and locks writes", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+attemptID+"/submit", nil, taker)
		if status != 200 {
			t.Fatalf("submit = %d %v", status, body)
		}
		a := body["attempt"].(map[string]any)
		if a["status"] != "submitted" || a["submit_kind"] != "manual" || a["submitted_at"] == nil {
			t.Fatalf("submitted attempt = %v", a)
		}
		status, body, _ = itest.Call(t, server, "PUT",
			"/api/v1/attempts/"+attemptID+"/answers/"+questionID,
			map[string]any{"response": "b"}, taker)
		if status != 409 || body["code"] != "ATTEMPT_ALREADY_SUBMITTED" {
			t.Fatalf("save after submit = %d %v, want 409 ATTEMPT_ALREADY_SUBMITTED", status, body)
		}
		// A repeat submit (double-click, retried request) answers 200 with
		// the same terminal state - the funnel is idempotent.
		status, body, _ = itest.Call(t, server, "POST", "/api/v1/attempts/"+attemptID+"/submit", nil, taker)
		if status != 200 || body["attempt"].(map[string]any)["status"] != "submitted" {
			t.Fatalf("repeat submit = %d %v, want 200 submitted", status, body)
		}
	})

	var secondID string
	t.Run("the deadline gates every write", func(t *testing.T) {
		status, body := start(taker)
		if status != 201 {
			t.Fatalf("second attempt = %d %v, want 201", status, body)
		}
		a := body["attempt"].(map[string]any)
		secondID = a["id"].(string)
		if a["attempt_no"].(float64) != 2 {
			t.Fatalf("attempt_no = %v, want 2", a["attempt_no"])
		}
		// Simulate the disappearing student: the deadline passed 10 s ago
		// (beyond the 5 s grace) with the row still in_progress.
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE attempts SET deadline_at = now() - interval '10 seconds' WHERE id = $1`, secondID); err != nil {
			t.Fatalf("expire attempt: %v", err)
		}
		status, body, _ = itest.Call(t, server, "PUT",
			"/api/v1/attempts/"+secondID+"/answers/"+questionID,
			map[string]any{"response": "a"}, taker)
		if status != 409 || body["code"] != "ATTEMPT_DEADLINE_PASSED" {
			t.Fatalf("late save = %d %v, want 409 ATTEMPT_DEADLINE_PASSED", status, body)
		}
		status, body, _ = itest.Call(t, server, "POST", "/api/v1/attempts/"+secondID+"/submit", nil, taker)
		if status != 409 || body["code"] != "ATTEMPT_DEADLINE_PASSED" {
			t.Fatalf("late manual submit = %d %v, want 409 ATTEMPT_DEADLINE_PASSED", status, body)
		}
	})

	t.Run("max_attempts is exhausted", func(t *testing.T) {
		// Both slots are used (one submitted, one expired), so a third start
		// is refused rather than resuming the dead row.
		status, body := start(taker)
		if status != 409 || body["code"] != "ATTEMPT_LIMIT_REACHED" {
			t.Fatalf("third start = %d %v, want 409 ATTEMPT_LIMIT_REACHED", status, body)
		}
	})

	t.Run("kicked attempts answer the lockout code", func(t *testing.T) {
		// The kick endpoint is a later Milestone 4/6 item; the write gate it
		// relies on must already hold.
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE attempts SET status = 'kicked' WHERE id = $1`, secondID); err != nil {
			t.Fatalf("mark kicked: %v", err)
		}
		status, body, _ := itest.Call(t, server, "PUT",
			"/api/v1/attempts/"+secondID+"/answers/"+questionID,
			map[string]any{"response": "a"}, taker)
		if status != 409 || body["code"] != "ATTEMPT_KICKED" {
			t.Fatalf("save on kicked = %d %v, want 409 ATTEMPT_KICKED", status, body)
		}
		status, body, _ = itest.Call(t, server, "POST", "/api/v1/attempts/"+secondID+"/submit", nil, taker)
		if status != 409 || body["code"] != "ATTEMPT_KICKED" {
			t.Fatalf("submit on kicked = %d %v, want 409 ATTEMPT_KICKED", status, body)
		}
	})
}

// provision inserts an account with a known password and no forced reset,
// standing in for the POST /users + password-change dance so the test stays
// inside the login rate-limit budget.
func provision(t *testing.T, ctx context.Context, sqlDB *sql.DB, role, email string) {
	t.Helper()
	hash, err := authusers.HashPassword("account-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO users (role, email, password_hash, full_name, created_by, must_change_password)
		 VALUES ($1, $2, $3, $4, (SELECT id FROM users WHERE role = 'admin'), false)`,
		role, email, hash, email); err != nil {
		t.Fatalf("provision %s: %v", email, err)
	}
}

func login(t *testing.T, server *httptest.Server, email, password string) map[string]string {
	t.Helper()
	status, body, cookies := itest.Call(t, server, "POST", "/api/v1/auth/login",
		map[string]string{"email": email, "password": password}, nil)
	if status != 200 {
		t.Fatalf("login %s = %d %v, want 200", email, status, body)
	}
	return cookies
}

func userID(t *testing.T, ctx context.Context, sqlDB *sql.DB, email string) string {
	t.Helper()
	var id string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT id FROM users WHERE email = $1`, email).Scan(&id); err != nil {
		t.Fatalf("resolve %s: %v", email, err)
	}
	return id
}
