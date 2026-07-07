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

// TestKickFlowE2E pins the Milestone 6 kick brick (docs/06 section 4): the
// teacher-or-admin POST /attempts/:id/kick removes a student from a live
// attempt. It proves the whole invariant chain the docs demand:
//
//   - Authorization: a student is 403 (requireStaff), a non-owning teacher and
//     an unknown attempt are both 404 (existence never leaks), the owner and
//     any admin succeed. An empty reason is 422.
//   - The flip: status='kicked', submit_kind='kicked', kicked_by, kick_reason,
//     submitted_at - all in one transaction with the attempt.kicked event row
//     and the audit_log row, and the Redis relay fires exactly once after commit.
//   - Enforcement is the status flip, not the socket: the kicked student's next
//     autosave and submit both answer 409 ATTEMPT_KICKED.
//   - Idempotence: a repeat kick, and a kick that lost the race to a submit,
//     both no-op - no second event, publish, or audit row.
//   - Kicked work is graded, not discarded: GradeSubmitted advances the attempt
//     to 'graded' (so 'graded' stays the one "grading landed" signal every
//     results read gates on) while submit_kind = 'kicked' keeps the kick, so the
//     live roster shows the student 'kicked' both before and after grading.
//
// It runs in its own database (macquiz_kicktest) - see itest.FreshDatabase.
func TestKickFlowE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_kicktest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	pub := &capturePublisher{}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	router := httpserver.New(httpserver.BuildInfo{Version: "test"}, httpserver.Deps{
		DB:      sqlDB,
		Auth:    authusers.NewHandler(authSvc, false),
		Quiz:    quiz.NewHandler(quiz.NewService(sqlDB, log), authSvc),
		Attempt: attempt.NewHandler(attempt.NewService(sqlDB, log, pub), authSvc),
	})
	server := httptest.NewServer(router)
	defer server.Close()

	if err := authSvc.EnsureBootstrapAdmin(ctx, "admin@school.test", "admin-password-1", "Root Admin"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	provision(t, ctx, sqlDB, "teacher", "owner@school.test")
	provision(t, ctx, sqlDB, "teacher", "other@school.test")
	provision(t, ctx, sqlDB, "student", "taker@school.test")
	provision(t, ctx, sqlDB, "student", "racer@school.test")
	provision(t, ctx, sqlDB, "student", "adminvictim@school.test")

	admin := login(t, server, "admin@school.test", "admin-password-1")
	teacher := login(t, server, "owner@school.test", "account-password")
	other := login(t, server, "other@school.test", "account-password")
	taker := login(t, server, "taker@school.test", "account-password")
	racer := login(t, server, "racer@school.test", "account-password")
	victim := login(t, server, "adminvictim@school.test", "account-password")
	takerID := userID(t, ctx, sqlDB, "taker@school.test")
	racerID := userID(t, ctx, sqlDB, "racer@school.test")
	victimID := userID(t, ctx, sqlDB, "adminvictim@school.test")
	teacherID := userID(t, ctx, sqlDB, "owner@school.test")

	// A one-question quiz, three students assigned, a 2-minute per-attempt budget.
	status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
		map[string]string{"title": "Kick Under Test"}, teacher)
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
		map[string]any{"student_ids": []string{takerID, racerID, victimID}}, teacher); status != 200 {
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

	// The taker starts and answers one question, so there is autosaved work to grade.
	takerAttempt := start(t, server, quizID, taker)
	save(t, server, takerAttempt, q1, "a", taker)

	t.Run("kick is refused without staff role, ownership, a target, or a reason", func(t *testing.T) {
		// A student may not moderate at all (requireStaff -> 403).
		if s, _, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+takerAttempt+"/kick",
			map[string]any{"reason": "eyes on your own screen"}, taker); s != 403 {
			t.Fatalf("student kick = %d, want 403", s)
		}
		// A non-owning teacher may not learn the attempt exists (404, not 403).
		if s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+takerAttempt+"/kick",
			map[string]any{"reason": "not my quiz"}, other); s != 404 {
			t.Fatalf("non-owner teacher kick = %d %v, want 404", s, b)
		}
		// An unknown attempt is a leak-free 404 for the owner too.
		if s, _, _ := itest.Call(t, server, "POST",
			"/api/v1/attempts/00000000-0000-0000-0000-000000000000/kick",
			map[string]any{"reason": "ghost"}, teacher); s != 404 {
			t.Fatalf("unknown attempt kick = %d, want 404", s)
		}
		// The reason is required.
		if s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+takerAttempt+"/kick",
			map[string]any{"reason": "   "}, teacher); s != 422 {
			t.Fatalf("blank reason kick = %d %v, want 422", s, b)
		}
		// None of the refusals touched the attempt.
		st, _, _, _, _ := kickCols(t, ctx, sqlDB, takerAttempt)
		if st != "in_progress" {
			t.Fatalf("attempt status after refused kicks = %q, want in_progress", st)
		}
	})

	t.Run("the owner kicks: one flip, one event, one publish, one audit row", func(t *testing.T) {
		s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+takerAttempt+"/kick",
			map[string]any{"reason": "looked at phone"}, teacher)
		if s != 200 {
			t.Fatalf("owner kick = %d %v, want 200", s, b)
		}
		at := b["attempt"].(map[string]any)
		if at["status"] != "kicked" {
			t.Fatalf("kicked attempt status = %v, want kicked", at["status"])
		}

		st, kind, kickedBy, reason, submittedAt := kickCols(t, ctx, sqlDB, takerAttempt)
		if st != "kicked" || kind != "kicked" || kickedBy != teacherID || reason != "looked at phone" || !submittedAt {
			t.Fatalf("kick cols = status %q kind %q by %q reason %q submitted %v; want kicked/kicked/%s/looked at phone/true",
				st, kind, kickedBy, reason, submittedAt, teacherID)
		}

		k := filter(events(t, ctx, sqlDB, takerAttempt), "attempt.kicked")
		if len(k) != 1 || k[0].payload["kicked_by"] != teacherID || k[0].payload["reason"] != "looked at phone" {
			t.Fatalf("kicked events = %v, want one with kicked_by/reason", k)
		}

		pk := filterCaptured(pub.forAttempt(takerAttempt), "attempt.kicked")
		if len(pk) != 1 || pk[0].quizID != quizID || pk[0].payload["reason"] != "looked at phone" {
			t.Fatalf("published kicked = %v, want one on quiz %s", pk, quizID)
		}

		if n := auditRows(t, ctx, sqlDB, "attempt.kicked", takerAttempt); n != 1 {
			t.Fatalf("audit rows for kick = %d, want 1", n)
		}
	})

	t.Run("enforcement is the status flip: the kicked student is locked out", func(t *testing.T) {
		if s, b, _ := itest.Call(t, server, "PUT",
			"/api/v1/attempts/"+takerAttempt+"/answers/"+q1,
			map[string]any{"response": "b"}, taker); s != 409 || b["code"] != "ATTEMPT_KICKED" {
			t.Fatalf("kicked autosave = %d %v, want 409 ATTEMPT_KICKED", s, b)
		}
		if s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+takerAttempt+"/submit",
			nil, taker); s != 409 || b["code"] != "ATTEMPT_KICKED" {
			t.Fatalf("kicked submit = %d %v, want 409 ATTEMPT_KICKED", s, b)
		}
		// A kick burns the attempt slot: with max_attempts = 1 (the default),
		// the kicked student cannot start a fresh attempt (re-admission, a
		// later brick, is what grants a new slot - docs/06 section 4).
		if s, b, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, taker); s != 409 || b["code"] != "ATTEMPT_LIMIT_REACHED" {
			t.Fatalf("kicked student restart = %d %v, want 409 ATTEMPT_LIMIT_REACHED", s, b)
		}
	})

	t.Run("a repeat kick is idempotent: no second event, publish, or audit row", func(t *testing.T) {
		if s, _, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+takerAttempt+"/kick",
			map[string]any{"reason": "again"}, teacher); s != 200 {
			t.Fatalf("repeat kick = %d, want 200", s)
		}
		if k := filter(events(t, ctx, sqlDB, takerAttempt), "attempt.kicked"); len(k) != 1 {
			t.Fatalf("repeat kick added an event: %v", k)
		}
		if pk := filterCaptured(pub.forAttempt(takerAttempt), "attempt.kicked"); len(pk) != 1 {
			t.Fatalf("repeat kick published again: %v", pk)
		}
		if n := auditRows(t, ctx, sqlDB, "attempt.kicked", takerAttempt); n != 1 {
			t.Fatalf("repeat kick added an audit row: %d", n)
		}
		// The reason field was not overwritten by the no-op re-kick.
		if _, _, _, reason, _ := kickCols(t, ctx, sqlDB, takerAttempt); reason != "looked at phone" {
			t.Fatalf("re-kick overwrote reason = %q, want looked at phone", reason)
		}
	})

	t.Run("kicked work is graded, advancing to graded while submit_kind keeps the kick", func(t *testing.T) {
		graded, err := attempt.GradeSubmitted(ctx, sqlDB)
		if err != nil || graded != 1 {
			t.Fatalf("grade = %d (err %v), want 1", graded, err)
		}
		// Grading advances a kicked attempt to 'graded' just like a submitted
		// one - so 'graded' stays the single "grading landed" signal every
		// results read gates on - while submit_kind = 'kicked' preserves the
		// kick for the roster and the results flag.
		var score float64
		var st, kind string
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT score, status, submit_kind FROM attempts WHERE id = $1`, takerAttempt).Scan(&score, &st, &kind); err != nil {
			t.Fatalf("read graded attempt: %v", err)
		}
		if score != 3 || st != "graded" || kind != "kicked" {
			t.Fatalf("graded kick = score %v status %q submit_kind %q, want 3/graded/kicked", score, st, kind)
		}
		if g := filter(events(t, ctx, sqlDB, takerAttempt), "attempt.graded"); len(g) != 1 || g[0].payload["score"] != float64(3) {
			t.Fatalf("graded events = %v, want one with score 3", g)
		}
		// A re-grade flips nothing (score no longer null) and emits nothing more.
		if _, err := attempt.GradeSubmitted(ctx, sqlDB); err != nil {
			t.Fatalf("re-grade: %v", err)
		}
		if g := filter(events(t, ctx, sqlDB, takerAttempt), "attempt.graded"); len(g) != 1 {
			t.Fatalf("re-grade added a graded event: %v", g)
		}
	})

	t.Run("the live roster shows the kicked student, before and after grading", func(t *testing.T) {
		row := rosterRow(t, server, quizID, teacher, takerID)
		if row["state"] != "kicked" {
			t.Fatalf("roster state for kicked+graded student = %v, want kicked", row["state"])
		}
		if row["score"] != float64(3) {
			t.Fatalf("roster score for kicked+graded student = %v, want 3", row["score"])
		}
	})

	t.Run("an admin can kick any quiz's live attempt", func(t *testing.T) {
		victimAttempt := start(t, server, quizID, victim)
		if s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+victimAttempt+"/kick",
			map[string]any{"reason": "admin override"}, admin); s != 200 {
			t.Fatalf("admin kick = %d %v, want 200", s, b)
		}
		if st, _, _, _, _ := kickCols(t, ctx, sqlDB, victimAttempt); st != "kicked" {
			t.Fatalf("admin-kicked attempt status = %q, want kicked", st)
		}
	})

	t.Run("a kick that loses the race to a submit is a clean no-op", func(t *testing.T) {
		racerAttempt := start(t, server, quizID, racer)
		if s, _, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+racerAttempt+"/submit", nil, racer); s != 200 {
			t.Fatalf("racer submit = %d, want 200", s)
		}
		// The submit committed first; the kick must leave it standing.
		if s, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+racerAttempt+"/kick",
			map[string]any{"reason": "too late"}, teacher); s != 200 {
			t.Fatalf("kick after submit = %d %v, want 200", s, b)
		}
		if st, _, _, _, _ := kickCols(t, ctx, sqlDB, racerAttempt); st != "submitted" {
			t.Fatalf("attempt kicked after submit = %q, want submitted (submit won the race)", st)
		}
		if k := filter(events(t, ctx, sqlDB, racerAttempt), "attempt.kicked"); len(k) != 0 {
			t.Fatalf("no-op kick emitted an event: %v", k)
		}
		if n := auditRows(t, ctx, sqlDB, "attempt.kicked", racerAttempt); n != 0 {
			t.Fatalf("no-op kick wrote an audit row: %d", n)
		}
	})
}

// start begins an attempt and returns its id, failing on any non-201.
func start(t *testing.T, server *httptest.Server, quizID string, cookies map[string]string) string {
	t.Helper()
	status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, cookies)
	if status != 201 {
		t.Fatalf("start = %d %v", status, body)
	}
	return body["attempt"].(map[string]any)["id"].(string)
}

// kickCols reads the kick-relevant attempt columns for one attempt. A null
// submit_kind, kicked_by, or kick_reason reads back as the empty string.
func kickCols(t *testing.T, ctx context.Context, sqlDB *sql.DB, attemptID string) (status, submitKind, kickedBy, kickReason string, submittedAtSet bool) {
	t.Helper()
	var kind, by, reason sql.NullString
	var submittedAt sql.NullTime
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT status, submit_kind, kicked_by, kick_reason, submitted_at
		 FROM attempts WHERE id = $1`, attemptID).Scan(&status, &kind, &by, &reason, &submittedAt); err != nil {
		t.Fatalf("read kick cols: %v", err)
	}
	return status, kind.String, by.String, reason.String, submittedAt.Valid
}

// auditRows counts audit_log rows for one action against one attempt resource.
func auditRows(t *testing.T, ctx context.Context, sqlDB *sql.DB, action, attemptID string) int {
	t.Helper()
	var n int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM audit_log
		 WHERE action = $1 AND resource_type = 'attempt' AND resource_id = $2`,
		action, attemptID).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// rosterRow fetches GET /quizzes/:id/live and returns the roster cell for one
// student, failing if it is absent.
func rosterRow(t *testing.T, server *httptest.Server, quizID string, cookies map[string]string, studentID string) map[string]any {
	t.Helper()
	status, body, _ := itest.Call(t, server, "GET", "/api/v1/quizzes/"+quizID+"/live", nil, cookies)
	if status != 200 {
		t.Fatalf("live roster = %d %v, want 200", status, body)
	}
	for _, r := range body["roster"].([]any) {
		row := r.(map[string]any)
		if row["student_id"] == studentID {
			return row
		}
	}
	t.Fatalf("student %s absent from roster %v", studentID, body["roster"])
	return nil
}
