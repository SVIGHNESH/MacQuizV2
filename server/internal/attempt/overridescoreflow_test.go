package attempt_test

import (
	"context"
	"database/sql"
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

// TestOverrideScoreFlowE2E pins docs/06 line 80's still-missing brick: "results
// are flagged kicked wherever scores appear, and the teacher can override the
// score to zero per attempt". It proves the invariant chain the docs demand:
//
//   - Authorization mirrors kick/readmit exactly: a student is 403
//     (requireStaff), a non-owning teacher and an unknown attempt are both 404
//     (existence never leaks), the owner and any admin succeed. An empty
//     reason is 422.
//   - The target must be kicked (else 409 ATTEMPT_NOT_KICKED) and already
//     graded (else 409 ATTEMPT_NOT_GRADED - the async grade job would
//     otherwise race the override and clobber it).
//   - The override zeroes score and stamps score_overridden_at/_by in the same
//     transaction as one attempt.score_overridden audit row.
//   - Idempotent per attempt: a repeat override answers 200 without zeroing
//     again or writing a second audit row.
//   - The overridden flag surfaces wherever a score appears: the teacher's
//     GET /quizzes/:id/results table (score_overridden: true).
//
// It runs in its own database (macquiz_overridescoretest) - see
// itest.FreshDatabase.
func TestOverrideScoreFlowE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_overridescoretest")
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
	provision(t, ctx, sqlDB, "student", "taker@school.test")
	provision(t, ctx, sqlDB, "student", "openstudent@school.test")
	provision(t, ctx, sqlDB, "student", "adminvictim@school.test")

	admin := login(t, server, "admin@school.test", "admin-password-1")
	teacher := login(t, server, "owner@school.test", "account-password")
	other := login(t, server, "other@school.test", "account-password")
	taker := login(t, server, "taker@school.test", "account-password")
	openStudent := login(t, server, "openstudent@school.test", "account-password")
	adminVictim := login(t, server, "adminvictim@school.test", "account-password")

	// A one-question quiz, three students assigned, max_attempts = 1 (the default).
	status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
		map[string]string{"title": "Override Score Under Test"}, teacher)
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
	q1 := body["question"].(map[string]any)["id"].(string)
	takerID := userID(t, ctx, sqlDB, "taker@school.test")
	openID := userID(t, ctx, sqlDB, "openstudent@school.test")
	adminVictimID := userID(t, ctx, sqlDB, "adminvictim@school.test")
	if status, _, _ = itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
		map[string]any{"student_ids": []string{takerID, openID, adminVictimID}}, teacher); status != 200 {
		t.Fatalf("assign = %d", status)
	}
	if status, _, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", map[string]any{
		"starts_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"ends_at":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		"duration_sec": 120,
	}, teacher); status != 200 {
		t.Fatalf("publish = %d", status)
	}
	// Backdate starts_at so the quiz reads live lazily.
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE quizzes SET starts_at = now() - interval '1 minute' WHERE id = $1`, quizID); err != nil {
		t.Fatalf("backdate starts_at: %v", err)
	}

	// The taker starts, answers correctly (earning 3), and is kicked.
	takerAttempt := start(t, server, quizID, taker)
	save(t, server, takerAttempt, q1, "a", taker)
	if s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+takerAttempt+"/kick",
		map[string]any{"reason": "looked at phone"}, teacher); s != 200 {
		t.Fatalf("kick = %d %v, want 200", s, b)
	}

	t.Run("override is refused before grading lands", func(t *testing.T) {
		if s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+takerAttempt+"/override-score",
			map[string]any{"reason": "too early"}, teacher); s != 409 || b["code"] != "ATTEMPT_NOT_GRADED" {
			t.Fatalf("pre-grade override = %d %v, want 409 ATTEMPT_NOT_GRADED", s, b)
		}
	})

	if graded, err := attempt.GradeSubmitted(ctx, sqlDB); err != nil || graded != 1 {
		t.Fatalf("grade = %d (err %v), want 1", graded, err)
	}

	t.Run("override is refused without staff role, ownership, a target, or a reason", func(t *testing.T) {
		if s, _, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+takerAttempt+"/override-score",
			map[string]any{"reason": "second look"}, taker); s != 403 {
			t.Fatalf("student override = %d, want 403", s)
		}
		if s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+takerAttempt+"/override-score",
			map[string]any{"reason": "not my quiz"}, other); s != 404 {
			t.Fatalf("non-owner teacher override = %d %v, want 404", s, b)
		}
		if s, _, _ := itest.Call(t, server, "POST",
			"/api/v1/attempts/00000000-0000-0000-0000-000000000000/override-score",
			map[string]any{"reason": "ghost"}, teacher); s != 404 {
			t.Fatalf("unknown attempt override = %d, want 404", s)
		}
		if s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+takerAttempt+"/override-score",
			map[string]any{"reason": "   "}, teacher); s != 422 {
			t.Fatalf("blank reason override = %d %v, want 422", s, b)
		}
		if n := auditRows(t, ctx, sqlDB, "attempt.score_overridden", takerAttempt); n != 0 {
			t.Fatalf("refused overrides wrote an audit row: %d", n)
		}
	})

	t.Run("override is only for a kicked attempt", func(t *testing.T) {
		openAttempt := start(t, server, quizID, openStudent)
		if s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+openAttempt+"/override-score",
			map[string]any{"reason": "no reason to"}, teacher); s != 409 || b["code"] != "ATTEMPT_NOT_KICKED" {
			t.Fatalf("override of in_progress attempt = %d %v, want 409 ATTEMPT_NOT_KICKED", s, b)
		}
	})

	t.Run("the owner overrides: score zeroed, marker stamped, one audit row", func(t *testing.T) {
		s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+takerAttempt+"/override-score",
			map[string]any{"reason": "integrity violation confirmed"}, teacher)
		if s != 200 {
			t.Fatalf("owner override = %d %v, want 200", s, b)
		}
		var score float64
		var overriddenAt sql.NullTime
		var overriddenBy sql.NullString
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT score, score_overridden_at, score_overridden_by FROM attempts WHERE id = $1`,
			takerAttempt).Scan(&score, &overriddenAt, &overriddenBy); err != nil {
			t.Fatalf("read overridden attempt: %v", err)
		}
		if score != 0 {
			t.Fatalf("score after override = %v, want 0", score)
		}
		if !overriddenAt.Valid {
			t.Fatal("score_overridden_at was not set by the override")
		}
		if !overriddenBy.Valid {
			t.Fatal("score_overridden_by was not set by the override")
		}
		if n := auditRows(t, ctx, sqlDB, "attempt.score_overridden", takerAttempt); n != 1 {
			t.Fatalf("audit rows for override = %d, want 1", n)
		}
	})

	t.Run("a repeat override is idempotent: 200, no re-zero, no second audit row", func(t *testing.T) {
		if s, _, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+takerAttempt+"/override-score",
			map[string]any{"reason": "again"}, teacher); s != 200 {
			t.Fatalf("repeat override = %d, want 200", s)
		}
		if n := auditRows(t, ctx, sqlDB, "attempt.score_overridden", takerAttempt); n != 1 {
			t.Fatalf("repeat override added an audit row: %d", n)
		}
	})

	t.Run("the overridden flag surfaces in the teacher's results table", func(t *testing.T) {
		s, b, _ := itest.Call(t, server, "GET", "/api/v1/quizzes/"+quizID+"/results", nil, teacher)
		if s != 200 {
			t.Fatalf("results = %d %v, want 200", s, b)
		}
		rows := b["results"].([]any)
		var found bool
		for _, raw := range rows {
			row := raw.(map[string]any)
			if row["attempt_id"] == takerAttempt {
				found = true
				if row["score_overridden"] != true {
					t.Fatalf("results row score_overridden = %v, want true", row["score_overridden"])
				}
				if row["score"] != float64(0) {
					t.Fatalf("results row score = %v, want 0", row["score"])
				}
			}
		}
		if !found {
			t.Fatal("results table did not include the overridden attempt")
		}
	})

	t.Run("an admin can override any quiz's kicked+graded attempt", func(t *testing.T) {
		victimAttempt := start(t, server, quizID, adminVictim)
		if s, _, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+victimAttempt+"/kick",
			map[string]any{"reason": "removed"}, teacher); s != 200 {
			t.Fatalf("kick for admin override = %d, want 200", s)
		}
		if graded, err := attempt.GradeSubmitted(ctx, sqlDB); err != nil || graded != 1 {
			t.Fatalf("grade victim = %d (err %v), want 1", graded, err)
		}
		if s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+victimAttempt+"/override-score",
			map[string]any{"reason": "admin override"}, admin); s != 200 {
			t.Fatalf("admin override = %d %v, want 200", s, b)
		}
		if n := auditRows(t, ctx, sqlDB, "attempt.score_overridden", victimAttempt); n != 1 {
			t.Fatalf("admin override audit rows = %d, want 1", n)
		}
	})
}
