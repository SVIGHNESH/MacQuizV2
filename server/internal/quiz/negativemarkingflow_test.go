// The negative-marking integration test lives in an external test package so
// it can drive the real httpserver router, matching importcommitflow_test.go.
package quiz_test

import (
	"context"
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

// TestNegativeMarkingFlow pins the whole marking-scheme journey: the teacher
// sets quiz-wide defaults (marks per question + penalty per wrong answer), a
// question may override either, publish resolves the effective values into
// the version snapshot, and grading prices an answered-but-wrong question at
// its penalty - never a blank one - with the attempt total floored at zero.
//
// It runs in its own database (macquiz_negmarktest) - see itest.FreshDatabase.
func TestNegativeMarkingFlow(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_negmarktest")
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
	provision(t, ctx, sqlDB, "teacher", "marker@school.test")
	provision(t, ctx, sqlDB, "student", "mixed@school.test")
	provision(t, ctx, sqlDB, "student", "skipper@school.test")
	teacher := login(t, server, "marker@school.test", "account-password")
	mixed := login(t, server, "mixed@school.test", "account-password")
	skipper := login(t, server, "skipper@school.test", "account-password")

	var quizID string
	t.Run("a new draft starts with the vanilla scheme and accepts defaults", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
			map[string]string{"title": "Negative marking"}, teacher)
		if status != 201 {
			t.Fatalf("create quiz = %d %v", status, body)
		}
		q := body["quiz"].(map[string]any)
		quizID = q["id"].(string)
		if q["default_points"] != float64(1) || q["default_penalty"] != float64(0) {
			t.Fatalf("fresh defaults = %v/%v, want 1/0", q["default_points"], q["default_penalty"])
		}

		status, body, _ = itest.Call(t, server, "PATCH", "/api/v1/quizzes/"+quizID,
			map[string]any{"default_points": 4, "default_penalty": 1}, teacher)
		if status != 200 {
			t.Fatalf("set marking defaults = %d %v", status, body)
		}
		q = body["quiz"].(map[string]any)
		if q["default_points"] != float64(4) || q["default_penalty"] != float64(1) {
			t.Fatalf("patched defaults = %v/%v, want 4/1", q["default_points"], q["default_penalty"])
		}

		status, body, _ = itest.Call(t, server, "PATCH", "/api/v1/quizzes/"+quizID,
			map[string]any{"default_penalty": -1}, teacher)
		if status != 422 {
			t.Fatalf("negative default_penalty = %d %v, want 422", status, body)
		}
	})

	var inheritQ, overrideQ string
	t.Run("questions inherit unless they override", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions",
			map[string]any{
				"type": "truefalse", "body": map[string]any{"text": "Inherits the scheme"},
				"correct": true,
			}, teacher)
		if status != 201 {
			t.Fatalf("add inheriting question = %d %v", status, body)
		}
		q := body["question"].(map[string]any)
		inheritQ = q["id"].(string)
		if q["points"] != nil || q["penalty"] != nil {
			t.Fatalf("inheriting question points/penalty = %v/%v, want null/null", q["points"], q["penalty"])
		}

		status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions",
			map[string]any{
				"type": "single", "body": map[string]any{"text": "Overrides the scheme"},
				"options": []map[string]string{{"key": "a", "text": "A"}, {"key": "b", "text": "B"}},
				"correct": "b", "points": 6, "penalty": 2,
			}, teacher)
		if status != 201 {
			t.Fatalf("add overriding question = %d %v", status, body)
		}
		q = body["question"].(map[string]any)
		overrideQ = q["id"].(string)
		if q["points"] != float64(6) || q["penalty"] != float64(2) {
			t.Fatalf("overriding question points/penalty = %v/%v, want 6/2", q["points"], q["penalty"])
		}

		status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions",
			map[string]any{
				"type": "truefalse", "body": map[string]any{"text": "Bad penalty"},
				"correct": true, "penalty": 2000,
			}, teacher)
		if status != 422 {
			t.Fatalf("oversized penalty = %d %v, want 422", status, body)
		}
	})

	t.Run("publish resolves the effective scheme into the snapshot", func(t *testing.T) {
		studentIDs := []string{
			userID(t, ctx, sqlDB, "mixed@school.test"),
			userID(t, ctx, sqlDB, "skipper@school.test"),
		}
		if status, _, _ := itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
			map[string]any{"student_ids": studentIDs}, teacher); status != 200 {
			t.Fatalf("assign = %d", status)
		}
		if status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", map[string]any{
			"starts_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"ends_at":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
			"duration_sec": 600,
		}, teacher); status != 200 {
			t.Fatalf("publish = %d %v", status, body)
		}
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET starts_at = now() - interval '1 minute' WHERE id = $1`, quizID); err != nil {
			t.Fatalf("backdate starts_at: %v", err)
		}

		var raw []byte
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT questions FROM quiz_versions WHERE quiz_id = $1 AND version = 1`, quizID).Scan(&raw); err != nil {
			t.Fatalf("load snapshot: %v", err)
		}
		var snapshot []struct {
			ID      string  `json:"id"`
			Points  float64 `json:"points"`
			Penalty float64 `json:"penalty"`
		}
		if err := json.Unmarshal(raw, &snapshot); err != nil {
			t.Fatalf("decode snapshot: %v", err)
		}
		want := map[string][2]float64{
			inheritQ:  {4, 1}, // quiz defaults
			overrideQ: {6, 2}, // its own scheme
		}
		for _, sq := range snapshot {
			if w, ok := want[sq.ID]; !ok || sq.Points != w[0] || sq.Penalty != w[1] {
				t.Fatalf("snapshot question %s = %v/%v, want %v", sq.ID, sq.Points, sq.Penalty, w)
			}
		}
	})

	attemptFor := func(cookies map[string]string, answers map[string]any) string {
		t.Helper()
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, cookies)
		if status != 200 && status != 201 {
			t.Fatalf("start attempt = %d %v", status, body)
		}
		id := body["attempt"].(map[string]any)["id"].(string)
		for qid, resp := range answers {
			if status, b, _ := itest.Call(t, server, "PUT",
				"/api/v1/attempts/"+id+"/answers/"+qid,
				map[string]any{"response": resp}, cookies); status != 200 {
				t.Fatalf("autosave = %d %v", status, b)
			}
		}
		if status, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+id+"/submit", nil, cookies); status != 200 {
			t.Fatalf("submit = %d %v", status, b)
		}
		return id
	}

	t.Run("students see the negative-marking flag before starting", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/quizzes/assigned", nil, mixed)
		if status != 200 {
			t.Fatalf("assigned list = %d %v", status, body)
		}
		assigned := body["quizzes"].([]any)
		if len(assigned) != 1 {
			t.Fatalf("assigned = %d quizzes, want 1", len(assigned))
		}
		q := assigned[0].(map[string]any)
		if q["negative_marking"] != true {
			t.Fatalf("negative_marking = %v, want true (every question carries a penalty)", q["negative_marking"])
		}

		// The player payload carries each question's resolved stakes.
		status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, mixed)
		if status != 200 && status != 201 {
			t.Fatalf("start attempt = %d %v", status, body)
		}
		byID := map[string]map[string]any{}
		for _, raw := range body["questions"].([]any) {
			pq := raw.(map[string]any)
			byID[pq["id"].(string)] = pq
		}
		if pq := byID[inheritQ]; pq["points"] != float64(4) || pq["penalty"] != float64(1) {
			t.Fatalf("player inherit question = %v/%v, want 4/1", pq["points"], pq["penalty"])
		}
		if pq := byID[overrideQ]; pq["points"] != float64(6) || pq["penalty"] != float64(2) {
			t.Fatalf("player override question = %v/%v, want 6/2", pq["points"], pq["penalty"])
		}
	})

	// mixed: inherit-question wrong (-1), override-question right (+6) -> 5.
	// skipper: inherit-question wrong (-1), override UNANSWERED (no penalty)
	// -> -1, floored to 0.
	mixedAttempt := attemptFor(mixed, map[string]any{inheritQ: false, overrideQ: "b"})
	skipperAttempt := attemptFor(skipper, map[string]any{inheritQ: false})

	if graded, err := attempt.GradeSubmitted(ctx, sqlDB); err != nil || graded != 2 {
		t.Fatalf("grade = (%d, %v), want (2, nil)", graded, err)
	}

	t.Run("grading applies penalties to wrong answers only and floors at zero", func(t *testing.T) {
		score := func(attemptID string) float64 {
			t.Helper()
			var s float64
			if err := sqlDB.QueryRowContext(ctx,
				`SELECT score::float8 FROM attempts WHERE id = $1`, attemptID).Scan(&s); err != nil {
				t.Fatalf("load score: %v", err)
			}
			return s
		}
		if got := score(mixedAttempt); got != 5 {
			t.Fatalf("mixed score = %v, want 5 (-1 + 6)", got)
		}
		if got := score(skipperAttempt); got != 0 {
			t.Fatalf("skipper score = %v, want 0 (floored from -1)", got)
		}

		// The per-answer trail stays honest: the wrong answer carries its -1,
		// and the unanswered question has no points_awarded row at all.
		var awarded float64
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT points_awarded::float8 FROM attempt_answers
			 WHERE attempt_id = $1 AND question_id = $2`, skipperAttempt, inheritQ).Scan(&awarded); err != nil {
			t.Fatalf("load penalized answer: %v", err)
		}
		if awarded != -1 {
			t.Fatalf("penalized answer points_awarded = %v, want -1", awarded)
		}
		var unansweredRows int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM attempt_answers
			 WHERE attempt_id = $1 AND question_id = $2 AND points_awarded IS NOT NULL`,
			skipperAttempt, overrideQ).Scan(&unansweredRows); err != nil {
			t.Fatalf("count unanswered rows: %v", err)
		}
		if unansweredRows != 0 {
			t.Fatalf("unanswered question has %d scored rows, want 0 (never penalized)", unansweredRows)
		}
	})
}
