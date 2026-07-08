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

// TestSessionInvalidatedFlowE2E pins Service.LogSessionInvalidated, the piece
// the realtime gateway calls when a second device's attempt:{id} socket
// force-closes the first (docs/08 section 1: "single active session...
// logged as an event the teacher can see"). It proves the append-then-publish
// discipline every other lifecycle event follows, and that a race against an
// unknown attempt id is a quiet no-op rather than an error.
//
// It runs in its own database (macquiz_sessioninvalidatetest) - see
// itest.FreshDatabase.
func TestSessionInvalidatedFlowE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_sessioninvalidatetest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	pub := &capturePublisher{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	attemptSvc := attempt.NewService(sqlDB, log, pub)
	router := httpserver.New(httpserver.BuildInfo{Version: "test"}, httpserver.Deps{
		DB:      sqlDB,
		Auth:    authusers.NewHandler(authSvc, false),
		Quiz:    quiz.NewHandler(quiz.NewService(sqlDB, log, quiz.LocalImportStorage{Dir: t.TempDir()}), authSvc),
		Attempt: attempt.NewHandler(attemptSvc, authSvc),
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
		map[string]string{"title": "Session Under Test"}, teacher)
	if status != 201 {
		t.Fatalf("create quiz = %d %v", status, body)
	}
	quizID := body["quiz"].(map[string]any)["id"].(string)
	status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", map[string]any{
		"type": "single", "body": map[string]string{"text": "v = ?"},
		"options": []map[string]string{{"key": "a", "text": "s/t"}, {"key": "b", "text": "s*t"}},
		"correct": "a", "points": 3,
	}, teacher)
	if status != 201 {
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

	status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, taker)
	if status != 201 {
		t.Fatalf("start = %d %v", status, body)
	}
	attemptID := body["attempt"].(map[string]any)["id"].(string)

	t.Run("logs and publishes attempt.session_invalidated", func(t *testing.T) {
		if err := attemptSvc.LogSessionInvalidated(ctx, attemptID); err != nil {
			t.Fatalf("LogSessionInvalidated: %v", err)
		}
		persisted := filter(events(t, ctx, sqlDB, attemptID), "attempt.session_invalidated")
		if len(persisted) != 1 {
			t.Fatalf("persisted session_invalidated events = %d, want 1", len(persisted))
		}
		published := filterCaptured(pub.forAttempt(attemptID), "attempt.session_invalidated")
		if len(published) != 1 || published[0].quizID != quizID {
			t.Fatalf("published session_invalidated = %v, want one on quiz %q", published, quizID)
		}
	})

	t.Run("a second call appends a second row - every socket takeover is its own event", func(t *testing.T) {
		if err := attemptSvc.LogSessionInvalidated(ctx, attemptID); err != nil {
			t.Fatalf("LogSessionInvalidated: %v", err)
		}
		if got := filter(events(t, ctx, sqlDB, attemptID), "attempt.session_invalidated"); len(got) != 2 {
			t.Fatalf("persisted session_invalidated events = %d, want 2", len(got))
		}
	})

	t.Run("an unknown attempt id is a quiet no-op, not an error", func(t *testing.T) {
		if err := attemptSvc.LogSessionInvalidated(ctx, "00000000-0000-0000-0000-000000000000"); err != nil {
			t.Fatalf("LogSessionInvalidated for unknown attempt: %v", err)
		}
	})
}
