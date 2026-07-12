package quiz_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/db"
	"macquiz/server/internal/httpserver"
	"macquiz/server/internal/itest"
	"macquiz/server/internal/quiz"
)

// TestCancelE2E pins the Scheduled-only "cancel" affordance (docs/06 section 1:
// "while Scheduled: reschedule and cancel are allowed"). Cancel returns a
// not-yet-open quiz to Draft: the window is cleared, the editor unlocks, and
// the quiz drops off student dashboards - while the append-only version history
// and the audience survive, so a republish is a plain version n+1 reschedule.
// Owner-teacher only; a quiz that has effectively opened (starts_at in the past,
// even before the sweep flips the row), or that is live/closed/archived, answers
// 409 QUIZ_NOT_CANCELLABLE - that one is force-closed, not cancelled.
//
// It runs in its own database (macquiz_canceltest) - see itest.FreshDatabase.
func TestCancelE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_canceltest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	quizSvc := quiz.NewService(sqlDB, log, quiz.LocalImportStorage{Dir: t.TempDir()})
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
	provision(t, ctx, sqlDB, "teacher", "other@school.test")
	provision(t, ctx, sqlDB, "student", "pupil@school.test")
	admin := login(t, server, "admin@school.test", "admin-password-1")
	owner := login(t, server, "owner@school.test", "account-password")
	other := login(t, server, "other@school.test", "account-password")
	student := login(t, server, "pupil@school.test", "account-password")
	pupilID := userID(t, ctx, sqlDB, "pupil@school.test")

	// publishFuture schedules a quiz an hour out - safely un-opened, so it is
	// cancellable until a subtest deliberately ages its window.
	publishFuture := func(t *testing.T, quizID string) {
		t.Helper()
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish",
			map[string]any{
				"starts_at":      time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				"ends_at":        time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
				"duration_sec":   1800,
				"guardrails":     map[string]any{"fullscreen": "warn", "focus_tracking": "count", "block_clipboard": true, "max_violations": 2, "violation_action": "flag"},
				"release_policy": "manual",
			}, owner)
		if status != 200 {
			t.Fatalf("publish = %d %v", status, body)
		}
	}

	quizID := authorMinimalQuiz(t, server, owner, "Midterm")
	assign(t, server, owner, quizID, pupilID)
	publishFuture(t, quizID)

	t.Run("only the owning teacher may cancel", func(t *testing.T) {
		// None of these reach the UPDATE, so the scheduled quiz survives for
		// the happy path below.
		cases := []struct {
			name    string
			cookies map[string]string
			target  string
			want    int
		}{
			{"student is forbidden", student, quizID, 403},
			{"admin cannot author", admin, quizID, 403},
			{"non-owning teacher is not found", other, quizID, 404},
			{"unknown quiz is not found", owner, "00000000-0000-0000-0000-000000000000", 404},
			{"non-uuid is not found", owner, "not-a-uuid", 404},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+tc.target+"/cancel", nil, tc.cookies)
				if status != tc.want {
					t.Fatalf("cancel = %d %v, want %d", status, body, tc.want)
				}
			})
		}
		if got := storedStatus(t, ctx, sqlDB, quizID); got != "scheduled" {
			t.Fatalf("stored status after refused cancels = %q, want scheduled", got)
		}
	})

	t.Run("a quiz that has effectively opened cannot be cancelled", func(t *testing.T) {
		// Stored 'scheduled' but with starts_at backdated and NOT swept, so
		// only the server clock knows it is open. Publish refuses a past
		// starts_at, so the window is aged in SQL - the same shape the sweep
		// races against. The UPDATE's starts_at > now() gate must refuse it.
		openID := authorMinimalQuiz(t, server, owner, "Already Open")
		assign(t, server, owner, openID, pupilID)
		publishFuture(t, openID)
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET starts_at = now() - interval '1 minute',
			                    ends_at = now() + interval '1 hour' WHERE id = $1`, openID); err != nil {
			t.Fatalf("age window: %v", err)
		}
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+openID+"/cancel", nil, owner)
		if status != 409 || body["code"] != "QUIZ_NOT_CANCELLABLE" {
			t.Fatalf("cancel effectively-open = %d %v, want 409 QUIZ_NOT_CANCELLABLE", status, body)
		}
		if got := storedStatus(t, ctx, sqlDB, openID); got != "scheduled" {
			t.Fatalf("stored status = %q, want scheduled (untouched)", got)
		}

		// Once swept to 'live' it is still refused - and the message is the
		// same, because both mean "the students can already see it".
		if _, _, err := quiz.SweepDueQuizzes(ctx, sqlDB); err != nil {
			t.Fatalf("sweep to live: %v", err)
		}
		if got := storedStatus(t, ctx, sqlDB, openID); got != "live" {
			t.Fatalf("stored status after sweep = %q, want live", got)
		}
		status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+openID+"/cancel", nil, owner)
		if status != 409 || body["code"] != "QUIZ_NOT_CANCELLABLE" {
			t.Fatalf("cancel live = %d %v, want 409 QUIZ_NOT_CANCELLABLE", status, body)
		}

		// And a closed quiz - the terminal end of that same road.
		if status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+openID+"/close", nil, owner); status != 200 {
			t.Fatalf("force close = %d %v", status, body)
		}
		status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+openID+"/cancel", nil, owner)
		if status != 409 || body["code"] != "QUIZ_NOT_CANCELLABLE" {
			t.Fatalf("cancel closed = %d %v, want 409 QUIZ_NOT_CANCELLABLE", status, body)
		}
	})

	t.Run("the student sees the scheduled quiz before it is cancelled", func(t *testing.T) {
		if !assignedListsQuiz(t, server, student, quizID) {
			t.Fatalf("assigned quizzes does not list the scheduled quiz")
		}
	})

	t.Run("the owner cancels the scheduled quiz", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/cancel", nil, owner)
		if status != 200 {
			t.Fatalf("cancel = %d %v, want 200", status, body)
		}
		respQuiz := body["quiz"].(map[string]any)
		if got := respQuiz["status"]; got != "draft" {
			t.Fatalf("response quiz.status = %v, want draft", got)
		}
		if got := respQuiz["starts_at"]; got != nil {
			t.Fatalf("response quiz.starts_at = %v, want null", got)
		}
		if got := respQuiz["ends_at"]; got != nil {
			t.Fatalf("response quiz.ends_at = %v, want null", got)
		}
		if got := respQuiz["published_at"]; got != nil {
			t.Fatalf("response quiz.published_at = %v, want null", got)
		}
		// Parity with publish/force-close: the question count is populated, not
		// left 0 by quizColumns (Iteration 1 trap).
		if got := respQuiz["question_count"]; got != float64(1) {
			t.Fatalf("response quiz.question_count = %v, want 1", got)
		}
		// The settings a republish reuses survive the round trip: version
		// (the history is append-only), duration, guardrails, release policy.
		if got := respQuiz["version"]; got != float64(1) {
			t.Fatalf("response quiz.version = %v, want 1", got)
		}
		if got := respQuiz["duration_sec"]; got != float64(1800) {
			t.Fatalf("response quiz.duration_sec = %v, want 1800", got)
		}
		if got := respQuiz["release_policy"]; got != "manual" {
			t.Fatalf("response quiz.release_policy = %v, want manual", got)
		}
		guardrails, ok := respQuiz["guardrails"].(map[string]any)
		if !ok || guardrails["fullscreen"] != "warn" || guardrails["max_violations"] != float64(2) {
			t.Fatalf("response quiz.guardrails = %v, want the published ladder", respQuiz["guardrails"])
		}

		if got := storedStatus(t, ctx, sqlDB, quizID); got != "draft" {
			t.Fatalf("stored status = %q, want draft", got)
		}
		// The version snapshot is append-only: publish wrote it, cancel keeps
		// it, so the frozen question set of the called-off run is still on file.
		var versions int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM quiz_versions WHERE quiz_id = $1`, quizID).Scan(&versions); err != nil {
			t.Fatalf("count versions: %v", err)
		}
		if versions != 1 {
			t.Fatalf("quiz_versions rows = %d, want 1 (history is append-only)", versions)
		}
		// The audience is kept, so a republish needs no re-assignment.
		var assignments int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM quiz_assignments WHERE quiz_id = $1`, quizID).Scan(&assignments); err != nil {
			t.Fatalf("count assignments: %v", err)
		}
		if assignments != 1 {
			t.Fatalf("quiz_assignments rows = %d, want 1", assignments)
		}
		// One audit row carrying the before/after under the changes convention
		// (docs/08 section 7): the status flip and the window it cleared.
		var auditCount int
		var statusDiffed, windowCleared bool
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*),
			        coalesce(bool_or(detail->'changes'->'status'->>'from' = 'scheduled'
			                     AND detail->'changes'->'status'->>'to' = 'draft'), false),
			        coalesce(bool_or(detail->'changes'->'starts_at'->>'from' IS NOT NULL
			                     AND detail->'changes'->'starts_at'->'to' = 'null'::jsonb), false)
			 FROM audit_log
			 WHERE action = 'quizzes.cancelled' AND resource_id = $1`, quizID).Scan(&auditCount, &statusDiffed, &windowCleared); err != nil {
			t.Fatalf("read audit rows: %v", err)
		}
		if auditCount != 1 {
			t.Fatalf("cancelled audit rows = %d, want 1", auditCount)
		}
		if !statusDiffed {
			t.Fatalf("audit detail does not record status scheduled -> draft")
		}
		if !windowCleared {
			t.Fatalf("audit detail does not record starts_at cleared to null")
		}
	})

	t.Run("the cancelled quiz drops off the student's dashboard", func(t *testing.T) {
		if assignedListsQuiz(t, server, student, quizID) {
			t.Fatalf("assigned quizzes still lists the cancelled quiz")
		}
	})

	t.Run("a second cancel is an idempotent no-op", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/cancel", nil, owner)
		if status != 200 {
			t.Fatalf("second cancel = %d %v, want 200", status, body)
		}
		if got := body["quiz"].(map[string]any)["status"]; got != "draft" {
			t.Fatalf("response quiz.status = %v, want draft", got)
		}
		var auditCount int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM audit_log WHERE action = 'quizzes.cancelled' AND resource_id = $1`,
			quizID).Scan(&auditCount); err != nil {
			t.Fatalf("count audit rows: %v", err)
		}
		if auditCount != 1 {
			t.Fatalf("cancelled audit rows after second cancel = %d, want 1 (no second row)", auditCount)
		}
	})

	t.Run("the scheduler leaves the cancelled draft alone", func(t *testing.T) {
		// The open_quiz/close_quiz jobs publish enqueued are still queued at the
		// original window; they all run SweepDueQuizzes, whose predicate needs
		// status IN (scheduled, live). A cancelled draft is inert against them.
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET starts_at = now() - interval '1 hour' WHERE id = $1`, quizID); err != nil {
			t.Fatalf("backdate starts_at: %v", err)
		}
		if _, _, err := quiz.SweepDueQuizzes(ctx, sqlDB); err != nil {
			t.Fatalf("sweep: %v", err)
		}
		if got := storedStatus(t, ctx, sqlDB, quizID); got != "draft" {
			t.Fatalf("stored status after sweep = %q, want draft (a cancelled quiz never opens)", got)
		}
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET starts_at = NULL WHERE id = $1`, quizID); err != nil {
			t.Fatalf("re-clear starts_at: %v", err)
		}
	})

	t.Run("the cancelled draft is editable and republishes at the next version", func(t *testing.T) {
		// Editable again: the draft-only guard that refuses a scheduled quiz's
		// edits no longer fires.
		if status, body, _ := itest.Call(t, server, "PATCH", "/api/v1/quizzes/"+quizID,
			map[string]any{"title": "Midterm (take two)"}, owner); status != 200 {
			t.Fatalf("edit cancelled draft = %d %v, want 200", status, body)
		}
		publishFuture(t, quizID)
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/quizzes/"+quizID, nil, owner)
		if status != 200 {
			t.Fatalf("get quiz = %d %v", status, body)
		}
		respQuiz := body["quiz"].(map[string]any)
		if got := respQuiz["status"]; got != "scheduled" {
			t.Fatalf("republished status = %v, want scheduled", got)
		}
		if got := respQuiz["version"]; got != float64(2) {
			t.Fatalf("republished version = %v, want 2", got)
		}
		if !assignedListsQuiz(t, server, student, quizID) {
			t.Fatalf("the republished quiz is not back on the student's dashboard")
		}
	})
}

// assignedListsQuiz reports whether the student's dashboard (GET
// /quizzes/assigned) carries the quiz.
func assignedListsQuiz(t *testing.T, server *httptest.Server, student map[string]string, quizID string) bool {
	t.Helper()
	status, body, _ := itest.Call(t, server, "GET", "/api/v1/quizzes/assigned", nil, student)
	if status != 200 {
		t.Fatalf("assigned quizzes = %d %v", status, body)
	}
	for _, raw := range body["quizzes"].([]any) {
		if raw.(map[string]any)["id"] == quizID {
			return true
		}
	}
	return false
}
