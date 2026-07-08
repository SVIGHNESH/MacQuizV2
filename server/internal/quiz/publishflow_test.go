package quiz_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/db"
	"macquiz/server/internal/httpserver"
	"macquiz/server/internal/itest"
	"macquiz/server/internal/quiz"
)

// capturePublisher is a test double for quiz.EventPublisher. It records every
// Publish call so the test can assert the "publish second" relay docs/05
// section 2 promises for quiz.extended/quiz.closed - and that a refused or
// idempotent no-op call (a non-owner, a second close on an already-closed
// quiz) never fires one.
type capturePublisher struct {
	mu       sync.Mutex
	captured []capturedEvent
}

type capturedEvent struct {
	quizID  string
	typ     string
	payload map[string]any
}

func (c *capturePublisher) Publish(_ context.Context, quizID, _ string, eventType string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		panic(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.captured = append(c.captured, capturedEvent{quizID: quizID, typ: eventType, payload: m})
}

func (c *capturePublisher) forType(typ string) []capturedEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []capturedEvent
	for _, e := range c.captured {
		if e.typ == typ {
			out = append(out, e)
		}
	}
	return out
}

// TestQuizWindowEventsE2E pins docs/05 section 2's "quiz.extended /
// quiz.closed | new ends_at | Banner to teacher and all in-progress students"
// row: Extend and ForceClose each relay one event, carrying the new ends_at,
// onto the quiz's channel after their transaction commits - and never for a
// call that changed nothing (a refused extend, a second force-close on an
// already-closed quiz).
//
// It runs in its own database (macquiz_publishtest) - see itest.FreshDatabase.
func TestQuizWindowEventsE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_publishtest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	publisher := &capturePublisher{}
	quizSvc := quiz.NewService(sqlDB, log, quiz.LocalImportStorage{Dir: t.TempDir()}, publisher)
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
	owner := login(t, server, "owner@school.test", "account-password")
	pupilID := userID(t, ctx, sqlDB, "pupil@school.test")

	quizID := authorMinimalQuiz(t, server, owner, "Live Session")
	assign(t, server, owner, quizID, pupilID)
	if status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish",
		map[string]any{
			"starts_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"ends_at":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
			"duration_sec": 600,
		}, owner); status != 200 {
		t.Fatalf("publish = %d %v", status, body)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE quizzes SET starts_at = now() - interval '1 minute',
		                    ends_at = now() + interval '1 hour' WHERE id = $1`, quizID); err != nil {
		t.Fatalf("rewind window: %v", err)
	}
	if _, _, err := quiz.SweepDueQuizzes(ctx, sqlDB); err != nil {
		t.Fatalf("sweep to live: %v", err)
	}

	t.Run("extend relays quiz.extended with the new ends_at", func(t *testing.T) {
		// Truncate to whole seconds: the request carries RFC3339 without
		// fractional seconds, so that is the precision that round-trips.
		newEndsAt := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/extend",
			map[string]any{"ends_at": newEndsAt.Format(time.RFC3339)}, owner)
		if status != 200 {
			t.Fatalf("extend = %d %v", status, body)
		}
		evs := publisher.forType("quiz.extended")
		if len(evs) != 1 {
			t.Fatalf("quiz.extended publishes = %d, want 1", len(evs))
		}
		if evs[0].quizID != quizID {
			t.Fatalf("quiz.extended quiz_id = %q, want %q", evs[0].quizID, quizID)
		}
		got, err := time.Parse(time.RFC3339, evs[0].payload["ends_at"].(string))
		if err != nil {
			t.Fatalf("parse ends_at: %v", err)
		}
		if !got.Equal(newEndsAt) {
			t.Fatalf("quiz.extended ends_at = %v, want %v", got, newEndsAt)
		}
	})

	t.Run("a refused extend publishes nothing new", func(t *testing.T) {
		before := len(publisher.forType("quiz.extended"))
		// Earlier than the already-extended ends_at: refused as a 422.
		status, _, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/extend",
			map[string]any{"ends_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}, owner)
		if status != 422 {
			t.Fatalf("extend earlier ends_at = %d, want 422", status)
		}
		if got := len(publisher.forType("quiz.extended")); got != before {
			t.Fatalf("quiz.extended publishes after refused extend = %d, want %d", got, before)
		}
	})

	t.Run("force-close relays quiz.closed with the new ends_at", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/close", nil, owner)
		if status != 200 {
			t.Fatalf("force-close = %d %v", status, body)
		}
		evs := publisher.forType("quiz.closed")
		if len(evs) != 1 {
			t.Fatalf("quiz.closed publishes = %d, want 1", len(evs))
		}
		if evs[0].quizID != quizID {
			t.Fatalf("quiz.closed quiz_id = %q, want %q", evs[0].quizID, quizID)
		}
		if _, ok := evs[0].payload["ends_at"]; !ok {
			t.Fatalf("quiz.closed payload missing ends_at: %v", evs[0].payload)
		}
	})

	t.Run("re-closing an already-closed quiz is idempotent and publishes nothing new", func(t *testing.T) {
		before := len(publisher.forType("quiz.closed"))
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/close", nil, owner)
		if status != 200 {
			t.Fatalf("re-close = %d %v", status, body)
		}
		if got := len(publisher.forType("quiz.closed")); got != before {
			t.Fatalf("quiz.closed publishes after idempotent re-close = %d, want %d", got, before)
		}
	})
}
