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

// TestReviewFlowE2E pins the teacher's per-question attempt review (GET
// /attempts/:id/review): the drill-down behind one row of the owner's
// results table. Gated on quiz ownership, not release - the owner reads the
// detail as soon as grading lands (the same pre-release rationale as
// quiz.Results) - and on grading itself (409 ATTEMPT_NOT_GRADED before).
// Everyone but the owner reads 404 (admins included, matching the results
// table); students never clear the staff gate (403).
//
// It runs in its own database (macquiz_reviewtest) - see itest.FreshDatabase.
func TestReviewFlowE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_reviewtest")
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

	admin := login(t, server, "admin@school.test", "admin-password-1")
	owner := login(t, server, "owner@school.test", "account-password")
	other := login(t, server, "other@school.test", "account-password")
	alpha := login(t, server, "alpha@school.test", "account-password")
	beta := login(t, server, "beta@school.test", "account-password")
	alphaUserID := userID(t, ctx, sqlDB, "alpha@school.test")
	betaUserID := userID(t, ctx, sqlDB, "beta@school.test")

	// A two-question manual-release quiz: single worth 2, truefalse worth 1.
	status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
		map[string]string{"title": "Review Flow"}, owner)
	if status != 201 {
		t.Fatalf("create quiz = %d %v", status, body)
	}
	quizID := body["quiz"].(map[string]any)["id"].(string)
	addQuestion := func(q map[string]any) string {
		t.Helper()
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", q, owner)
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
	addQuestion(map[string]any{
		"type": "truefalse", "body": map[string]string{"text": "Reviews are gated."},
		"correct": true,
	})
	if status, _, _ := itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
		map[string]any{"student_ids": []string{alphaUserID, betaUserID}}, owner); status != 200 {
		t.Fatalf("assign = %d", status)
	}
	if status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish",
		map[string]any{
			"starts_at":      time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"ends_at":        time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
			"duration_sec":   600,
			"release_policy": "manual",
		}, owner); status != 200 {
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

	// Alpha answers the single correctly (1500 ms on the clock), skips the
	// truefalse entirely, submits, and gets graded. Beta starts but never
	// submits, so their attempt stays ungraded.
	alphaAttempt := start(alpha)
	if status, body, _ := itest.Call(t, server, "PUT",
		"/api/v1/attempts/"+alphaAttempt+"/answers/"+singleID,
		map[string]any{"response": "b", "time_spent_ms": 1500}, alpha); status != 200 {
		t.Fatalf("autosave = %d %v", status, body)
	}
	if status, body, _ := itest.Call(t, server, "POST",
		"/api/v1/attempts/"+alphaAttempt+"/submit", nil, alpha); status != 200 {
		t.Fatalf("submit = %d %v", status, body)
	}
	if graded, err := attempt.GradeSubmitted(ctx, sqlDB); err != nil || graded != 1 {
		t.Fatalf("grade = (%d, %v), want (1, nil)", graded, err)
	}
	betaAttempt := start(beta)

	t.Run("the owner reads the full review before any release", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/attempts/"+alphaAttempt+"/review", nil, owner)
		if status != 200 {
			t.Fatalf("review = %d %v", status, body)
		}
		if body["released_at"] != nil {
			t.Fatalf("released_at = %v, want null before release", body["released_at"])
		}
		// provision sets full_name to the email, so both identity fields
		// read the same value here.
		if body["full_name"] != "alpha@school.test" || body["email"] != "alpha@school.test" {
			t.Fatalf("student identity = %v / %v, want alpha@school.test for both",
				body["full_name"], body["email"])
		}
		if body["student_id"] != alphaUserID {
			t.Fatalf("student_id = %v, want %v", body["student_id"], alphaUserID)
		}
		if body["score"].(float64) != 2 || body["max_score"].(float64) != 3 {
			t.Fatalf("score = %v/%v, want 2/3", body["score"], body["max_score"])
		}
		questions := body["questions"].([]any)
		if len(questions) != 2 {
			t.Fatalf("questions = %d, want 2", len(questions))
		}
		var answered, skipped map[string]any
		for _, raw := range questions {
			q := raw.(map[string]any)
			if q["id"] == singleID {
				answered = q
			} else {
				skipped = q
			}
		}
		if answered == nil || skipped == nil {
			t.Fatalf("could not find both questions in %v", questions)
		}
		if answered["response"] != "b" || answered["is_correct"] != true {
			t.Fatalf("answered question = %v/%v, want response b graded correct",
				answered["response"], answered["is_correct"])
		}
		if answered["points_awarded"].(float64) != 2 {
			t.Fatalf("points_awarded = %v, want 2", answered["points_awarded"])
		}
		if answered["time_spent_ms"].(float64) != 1500 {
			t.Fatalf("time_spent_ms = %v, want 1500", answered["time_spent_ms"])
		}
		// A skip is absence, not a wrong answer: response, is_correct, and
		// time_spent_ms all read null, while the key stays visible.
		if skipped["response"] != nil || skipped["is_correct"] != nil || skipped["time_spent_ms"] != nil {
			t.Fatalf("skipped question = %v, want null response/is_correct/time_spent_ms", skipped)
		}
		if _, hasKey := skipped["correct"]; !hasKey {
			t.Fatalf("skipped question withholds the answer key: %v", skipped)
		}
	})

	t.Run("everyone but the owner is refused", func(t *testing.T) {
		cases := []struct {
			name    string
			cookies map[string]string
			target  string
			want    int
		}{
			{"a student never clears the staff gate", alpha, alphaAttempt, 403},
			{"a non-owning teacher reads 404", other, alphaAttempt, 404},
			{"an admin reads 404, like the results table", admin, alphaAttempt, 404},
			{"an unknown attempt reads 404", owner, "00000000-0000-0000-0000-000000000000", 404},
			{"a non-uuid id reads 404", owner, "not-a-uuid", 404},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				status, _, _ := itest.Call(t, server, "GET", "/api/v1/attempts/"+tc.target+"/review", nil, tc.cookies)
				if status != tc.want {
					t.Fatalf("review = %d, want %d", status, tc.want)
				}
			})
		}
	})

	t.Run("an ungraded attempt answers 409 ATTEMPT_NOT_GRADED", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/attempts/"+betaAttempt+"/review", nil, owner)
		if status != 409 || body["code"] != "ATTEMPT_NOT_GRADED" {
			t.Fatalf("ungraded review = %d %v, want 409 ATTEMPT_NOT_GRADED", status, body)
		}
	})
}
