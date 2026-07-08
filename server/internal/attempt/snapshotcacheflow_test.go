package attempt_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"macquiz/server/internal/attempt"
	"macquiz/server/internal/authusers"
	"macquiz/server/internal/db"
	"macquiz/server/internal/httpserver"
	"macquiz/server/internal/itest"
	"macquiz/server/internal/quiz"
)

// captureSnapshotCache is a test double for attempt.SnapshotCache: an
// in-memory map plus per-key hit/miss/set counters, so a test can assert
// buildDetail actually reads through the cache instead of re-querying
// Postgres on every call.
type captureSnapshotCache struct {
	mu       sync.Mutex
	store    map[string][2][]byte
	getCalls int
	setCalls int
}

func newCaptureSnapshotCache() *captureSnapshotCache {
	return &captureSnapshotCache{store: map[string][2][]byte{}}
}

func snapCacheKey(quizID string, version int) string {
	return fmt.Sprintf("%s:%d", quizID, version)
}

func (c *captureSnapshotCache) Get(_ context.Context, quizID string, version int) ([]byte, []byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.getCalls++
	v, ok := c.store[snapCacheKey(quizID, version)]
	if !ok {
		return nil, nil, false
	}
	return v[0], v[1], true
}

func (c *captureSnapshotCache) Set(_ context.Context, quizID string, version int, questions, guardrails []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setCalls++
	c.store[snapCacheKey(quizID, version)] = [2][]byte{questions, guardrails}
}

// TestSnapshotCacheE2E pins the docs/01 "Go-live herd" read-through cache: the
// first buildDetail read for a (quiz, version) misses and populates the
// cache; every subsequent read for the same version - across both Start
// (resume) and Get - hits the cache and never calls Set again, even though
// each read still returns the correct questions and guardrails.
func TestSnapshotCacheE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_snapcachetest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	cache := newCaptureSnapshotCache()
	attemptSvc := attempt.NewService(sqlDB, log)
	attemptSvc.SetSnapshotCache(cache)
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
		map[string]string{"title": "Snapshot Cache Under Test"}, teacher)
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

	if cache.setCalls != 0 {
		t.Fatalf("setCalls before any read = %d, want 0", cache.setCalls)
	}

	// Start is the first buildDetail read for this (quiz, version): a miss
	// that populates the cache.
	status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, taker)
	if status != 201 {
		t.Fatalf("start = %d %v", status, body)
	}
	attemptID := body["attempt"].(map[string]any)["id"].(string)
	startQuestions := body["questions"]
	if cache.setCalls != 1 {
		t.Fatalf("setCalls after start = %d, want 1 (one miss populates the cache)", cache.setCalls)
	}

	// Resume (Start again) and a plain Get both read the same version: both
	// must hit the now-warm cache and never call Set again.
	if status, _, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, taker); status != 200 {
		t.Fatalf("resume = %d, want 200", status)
	}
	status, body, _ = itest.Call(t, server, "GET", "/api/v1/attempts/"+attemptID, nil, taker)
	if status != 200 {
		t.Fatalf("get = %d %v", status, body)
	}
	if cache.setCalls != 1 {
		t.Fatalf("setCalls after resume+get = %d, want still 1 (both reads should hit cache)", cache.setCalls)
	}
	if cache.getCalls < 3 {
		t.Fatalf("getCalls = %d, want at least 3 (start, resume, get)", cache.getCalls)
	}

	// The cache-served read must still return the real, correct snapshot.
	gotQuestions := body["questions"]
	if got, want := gotQuestions.([]any)[0].(map[string]any)["id"], startQuestions.([]any)[0].(map[string]any)["id"]; got != want {
		t.Fatalf("cached question id = %v, want %v", got, want)
	}
}
