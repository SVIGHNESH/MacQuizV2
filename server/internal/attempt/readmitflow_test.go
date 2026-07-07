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

// TestReadmitFlowE2E pins the Milestone 6 re-admission brick (docs/06 section
// 4:81): the teacher-or-admin POST /attempts/:id/readmit grants a kicked
// student one fresh attempt slot. It proves the invariant chain the docs demand:
//
//   - Authorization mirrors kick exactly: a student is 403 (requireStaff), a
//     non-owning teacher and an unknown attempt are both 404 (existence never
//     leaks), the owner and any admin succeed. An empty reason is 422.
//   - Re-admission is only for a kicked student: readmitting an in_progress (or
//     otherwise not-kicked) attempt answers 409 ATTEMPT_NOT_KICKED.
//   - The grant is one fresh slot, no more: with max_attempts = 1 a kicked
//     student cannot start; after one readmit Start succeeds with a new attempt;
//     a second Start is blocked again (exactly one extra slot was granted).
//   - The kicked attempt stays terminal and immutable: status='kicked',
//     submit_kind='kicked' are untouched; only readmitted_at is marked.
//   - Idempotent per attempt: a repeat readmit answers 200 without granting a
//     second slot or writing a second audit row.
//   - Audited: exactly one attempt.readmitted audit_log row per real grant.
//
// It runs in its own database (macquiz_readmittest) - see itest.FreshDatabase.
func TestReadmitFlowE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_readmittest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	router := httpserver.New(httpserver.BuildInfo{Version: "test"}, httpserver.Deps{
		DB:      sqlDB,
		Auth:    authusers.NewHandler(authSvc, false),
		Quiz:    quiz.NewHandler(quiz.NewService(sqlDB, log), authSvc),
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
	provision(t, ctx, sqlDB, "student", "adminreadmit@school.test")

	admin := login(t, server, "admin@school.test", "admin-password-1")
	teacher := login(t, server, "owner@school.test", "account-password")
	other := login(t, server, "other@school.test", "account-password")
	taker := login(t, server, "taker@school.test", "account-password")
	openStudent := login(t, server, "openstudent@school.test", "account-password")
	adminVictim := login(t, server, "adminreadmit@school.test", "account-password")
	takerID := userID(t, ctx, sqlDB, "taker@school.test")
	openID := userID(t, ctx, sqlDB, "openstudent@school.test")
	adminVictimID := userID(t, ctx, sqlDB, "adminreadmit@school.test")

	// A one-question quiz, three students assigned, max_attempts = 1 (the default).
	status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
		map[string]string{"title": "Readmit Under Test"}, teacher)
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

	// The taker starts, answers, and is kicked - the one live attempt slot is burnt.
	takerAttempt := start(t, server, quizID, taker)
	save(t, server, takerAttempt, q1, "a", taker)
	if s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+takerAttempt+"/kick",
		map[string]any{"reason": "looked at phone"}, teacher); s != 200 {
		t.Fatalf("kick = %d %v, want 200", s, b)
	}
	// With max_attempts = 1 and the kicked row still counting, the student is locked out.
	if s, b, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, taker); s != 409 || b["code"] != "ATTEMPT_LIMIT_REACHED" {
		t.Fatalf("pre-readmit restart = %d %v, want 409 ATTEMPT_LIMIT_REACHED", s, b)
	}

	t.Run("readmit is refused without staff role, ownership, a target, or a reason", func(t *testing.T) {
		// A student may not moderate at all (requireStaff -> 403).
		if s, _, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+takerAttempt+"/readmit",
			map[string]any{"reason": "second chance"}, taker); s != 403 {
			t.Fatalf("student readmit = %d, want 403", s)
		}
		// A non-owning teacher may not learn the attempt exists (404, not 403).
		if s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+takerAttempt+"/readmit",
			map[string]any{"reason": "not my quiz"}, other); s != 404 {
			t.Fatalf("non-owner teacher readmit = %d %v, want 404", s, b)
		}
		// An unknown attempt is a leak-free 404 for the owner too.
		if s, _, _ := itest.Call(t, server, "POST",
			"/api/v1/attempts/00000000-0000-0000-0000-000000000000/readmit",
			map[string]any{"reason": "ghost"}, teacher); s != 404 {
			t.Fatalf("unknown attempt readmit = %d, want 404", s)
		}
		// The reason is required.
		if s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+takerAttempt+"/readmit",
			map[string]any{"reason": "   "}, teacher); s != 422 {
			t.Fatalf("blank reason readmit = %d %v, want 422", s, b)
		}
		// None of the refusals granted a slot.
		if n := auditRows(t, ctx, sqlDB, "attempt.readmitted", takerAttempt); n != 0 {
			t.Fatalf("refused readmits wrote an audit row: %d", n)
		}
	})

	t.Run("re-admission is only for a kicked student", func(t *testing.T) {
		// A live (never-kicked) attempt has nothing to readmit from.
		openAttempt := start(t, server, quizID, openStudent)
		if s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+openAttempt+"/readmit",
			map[string]any{"reason": "no reason to"}, teacher); s != 409 || b["code"] != "ATTEMPT_NOT_KICKED" {
			t.Fatalf("readmit of in_progress attempt = %d %v, want 409 ATTEMPT_NOT_KICKED", s, b)
		}
		if n := auditRows(t, ctx, sqlDB, "attempt.readmitted", openAttempt); n != 0 {
			t.Fatalf("not-kicked readmit wrote an audit row: %d", n)
		}
	})

	t.Run("the owner readmits: the kicked row is untouched, one audit row, one fresh slot", func(t *testing.T) {
		s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+takerAttempt+"/readmit",
			map[string]any{"reason": "second chance"}, teacher)
		if s != 200 {
			t.Fatalf("owner readmit = %d %v, want 200", s, b)
		}
		// The kicked attempt stays terminal and immutable; only readmitted_at is set.
		st, kind, _, reason, _ := kickCols(t, ctx, sqlDB, takerAttempt)
		if st != "kicked" || kind != "kicked" || reason != "looked at phone" {
			t.Fatalf("readmit mutated the kicked row: status %q kind %q reason %q", st, kind, reason)
		}
		var readmittedAt sql.NullTime
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT readmitted_at FROM attempts WHERE id = $1`, takerAttempt).Scan(&readmittedAt); err != nil {
			t.Fatalf("read readmitted_at: %v", err)
		}
		if !readmittedAt.Valid {
			t.Fatal("readmitted_at was not set by the grant")
		}
		if n := auditRows(t, ctx, sqlDB, "attempt.readmitted", takerAttempt); n != 1 {
			t.Fatalf("audit rows for readmit = %d, want 1", n)
		}
	})

	t.Run("a repeat readmit is idempotent: 200, no second slot, no second audit row", func(t *testing.T) {
		if s, _, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+takerAttempt+"/readmit",
			map[string]any{"reason": "again"}, teacher); s != 200 {
			t.Fatalf("repeat readmit = %d, want 200", s)
		}
		if n := auditRows(t, ctx, sqlDB, "attempt.readmitted", takerAttempt); n != 1 {
			t.Fatalf("repeat readmit added an audit row: %d", n)
		}
	})

	t.Run("the granted slot lets the student start exactly one fresh attempt", func(t *testing.T) {
		// The extra slot is now available: Start succeeds with a new attempt_no.
		s, b, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, taker)
		if s != 201 {
			t.Fatalf("post-readmit start = %d %v, want 201", s, b)
		}
		fresh := b["attempt"].(map[string]any)
		if fresh["status"] != "in_progress" {
			t.Fatalf("fresh attempt status = %v, want in_progress", fresh["status"])
		}
		if fresh["attempt_no"] != float64(2) {
			t.Fatalf("fresh attempt_no = %v, want 2", fresh["attempt_no"])
		}
		if fresh["id"] == takerAttempt {
			t.Fatal("readmit reused the kicked attempt row instead of starting a new one")
		}
		// Exactly one slot: a second start is blocked again. (The in_progress
		// fresh attempt would resume rather than burn a slot, so submit it first.)
		if s, _, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+fresh["id"].(string)+"/submit", nil, taker); s != 200 {
			t.Fatalf("submit fresh attempt = %d, want 200", s)
		}
		if s, b, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, taker); s != 409 || b["code"] != "ATTEMPT_LIMIT_REACHED" {
			t.Fatalf("second post-readmit start = %d %v, want 409 ATTEMPT_LIMIT_REACHED (one slot only)", s, b)
		}
	})

	t.Run("an admin can readmit any quiz's kicked attempt", func(t *testing.T) {
		victimAttempt := start(t, server, quizID, adminVictim)
		if s, _, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+victimAttempt+"/kick",
			map[string]any{"reason": "removed"}, teacher); s != 200 {
			t.Fatalf("kick for admin readmit = %d, want 200", s)
		}
		if s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+victimAttempt+"/readmit",
			map[string]any{"reason": "admin override"}, admin); s != 200 {
			t.Fatalf("admin readmit = %d %v, want 200", s, b)
		}
		if n := auditRows(t, ctx, sqlDB, "attempt.readmitted", victimAttempt); n != 1 {
			t.Fatalf("admin readmit audit rows = %d, want 1", n)
		}
		// The readmitted student can now start fresh.
		if s, b, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, adminVictim); s != 201 {
			t.Fatalf("admin-readmitted student start = %d %v, want 201", s, b)
		}
	})
}
