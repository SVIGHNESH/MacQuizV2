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

// TestKickAndReadmitRateLimitPerTeacher pins the docs/08-security.md section 4
// / docs/04-api.md section 5 requirement that kick and readmit are
// rate-limited per teacher, to prevent kick storms. It hammers each endpoint
// well past the configured per-teacher limit (repeat calls on an
// already-kicked/-readmitted attempt are idempotent 200s, so the limiter -
// not the business logic - is what eventually answers 429 RATE_LIMITED)
// and checks the two endpoints' limiters are independent (a readmit-storm on
// one teacher does not also throttle that teacher's kicks).
//
// It runs in its own database (macquiz_ratelimittest) - see itest.FreshDatabase.
func TestKickAndReadmitRateLimitPerTeacher(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_ratelimittest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	router := httpserver.New(httpserver.BuildInfo{Version: "test"}, httpserver.Deps{
		DB:      sqlDB,
		Auth:    authusers.NewHandler(authSvc, false),
		Quiz:    quiz.NewHandler(quiz.NewService(sqlDB, log, quiz.LocalImportStorage{Dir: t.TempDir()}), authSvc),
		Attempt: attempt.NewHandler(attempt.NewService(sqlDB, log, &capturePublisher{}), authSvc),
	})
	server := httptest.NewServer(router)
	defer server.Close()

	if err := authSvc.EnsureBootstrapAdmin(ctx, "admin@school.test", "admin-password-1", "Root Admin"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	provision(t, ctx, sqlDB, "teacher", "owner@school.test")
	provision(t, ctx, sqlDB, "student", "taker@school.test")

	teacher := login(t, server, "owner@school.test", "account-password")
	taker := login(t, server, "taker@school.test", "account-password")
	takerID := userID(t, ctx, sqlDB, "taker@school.test")

	status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
		map[string]string{"title": "Rate Limit Under Test"}, teacher)
	if status != 201 {
		t.Fatalf("create quiz = %d %v", status, body)
	}
	quizID := body["quiz"].(map[string]any)["id"].(string)
	if status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", map[string]any{
		"type": "truefalse", "body": map[string]string{"text": "Is this rate-limited?"},
		"correct": true, "points": 1,
	}, teacher); status != 201 {
		t.Fatalf("add question = %d %v", status, body)
	}
	if status, _, _ = itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
		map[string]any{"student_ids": []string{takerID}}, teacher); status != 200 {
		t.Fatalf("assign = %d", status)
	}
	if status, _, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", map[string]any{
		"starts_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"ends_at":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		"duration_sec": 120,
	}, teacher); status != 200 {
		t.Fatalf("publish = %d", status)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE quizzes SET starts_at = now() - interval '1 minute' WHERE id = $1`, quizID); err != nil {
		t.Fatalf("backdate starts_at: %v", err)
	}
	attemptID := start(t, server, quizID, taker)

	t.Run("kick is rate-limited per teacher", func(t *testing.T) {
		var status int
		var body map[string]any
		for i := range 21 {
			status, body, _ = itest.Call(t, server, "POST", "/api/v1/attempts/"+attemptID+"/kick",
				map[string]any{"reason": "kick storm test"}, teacher)
			if i < 20 && status != 200 {
				t.Fatalf("kick %d = %d %v, want 200 (repeat kicks are idempotent)", i+1, status, body)
			}
		}
		if status != 429 {
			t.Fatalf("21st kick within a minute = %d %v, want 429 RATE_LIMITED", status, body)
		}
		if body["code"] != "RATE_LIMITED" {
			t.Fatalf("21st kick body = %v, want code RATE_LIMITED", body)
		}
	})

	t.Run("readmit has its own independent per-teacher limit", func(t *testing.T) {
		var status int
		var body map[string]any
		for i := range 21 {
			status, body, _ = itest.Call(t, server, "POST", "/api/v1/attempts/"+attemptID+"/readmit",
				map[string]any{"reason": "readmit storm test"}, teacher)
			if i < 20 && status != 200 {
				t.Fatalf("readmit %d = %d %v, want 200 (repeat readmits are idempotent)", i+1, status, body)
			}
		}
		if status != 429 {
			t.Fatalf("21st readmit within a minute = %d %v, want 429 RATE_LIMITED", status, body)
		}
	})
}
