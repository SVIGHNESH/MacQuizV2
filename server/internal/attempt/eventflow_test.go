package attempt_test

import (
	"context"
	"database/sql"
	"encoding/json"
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

// TestEventFlowE2E pins the attempt_events source-of-truth layer (docs/05
// sections 1-2): every lifecycle write appends its typed event row in the same
// transaction as the state change, and the payloads carry exactly what the
// dashboard deltas need. It walks one attempt through started -> progress ->
// submitted(manual) -> graded, proves a resume emits no duplicate started, and
// then drives a second attempt through the batch sweep to prove auto-submit
// emits submitted(auto) per flipped row.
//
// It runs in its own database (macquiz_eventtest) - see itest.FreshDatabase.
func TestEventFlowE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_eventtest")
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
	provision(t, ctx, sqlDB, "student", "taker@school.test")
	provision(t, ctx, sqlDB, "student", "closer@school.test")

	teacher := login(t, server, "owner@school.test", "account-password")
	taker := login(t, server, "taker@school.test", "account-password")
	closer := login(t, server, "closer@school.test", "account-password")
	takerID := userID(t, ctx, sqlDB, "taker@school.test")
	closerID := userID(t, ctx, sqlDB, "closer@school.test")

	// A two-question quiz, two attempts allowed, a 2-minute per-attempt budget.
	status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
		map[string]string{"title": "Events Under Test"}, teacher)
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
		t.Fatalf("add question 1 = %d %v", status, body)
	}
	q1 := body["question"].(map[string]any)["id"].(string)
	status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", map[string]any{
		"type": "short", "body": map[string]string{"text": "Unit of force?"},
		"correct": map[string]any{"accepted": []string{"newton"}}, "points": 2,
	}, teacher)
	if status != 201 {
		t.Fatalf("add question 2 = %d %v", status, body)
	}
	q2 := body["question"].(map[string]any)["id"].(string)
	if status, _, _ = itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
		map[string]any{"student_ids": []string{takerID, closerID}}, teacher); status != 200 {
		t.Fatalf("assign = %d", status)
	}
	if status, _, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", map[string]any{
		"starts_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"ends_at":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		"duration_sec": 120,
	}, teacher); status != 200 {
		t.Fatalf("publish = %d", status)
	}
	// Backdate starts_at so the quiz reads live lazily, exactly as it would in
	// the gap before the scheduler open job lands.
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE quizzes SET starts_at = now() - interval '1 minute' WHERE id = $1`, quizID); err != nil {
		t.Fatalf("backdate starts_at: %v", err)
	}

	var firstID string
	t.Run("start emits one attempt.started; resume emits none", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, taker)
		if status != 201 {
			t.Fatalf("start = %d %v", status, body)
		}
		firstID = body["attempt"].(map[string]any)["id"].(string)

		evs := events(t, ctx, sqlDB, firstID)
		if len(evs) != 1 || evs[0].typ != "attempt.started" {
			t.Fatalf("after start events = %v, want one attempt.started", evs)
		}
		p := evs[0].payload
		if p["student_id"] != takerID || p["attempt_id"] != firstID || p["deadline_at"] == nil {
			t.Fatalf("started payload = %v, want student/attempt/deadline set", p)
		}

		// Resume: idempotent start burns no attempt and appends no event.
		if status, _, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, taker); status != 200 {
			t.Fatalf("resume = %d, want 200", status)
		}
		if evs := events(t, ctx, sqlDB, firstID); len(evs) != 1 {
			t.Fatalf("resume added events: %v, want still one", evs)
		}
	})

	t.Run("each autosave emits an honest attempt.progress", func(t *testing.T) {
		save(t, server, firstID, q1, "a", taker)
		save(t, server, firstID, q2, "newton", taker)
		// A re-save of q1 upserts one row, so the answered count holds at two.
		save(t, server, firstID, q1, "a", taker)

		progress := filter(events(t, ctx, sqlDB, firstID), "attempt.progress")
		if len(progress) != 3 {
			t.Fatalf("progress events = %d, want 3", len(progress))
		}
		wantCounts := []float64{1, 2, 2}
		for i, ev := range progress {
			if ev.payload["answered_count"] != wantCounts[i] {
				t.Fatalf("progress[%d] answered_count = %v, want %v", i, ev.payload["answered_count"], wantCounts[i])
			}
			// current_question is deliberately null over REST (no server cursor).
			if got, ok := ev.payload["current_question"]; !ok || got != nil {
				t.Fatalf("progress[%d] current_question = %v, want explicit null", i, got)
			}
		}
	})

	t.Run("manual submit then grade emit submitted(manual) and graded", func(t *testing.T) {
		if status, _, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+firstID+"/submit", nil, taker); status != 200 {
			t.Fatalf("submit = %d", status)
		}
		sub := filter(events(t, ctx, sqlDB, firstID), "attempt.submitted")
		if len(sub) != 1 || sub[0].payload["submit_kind"] != "manual" || sub[0].payload["answered_count"] != float64(2) {
			t.Fatalf("submitted events = %v, want one manual with answered_count 2", sub)
		}
		// A repeat submit is idempotent and appends no second event.
		if status, _, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+firstID+"/submit", nil, taker); status != 200 {
			t.Fatalf("repeat submit = %d", status)
		}
		if sub := filter(events(t, ctx, sqlDB, firstID), "attempt.submitted"); len(sub) != 1 {
			t.Fatalf("repeat submit added a submitted event: %v", sub)
		}

		graded, err := attempt.GradeSubmitted(ctx, sqlDB)
		if err != nil || graded != 1 {
			t.Fatalf("grade = %d (err %v), want 1", graded, err)
		}
		g := filter(events(t, ctx, sqlDB, firstID), "attempt.graded")
		if len(g) != 1 {
			t.Fatalf("graded events = %d, want 1", len(g))
		}
		// The graded event records exactly the score written to attempts.
		var score float64
		if err := sqlDB.QueryRowContext(ctx, `SELECT score FROM attempts WHERE id = $1`, firstID).Scan(&score); err != nil {
			t.Fatalf("read score: %v", err)
		}
		if g[0].payload["score"] != score {
			t.Fatalf("graded payload score = %v, want %v", g[0].payload["score"], score)
		}
		// Both answers correct: 3 + 2 points.
		if score != 5 {
			t.Fatalf("score = %v, want 5", score)
		}
		// A re-grade of an already-graded attempt appends nothing.
		if _, err := attempt.GradeSubmitted(ctx, sqlDB); err != nil {
			t.Fatalf("re-grade: %v", err)
		}
		if g := filter(events(t, ctx, sqlDB, firstID), "attempt.graded"); len(g) != 1 {
			t.Fatalf("re-grade added a graded event: %v", g)
		}
	})

	t.Run("the batch sweep emits submitted(auto) per flipped row", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, taker)
		if status != 201 {
			t.Fatalf("second start = %d %v", status, body)
		}
		secondID := body["attempt"].(map[string]any)["id"].(string)
		save(t, server, secondID, q1, "a", taker)

		// The disappearing student: deadline in the past beyond the grace.
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE attempts SET deadline_at = now() - interval '10 seconds' WHERE id = $1`, secondID); err != nil {
			t.Fatalf("expire attempt: %v", err)
		}
		auto, forced, err := attempt.SweepDueAttempts(ctx, sqlDB)
		if err != nil || auto != 1 || forced != 0 {
			t.Fatalf("sweep = auto %d forced %d (err %v), want auto 1 forced 0", auto, forced, err)
		}
		sub := filter(events(t, ctx, sqlDB, secondID), "attempt.submitted")
		if len(sub) != 1 || sub[0].payload["submit_kind"] != "auto" || sub[0].payload["answered_count"] != float64(1) {
			t.Fatalf("auto submitted events = %v, want one auto with answered_count 1", sub)
		}
		// Re-running the sweep flips nothing, so it emits no second event.
		if auto, _, err := attempt.SweepDueAttempts(ctx, sqlDB); err != nil || auto != 0 {
			t.Fatalf("re-sweep = auto %d (err %v), want auto 0", auto, err)
		}
		if sub := filter(events(t, ctx, sqlDB, secondID), "attempt.submitted"); len(sub) != 1 {
			t.Fatalf("re-sweep added a submitted event: %v", sub)
		}
	})

	t.Run("closing a quiz force-submits open attempts and emits submitted(forced)", func(t *testing.T) {
		// A second student with a fresh, unexpired attempt: only the quiz
		// closing under them terminates it, so the sweep's forced branch (its
		// own aliased RETURNING subquery) is the one exercised here.
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, closer)
		if status != 201 {
			t.Fatalf("closer start = %d %v", status, body)
		}
		closerAttempt := body["attempt"].(map[string]any)["id"].(string)
		save(t, server, closerAttempt, q1, "a", closer)

		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET status = 'closed' WHERE id = $1`, quizID); err != nil {
			t.Fatalf("close quiz: %v", err)
		}
		auto, forced, err := attempt.SweepDueAttempts(ctx, sqlDB)
		if err != nil || auto != 0 || forced != 1 {
			t.Fatalf("close sweep = auto %d forced %d (err %v), want auto 0 forced 1", auto, forced, err)
		}
		sub := filter(events(t, ctx, sqlDB, closerAttempt), "attempt.submitted")
		if len(sub) != 1 || sub[0].payload["submit_kind"] != "forced" || sub[0].payload["answered_count"] != float64(1) {
			t.Fatalf("forced submitted events = %v, want one forced with answered_count 1", sub)
		}
	})
}

// event is one decoded attempt_events row, ordered by the bigserial id.
type event struct {
	typ     string
	payload map[string]any
}

// events reads every attempt_events row for one attempt in append order.
func events(t *testing.T, ctx context.Context, sqlDB *sql.DB, attemptID string) []event {
	t.Helper()
	rows, err := sqlDB.QueryContext(ctx,
		`SELECT type, payload FROM attempt_events WHERE attempt_id = $1 ORDER BY id`, attemptID)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	defer rows.Close()
	var evs []event
	for rows.Next() {
		var e event
		var raw []byte
		if err := rows.Scan(&e.typ, &raw); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		if err := json.Unmarshal(raw, &e.payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		evs = append(evs, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read events: %v", err)
	}
	return evs
}

// filter keeps only the events of one type, preserving order.
func filter(evs []event, typ string) []event {
	var out []event
	for _, e := range evs {
		if e.typ == typ {
			out = append(out, e)
		}
	}
	return out
}

// save autosaves one response and fails the test on any non-200.
func save(t *testing.T, server *httptest.Server, attemptID, questionID, response string, cookies map[string]string) {
	t.Helper()
	status, body, _ := itest.Call(t, server, "PUT",
		"/api/v1/attempts/"+attemptID+"/answers/"+questionID,
		map[string]any{"response": response}, cookies)
	if status != 200 {
		t.Fatalf("save %s = %d %v", questionID, status, body)
	}
}
