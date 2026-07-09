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

// TestLeaderboardFlowE2E pins the "dark island" leaderboard of
// docs/11-frontend-design-system.md section 4 (the frontend design doc's St5
// screen): every student's best graded attempt, ranked by accuracy with ties
// broken by time taken, behind the same release gate as the student's own
// released review.
//
// It runs in its own database (macquiz_leaderboardtest).
func TestLeaderboardFlowE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_leaderboardtest")
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
	// Three students: alpha and gamma tie on score (gamma finishes faster),
	// beta trails. Names are deliberately not in rank order, so a leaderboard
	// accidentally ordered by name cannot pass.
	provision(t, ctx, sqlDB, "student", "alpha@school.test")
	provision(t, ctx, sqlDB, "student", "beta@school.test")
	provision(t, ctx, sqlDB, "student", "gamma@school.test")

	teacher := login(t, server, "owner@school.test", "account-password")
	alpha := login(t, server, "alpha@school.test", "account-password")
	beta := login(t, server, "beta@school.test", "account-password")
	gamma := login(t, server, "gamma@school.test", "account-password")
	alphaUserID := userID(t, ctx, sqlDB, "alpha@school.test")
	betaUserID := userID(t, ctx, sqlDB, "beta@school.test")
	gammaUserID := userID(t, ctx, sqlDB, "gamma@school.test")

	status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
		map[string]string{"title": "Data handling essentials"}, teacher)
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
		"type": "truefalse", "body": map[string]string{"text": "Ranks update as attempts are graded."},
		"correct": true,
	})

	if status, _, _ := itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
		map[string]any{"student_ids": []string{alphaUserID, betaUserID, gammaUserID}}, teacher); status != 200 {
		t.Fatalf("assign = %d", status)
	}
	status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", map[string]any{
		"starts_at":      time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"ends_at":        time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		"duration_sec":   600,
		"release_policy": "manual",
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
		if status, body, _ := itest.Call(t, server, "PUT",
			"/api/v1/attempts/"+attemptID+"/answers/"+questionID,
			map[string]any{"response": response}, cookies); status != 200 {
			t.Fatalf("autosave = %d %v", status, body)
		}
	}
	submit := func(cookies map[string]string, attemptID string) {
		t.Helper()
		if status, body, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+attemptID+"/submit", nil, cookies); status != 200 {
			t.Fatalf("submit = %d %v", status, body)
		}
	}
	// Attempt duration is wall-clock, and the whole test runs in well under a
	// second, so the tie-break input is set explicitly rather than raced for.
	setTaken := func(attemptID string, seconds int) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE attempts SET started_at = submitted_at - make_interval(secs => $2) WHERE id = $1`,
			attemptID, seconds); err != nil {
			t.Fatalf("set time taken: %v", err)
		}
	}

	// Alpha and gamma both score 3/3; gamma took 60s, alpha 300s. Beta scores
	// 1/3.
	alphaAttempt := start(alpha)
	save(alpha, alphaAttempt, singleID, "b")
	save(alpha, alphaAttempt, truefalseID, true)
	submit(alpha, alphaAttempt)
	betaAttempt := start(beta)
	save(beta, betaAttempt, truefalseID, true)
	submit(beta, betaAttempt)
	gammaAttempt := start(gamma)
	save(gamma, gammaAttempt, singleID, "b")
	save(gamma, gammaAttempt, truefalseID, true)
	submit(gamma, gammaAttempt)
	setTaken(alphaAttempt, 300)
	setTaken(betaAttempt, 120)
	setTaken(gammaAttempt, 60)

	t.Run("the leaderboard answers 409 before grading lands", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/attempts/"+alphaAttempt+"/leaderboard", nil, alpha)
		if status != 409 || body["code"] != "RESULTS_NOT_RELEASED" {
			t.Fatalf("leaderboard before grading = %d %v, want 409 RESULTS_NOT_RELEASED", status, body)
		}
	})

	if graded, err := attempt.GradeSubmitted(ctx, sqlDB); err != nil || graded != 3 {
		t.Fatalf("grade = (%d, %v), want (3, nil)", graded, err)
	}

	t.Run("the leaderboard answers 409 before release", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/attempts/"+alphaAttempt+"/leaderboard", nil, alpha)
		if status != 409 || body["code"] != "RESULTS_NOT_RELEASED" {
			t.Fatalf("leaderboard before release = %d %v, want 409 RESULTS_NOT_RELEASED", status, body)
		}
	})

	// Close and release manually, as the teacher would.
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE quizzes SET ends_at = now() - interval '1 second' WHERE id = $1`, quizID); err != nil {
		t.Fatalf("backdate ends_at: %v", err)
	}
	if _, _, err := quiz.SweepDueQuizzes(ctx, sqlDB); err != nil {
		t.Fatalf("sweep quizzes: %v", err)
	}
	if status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/release-results", nil, teacher); status != 200 {
		t.Fatalf("release = %d %v", status, body)
	}

	entriesOf := func(body map[string]any) []map[string]any {
		t.Helper()
		raw, ok := body["entries"].([]any)
		if !ok {
			t.Fatalf("entries missing in %v", body)
		}
		out := make([]map[string]any, len(raw))
		for i, e := range raw {
			out[i] = e.(map[string]any)
		}
		return out
	}

	t.Run("ranks by accuracy, ties broken by time taken", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/attempts/"+alphaAttempt+"/leaderboard", nil, alpha)
		if status != 200 {
			t.Fatalf("leaderboard = %d %v", status, body)
		}
		if body["quiz_title"] != "Data handling essentials" {
			t.Fatalf("quiz_title = %v", body["quiz_title"])
		}
		if body["total"].(float64) != 3 {
			t.Fatalf("total = %v, want 3", body["total"])
		}
		entries := entriesOf(body)
		if len(entries) != 3 {
			t.Fatalf("entries = %d, want 3", len(entries))
		}
		// gamma (3/3 in 60s) outranks alpha (3/3 in 300s) outranks beta (1/3).
		wantIDs := []string{gammaUserID, alphaUserID, betaUserID}
		wantRanks := []float64{1, 2, 3}
		wantAccuracy := []float64{1, 1, 1.0 / 3.0}
		for i, e := range entries {
			if e["student_id"] != wantIDs[i] {
				t.Fatalf("entry %d student_id = %v, want %v", i, e["student_id"], wantIDs[i])
			}
			if e["rank"].(float64) != wantRanks[i] {
				t.Fatalf("entry %d rank = %v, want %v", i, e["rank"], wantRanks[i])
			}
			if got := e["accuracy"].(float64); got < wantAccuracy[i]-0.001 || got > wantAccuracy[i]+0.001 {
				t.Fatalf("entry %d accuracy = %v, want ~%v", i, got, wantAccuracy[i])
			}
			if wantSelf := e["student_id"] == alphaUserID; e["is_self"] != wantSelf {
				t.Fatalf("entry %d is_self = %v, want %v", i, e["is_self"], wantSelf)
			}
		}
		if entries[0]["full_name"] == "" {
			t.Fatalf("full_name missing: %v", entries[0])
		}
	})

	t.Run("is_self follows the reader", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/attempts/"+betaAttempt+"/leaderboard", nil, beta)
		if status != 200 {
			t.Fatalf("leaderboard = %d %v", status, body)
		}
		for _, e := range entriesOf(body) {
			if wantSelf := e["student_id"] == betaUserID; e["is_self"] != wantSelf {
				t.Fatalf("entry %v is_self = %v, want %v", e["student_id"], e["is_self"], wantSelf)
			}
		}
	})

	t.Run("another student's attempt reads 404, never 403", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/attempts/"+gammaAttempt+"/leaderboard", nil, alpha)
		if status != 404 || body["code"] != "NOT_FOUND" {
			t.Fatalf("cross-student leaderboard = %d %v, want 404 NOT_FOUND", status, body)
		}
	})

	t.Run("the teacher has no student leaderboard surface", func(t *testing.T) {
		if status, _, _ := itest.Call(t, server, "GET", "/api/v1/attempts/"+alphaAttempt+"/leaderboard", nil, teacher); status != 403 {
			t.Fatalf("teacher leaderboard = %d, want 403", status)
		}
	})
}
