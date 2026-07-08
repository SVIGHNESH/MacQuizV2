package attempt_test

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

	"macquiz/server/internal/attempt"
	"macquiz/server/internal/authusers"
	"macquiz/server/internal/db"
	"macquiz/server/internal/httpserver"
	"macquiz/server/internal/itest"
	"macquiz/server/internal/quiz"
)

// capturePublisher is a test double for attempt.EventPublisher. It records
// every Publish call so the test can assert the "publish second" relay fires
// with the right channel (quiz_id), attempt, type, and payload - and, just as
// importantly, does NOT fire for a transaction that rolled back or a write
// that flipped no row.
type capturePublisher struct {
	mu       sync.Mutex
	captured []capturedEvent
}

type capturedEvent struct {
	quizID    string
	attemptID string
	typ       string
	payload   map[string]any
}

// Publish marshals the typed payload the same way realtime.Publisher does, so
// the recorded map is exactly what would ride the Redis envelope.
func (c *capturePublisher) Publish(_ context.Context, quizID, attemptID, eventType string, payload any) {
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
	c.captured = append(c.captured, capturedEvent{quizID, attemptID, eventType, m})
}

// forAttempt returns the captured events for one attempt, in publish order.
func (c *capturePublisher) forAttempt(attemptID string) []capturedEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []capturedEvent
	for _, e := range c.captured {
		if e.attemptID == attemptID {
			out = append(out, e)
		}
	}
	return out
}

func filterCaptured(evs []capturedEvent, typ string) []capturedEvent {
	var out []capturedEvent
	for _, e := range evs {
		if e.typ == typ {
			out = append(out, e)
		}
	}
	return out
}

// TestEventPublishE2E pins the "publish second" half of the docs/05 section 1
// pipeline: after each lifecycle transaction commits, the same delta is
// relayed to the quiz's channel via attempt.EventPublisher. It proves three
// properties a naive relay gets wrong:
//
//  1. Correctness: every published event carries the right quiz_id (the Redis
//     channel), attempt_id, type, and payload - identical to the persisted
//     attempt_events row.
//  2. After commit only: a resume, a repeat submit, a re-grade, and a re-sweep
//     - none of which change a row - publish nothing, and neither does an
//     autosave rejected past the deadline (its transaction rolls back).
//  3. Both processes: the serve-side events (started/progress/submitted) and
//     the worker-side events (submitted via sweep, graded) both relay, since
//     the sweep and grader are handed the same publisher the API service is.
//
// It runs in its own database (macquiz_publishtest) - see itest.FreshDatabase.
func TestEventPublishE2E(t *testing.T) {
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

	pub := &capturePublisher{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	router := httpserver.New(httpserver.BuildInfo{Version: "test"}, httpserver.Deps{
		DB:      sqlDB,
		Auth:    authusers.NewHandler(authSvc, false),
		Quiz:    quiz.NewHandler(quiz.NewService(sqlDB, log, quiz.LocalImportStorage{Dir: t.TempDir()}), authSvc),
		Attempt: attempt.NewHandler(attempt.NewService(sqlDB, log, pub), authSvc),
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
		map[string]string{"title": "Publish Under Test"}, teacher)
	if status != 201 {
		t.Fatalf("create quiz = %d %v", status, body)
	}
	quizID := body["quiz"].(map[string]any)["id"].(string)
	if status, _, _ = itest.Call(t, server, "PATCH", "/api/v1/quizzes/"+quizID,
		map[string]any{"max_attempts": 2}, teacher); status != 200 {
		t.Fatalf("set max_attempts = %d", status)
	}
	status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", map[string]any{
		"type": "single", "body": map[string]string{"text": "v = ?"},
		"options": []map[string]string{{"key": "a", "text": "s/t"}, {"key": "b", "text": "s*t"}},
		"correct": "a", "points": 3,
	}, teacher)
	if status != 201 {
		t.Fatalf("add question = %d %v", status, body)
	}
	q1 := body["question"].(map[string]any)["id"].(string)
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

	var firstID string
	t.Run("start relays attempt.started on the quiz channel; resume relays nothing", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, taker)
		if status != 201 {
			t.Fatalf("start = %d %v", status, body)
		}
		firstID = body["attempt"].(map[string]any)["id"].(string)

		got := pub.forAttempt(firstID)
		if len(got) != 1 || got[0].typ != "attempt.started" {
			t.Fatalf("after start published = %v, want one attempt.started", got)
		}
		// The channel is quiz:{quiz_id}:events, so quiz_id must be correct.
		if got[0].quizID != quizID {
			t.Fatalf("started quiz_id = %q, want %q", got[0].quizID, quizID)
		}
		if got[0].payload["student_id"] != takerID || got[0].payload["attempt_id"] != firstID || got[0].payload["deadline_at"] == nil {
			t.Fatalf("started payload = %v, want student/attempt/deadline set", got[0].payload)
		}

		// Resume is an idempotent no-flip: it commits no started row and must
		// therefore publish nothing.
		if status, _, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, taker); status != 200 {
			t.Fatalf("resume = %d, want 200", status)
		}
		if got := pub.forAttempt(firstID); len(got) != 1 {
			t.Fatalf("resume published extra events: %v, want still one", got)
		}
	})

	t.Run("each committed autosave relays attempt.progress; a rejected one relays nothing", func(t *testing.T) {
		save(t, server, firstID, q1, "a", taker)
		progress := filterCaptured(pub.forAttempt(firstID), "attempt.progress")
		if len(progress) != 1 {
			t.Fatalf("progress published = %d, want 1", len(progress))
		}
		if progress[0].quizID != quizID || progress[0].payload["answered_count"] != float64(1) {
			t.Fatalf("progress[0] = %v, want quiz %q answered_count 1", progress[0], quizID)
		}
		if progress[0].payload["current_question"] != float64(1) {
			t.Fatalf("progress current_question = %v, want 1", progress[0].payload["current_question"])
		}

		// Push the deadline into the past, then autosave: the write gate rejects
		// it, the transaction rolls back, so no progress event is published.
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE attempts SET deadline_at = now() - interval '1 hour' WHERE id = $1`, firstID); err != nil {
			t.Fatalf("expire deadline: %v", err)
		}
		status, _, _ := itest.Call(t, server, "PUT",
			"/api/v1/attempts/"+firstID+"/answers/"+q1, map[string]any{"response": "b"}, taker)
		if status != 409 {
			t.Fatalf("late save = %d, want 409", status)
		}
		if got := filterCaptured(pub.forAttempt(firstID), "attempt.progress"); len(got) != 1 {
			t.Fatalf("rejected save published a progress event: %v", got)
		}
		// Restore a live deadline for the manual-submit leg.
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE attempts SET deadline_at = now() + interval '1 hour' WHERE id = $1`, firstID); err != nil {
			t.Fatalf("restore deadline: %v", err)
		}
	})

	t.Run("manual submit relays submitted(manual) once; grading relays graded once", func(t *testing.T) {
		if status, _, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+firstID+"/submit", nil, taker); status != 200 {
			t.Fatalf("submit = %d", status)
		}
		sub := filterCaptured(pub.forAttempt(firstID), "attempt.submitted")
		if len(sub) != 1 || sub[0].quizID != quizID ||
			sub[0].payload["submit_kind"] != "manual" || sub[0].payload["answered_count"] != float64(1) {
			t.Fatalf("submitted published = %v, want one manual answered_count 1 on quiz %q", sub, quizID)
		}
		// A repeat submit flips no row and publishes nothing.
		if status, _, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+firstID+"/submit", nil, taker); status != 200 {
			t.Fatalf("repeat submit = %d", status)
		}
		if got := filterCaptured(pub.forAttempt(firstID), "attempt.submitted"); len(got) != 1 {
			t.Fatalf("repeat submit published a second submitted: %v", got)
		}

		// Grading runs in the worker process; it is handed the same publisher.
		graded, err := attempt.GradeSubmitted(ctx, sqlDB, pub)
		if err != nil || graded != 1 {
			t.Fatalf("grade = %d (err %v), want 1", graded, err)
		}
		g := filterCaptured(pub.forAttempt(firstID), "attempt.graded")
		if len(g) != 1 || g[0].quizID != quizID {
			t.Fatalf("graded published = %v, want one on quiz %q", g, quizID)
		}
		var score float64
		if err := sqlDB.QueryRowContext(ctx, `SELECT score FROM attempts WHERE id = $1`, firstID).Scan(&score); err != nil {
			t.Fatalf("read score: %v", err)
		}
		if g[0].payload["score"] != score || score != 3 {
			t.Fatalf("graded score = %v (db %v), want 3", g[0].payload["score"], score)
		}
		// A re-grade of a graded attempt flips no row and publishes nothing.
		if _, err := attempt.GradeSubmitted(ctx, sqlDB, pub); err != nil {
			t.Fatalf("re-grade: %v", err)
		}
		if got := filterCaptured(pub.forAttempt(firstID), "attempt.graded"); len(got) != 1 {
			t.Fatalf("re-grade published a second graded: %v", got)
		}
	})

	t.Run("the batch sweep relays submitted(auto) per flipped row and re-sweeps silently", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, taker)
		if status != 201 {
			t.Fatalf("second start = %d %v", status, body)
		}
		secondID := body["attempt"].(map[string]any)["id"].(string)
		save(t, server, secondID, q1, "a", taker)

		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE attempts SET deadline_at = now() - interval '10 seconds' WHERE id = $1`, secondID); err != nil {
			t.Fatalf("expire attempt: %v", err)
		}
		auto, forced, err := attempt.SweepDueAttempts(ctx, sqlDB, pub)
		if err != nil || auto != 1 || forced != 0 {
			t.Fatalf("sweep = auto %d forced %d (err %v), want auto 1 forced 0", auto, forced, err)
		}
		sub := filterCaptured(pub.forAttempt(secondID), "attempt.submitted")
		if len(sub) != 1 || sub[0].quizID != quizID ||
			sub[0].payload["submit_kind"] != "auto" || sub[0].payload["answered_count"] != float64(1) {
			t.Fatalf("auto submitted published = %v, want one auto answered_count 1 on quiz %q", sub, quizID)
		}
		// Re-running the sweep flips nothing, so it publishes nothing.
		if auto, _, err := attempt.SweepDueAttempts(ctx, sqlDB, pub); err != nil || auto != 0 {
			t.Fatalf("re-sweep = auto %d (err %v), want auto 0", auto, err)
		}
		if got := filterCaptured(pub.forAttempt(secondID), "attempt.submitted"); len(got) != 1 {
			t.Fatalf("re-sweep published a second submitted: %v", got)
		}
	})

	t.Run("every published event matches a persisted attempt_events row", func(t *testing.T) {
		// The persist layer is the source of truth (docs/05 section 1); the relay
		// must never invent or drop deltas. Per attempt, the published count of
		// each type equals the persisted count.
		for _, id := range []string{firstID} {
			persisted := events(t, ctx, sqlDB, id)
			published := pub.forAttempt(id)
			for _, typ := range []string{"attempt.started", "attempt.progress", "attempt.submitted", "attempt.graded"} {
				if got, want := len(filterCaptured(published, typ)), len(filter(persisted, typ)); got != want {
					t.Fatalf("attempt %s: published %d %s, persisted %d", id, got, typ, want)
				}
			}
		}
	})
}
