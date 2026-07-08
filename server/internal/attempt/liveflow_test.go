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

// TestLiveRosterE2E pins the live-roster snapshot half of Milestone 5
// (docs/05 section 4): GET /quizzes/:id/live materializes one roster cell per
// assigned student from their latest attempt, collapsed to max(attempt_no) so
// a student with two attempts is one row (docs/05 section 6), with the roster
// state, answered count, and per-attempt max score. Authorization follows
// docs/05 section 3: the owning teacher or any admin, never a non-owning
// teacher, never a student.
//
// It runs in its own database (macquiz_livetest) - see itest.FreshDatabase.
func TestLiveRosterE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_livetest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	router := httpserver.New(httpserver.BuildInfo{Version: "test"}, httpserver.Deps{
		DB:      sqlDB,
		Auth:    authusers.NewHandler(authSvc, false),
		Quiz:    quiz.NewHandler(quiz.NewService(sqlDB, log, quiz.LocalImportStorage{Dir: t.TempDir()}), authSvc),
		Attempt: attempt.NewHandler(attempt.NewService(sqlDB, log), authSvc),
	})
	server := httptest.NewServer(router)
	defer server.Close()

	if err := authSvc.EnsureBootstrapAdmin(ctx, "admin@school.test", "admin-password-1", "Root Admin"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	provision(t, ctx, sqlDB, "teacher", "owner@school.test")
	provision(t, ctx, sqlDB, "teacher", "other@school.test")
	provision(t, ctx, sqlDB, "student", "alpha@school.test")
	provision(t, ctx, sqlDB, "student", "beta@school.test")
	provision(t, ctx, sqlDB, "student", "gamma@school.test")

	teacher := login(t, server, "owner@school.test", "account-password")
	other := login(t, server, "other@school.test", "account-password")
	admin := login(t, server, "admin@school.test", "admin-password-1")
	alpha := login(t, server, "alpha@school.test", "account-password")
	// beta never logs in - the roster must show it as not_started from the
	// assignment alone.
	gamma := login(t, server, "gamma@school.test", "account-password")
	alphaID := userID(t, ctx, sqlDB, "alpha@school.test")
	betaID := userID(t, ctx, sqlDB, "beta@school.test")
	gammaID := userID(t, ctx, sqlDB, "gamma@school.test")

	// A two-question quiz (single worth 2, truefalse worth 1) assigned to all
	// three students, backdated so attempts can start immediately.
	status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
		map[string]string{"title": "Live Roster"}, teacher)
	if status != 201 {
		t.Fatalf("create quiz = %d %v", status, body)
	}
	quizID := body["quiz"].(map[string]any)["id"].(string)
	addQuestion := func(q map[string]any) string {
		t.Helper()
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", q, teacher)
		if status != 201 {
			t.Fatalf("add question = %d %v", status, body)
		}
		return body["question"].(map[string]any)["id"].(string)
	}
	singleID := addQuestion(map[string]any{
		"type": "single", "body": map[string]string{"text": "Pick b."},
		"options": []map[string]string{{"key": "a", "text": "A"}, {"key": "b", "text": "B"}},
		"correct": "b", "points": 2,
	})
	truefalseID := addQuestion(map[string]any{
		"type": "truefalse", "body": map[string]string{"text": "Rosters are live."},
		"correct": true,
	})
	status, _, _ = itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
		map[string]any{"student_ids": []string{alphaID, betaID, gammaID}}, teacher)
	if status != 200 {
		t.Fatalf("assign = %d", status)
	}
	status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", map[string]any{
		"starts_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"ends_at":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		"duration_sec": 600,
	}, teacher)
	if status != 200 {
		t.Fatalf("publish = %d %v", status, body)
	}
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
	save := func(cookies map[string]string, attemptID, questionID string, response any) {
		t.Helper()
		status, body, _ := itest.Call(t, server, "PUT",
			"/api/v1/attempts/"+attemptID+"/answers/"+questionID,
			map[string]any{"response": response}, cookies)
		if status != 200 {
			t.Fatalf("autosave = %d %v", status, body)
		}
	}

	// State per student: alpha in progress with one saved answer, gamma
	// submitted, beta never starts (not_started).
	alphaAttempt := start(alpha)
	save(alpha, alphaAttempt, singleID, "b")
	gammaAttempt := start(gamma)
	save(gamma, gammaAttempt, singleID, "b")
	save(gamma, gammaAttempt, truefalseID, true)
	if status, body, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+gammaAttempt+"/submit", nil, gamma); status != 200 {
		t.Fatalf("submit = %d %v", status, body)
	}

	// Discriminator: alpha gets a stale earlier attempt inserted directly, so
	// the roster must collapse to the latest (attempt_no 1 in progress) and
	// still emit exactly one row for alpha.
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO attempts (quiz_id, student_id, attempt_no, quiz_version, started_at, deadline_at, submitted_at, submit_kind, status)
		 SELECT quiz_id, student_id, 0, quiz_version, started_at - interval '1 hour',
		        deadline_at - interval '1 hour', started_at - interval '1 hour', 'manual', 'submitted'
		 FROM attempts WHERE id = $1`, alphaAttempt); err != nil {
		t.Fatalf("insert stale attempt: %v", err)
	}

	rosterByStudent := func(cookies map[string]string) (map[string]map[string]any, int) {
		t.Helper()
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/quizzes/"+quizID+"/live", nil, cookies)
		if status != 200 {
			t.Fatalf("live roster = %d %v", status, body)
		}
		if body["server_time"] == nil {
			t.Fatalf("live roster missing server_time: %v", body)
		}
		rows := body["roster"].([]any)
		out := map[string]map[string]any{}
		for _, raw := range rows {
			r := raw.(map[string]any)
			out[r["student_id"].(string)] = r
		}
		return out, len(rows)
	}

	t.Run("the owner sees one row per student with the right state", func(t *testing.T) {
		roster, n := rosterByStudent(teacher)
		if n != 3 {
			t.Fatalf("roster rows = %d, want 3 (one per assigned student)", n)
		}
		if roster[betaID]["state"] != "not_started" || roster[betaID]["attempt_id"] != nil {
			t.Fatalf("beta = %v, want not_started with no attempt", roster[betaID])
		}
		if roster[alphaID]["state"] != "in_progress" {
			t.Fatalf("alpha state = %v, want in_progress", roster[alphaID]["state"])
		}
		if roster[alphaID]["attempt_no"].(float64) != 1 {
			t.Fatalf("alpha attempt_no = %v, want 1 (latest, not the stale 0)", roster[alphaID]["attempt_no"])
		}
		if roster[alphaID]["answered_count"].(float64) != 1 {
			t.Fatalf("alpha answered_count = %v, want 1", roster[alphaID]["answered_count"])
		}
		if roster[alphaID]["question_count"].(float64) != 2 || roster[alphaID]["max_score"].(float64) != 3 {
			t.Fatalf("alpha question_count/max_score = %v/%v, want 2/3",
				roster[alphaID]["question_count"], roster[alphaID]["max_score"])
		}
		if roster[alphaID]["current_question"] != float64(1) {
			t.Fatalf("alpha current_question = %v, want 1 (ordinal of the saved question)", roster[alphaID]["current_question"])
		}
		if roster[gammaID]["state"] != "submitted" || roster[gammaID]["answered_count"].(float64) != 2 {
			t.Fatalf("gamma = %v, want submitted with answered_count 2", roster[gammaID])
		}
	})

	t.Run("an admin can watch a quiz it does not own", func(t *testing.T) {
		_, n := rosterByStudent(admin)
		if n != 3 {
			t.Fatalf("admin roster rows = %d, want 3", n)
		}
	})

	t.Run("a non-owning teacher gets 404, not the roster", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/quizzes/"+quizID+"/live", nil, other)
		if status != 404 {
			t.Fatalf("non-owner teacher live = %d %v, want 404", status, body)
		}
	})

	t.Run("a student cannot watch the roster", func(t *testing.T) {
		status, _, _ := itest.Call(t, server, "GET", "/api/v1/quizzes/"+quizID+"/live", nil, alpha)
		if status != 403 {
			t.Fatalf("student live = %d, want 403", status)
		}
	})
}
