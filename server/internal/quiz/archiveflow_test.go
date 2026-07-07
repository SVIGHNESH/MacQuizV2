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

// TestArchiveE2E pins the Milestone 6 live-quiz teacher control "archive"
// (docs/06 section 1: "Closed -> Archived | Teacher archives | Read-only;
// analytics retained"): POST /quizzes/:id/archive retires a closed quiz to the
// terminal 'archived' state. Owner-teacher only (a student is 403, and an
// admin, who cannot author, is 403; both fail before the row is touched). The
// gate is the stored status, so only a closed quiz archives - a draft or a
// live quiz answers 409 QUIZ_NOT_CLOSED. A real archive flips the stored row to
// archived and writes exactly one audit row; re-archiving is an idempotent
// no-op. "Analytics retained" is proven two ways: the teacher's Results view
// still reads after archiving, and a manual-release quiz archived before its
// results were released can still be released without leaving the archived
// state.
//
// It runs in its own database (macquiz_archivetest) - see itest.FreshDatabase.
func TestArchiveE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_archivetest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	quizSvc := quiz.NewService(sqlDB, log)
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

	// closeQuiz publishes a minimal quiz (manual release policy so the
	// release-after-archive subtest has an unreleased quiz to work with),
	// rewinds its whole window into the past, and sweeps it to the stored
	// 'closed' state - the only state archive accepts.
	closeQuiz := func(title string) string {
		id := authorMinimalQuiz(t, server, owner, title)
		assign(t, server, owner, id, pupilID)
		if status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+id+"/publish",
			map[string]any{
				"starts_at":      time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				"ends_at":        time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
				"duration_sec":   600,
				"release_policy": "manual",
			}, owner); status != 200 {
			t.Fatalf("publish = %d %v", status, body)
		}
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET starts_at = now() - interval '2 hours',
			                    ends_at = now() - interval '1 hour' WHERE id = $1`, id); err != nil {
			t.Fatalf("rewind window: %v", err)
		}
		if _, _, err := quiz.SweepDueQuizzes(ctx, sqlDB); err != nil {
			t.Fatalf("sweep to closed: %v", err)
		}
		if got := storedStatus(t, ctx, sqlDB, id); got != "closed" {
			t.Fatalf("stored status after sweep = %q, want closed", got)
		}
		return id
	}

	quizID := closeQuiz("Past Session")

	t.Run("only the owning teacher may archive", func(t *testing.T) {
		// None of these reach the UPDATE, so the closed quiz is untouched and
		// available to the happy-path subtests below.
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
				status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+tc.target+"/archive", nil, tc.cookies)
				if status != tc.want {
					t.Fatalf("archive = %d %v, want %d", status, body, tc.want)
				}
			})
		}
		if got := storedStatus(t, ctx, sqlDB, quizID); got != "closed" {
			t.Fatalf("stored status after refused archives = %q, want closed", got)
		}
	})

	t.Run("a quiz that is not closed cannot be archived", func(t *testing.T) {
		// A draft was never opened.
		draftID := authorMinimalQuiz(t, server, owner, "Never Published")
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+draftID+"/archive", nil, owner)
		if status != 409 {
			t.Fatalf("archive draft = %d %v, want 409", status, body)
		}
		if code := body["code"]; code != "QUIZ_NOT_CLOSED" {
			t.Fatalf("archive draft code = %v, want QUIZ_NOT_CLOSED", code)
		}

		// A live quiz must be force-closed first; its stored status is 'live',
		// so archive refuses it (the stored-status gate, not effective).
		liveID := authorMinimalQuiz(t, server, owner, "Still Live")
		assign(t, server, owner, liveID, pupilID)
		if s, b, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+liveID+"/publish",
			map[string]any{
				"starts_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				"ends_at":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
				"duration_sec": 600,
			}, owner); s != 200 {
			t.Fatalf("publish live = %d %v", s, b)
		}
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET starts_at = now() - interval '1 minute',
			                    ends_at = now() + interval '1 hour' WHERE id = $1`, liveID); err != nil {
			t.Fatalf("rewind live window: %v", err)
		}
		if _, _, err := quiz.SweepDueQuizzes(ctx, sqlDB); err != nil {
			t.Fatalf("sweep to live: %v", err)
		}
		status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+liveID+"/archive", nil, owner)
		if status != 409 {
			t.Fatalf("archive live = %d %v, want 409", status, body)
		}
		if code := body["code"]; code != "QUIZ_NOT_CLOSED" {
			t.Fatalf("archive live code = %v, want QUIZ_NOT_CLOSED", code)
		}
	})

	t.Run("the owner archives the closed quiz", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/archive", nil, owner)
		if status != 200 {
			t.Fatalf("archive = %d %v, want 200", status, body)
		}
		respQuiz := body["quiz"].(map[string]any)
		if got := respQuiz["status"]; got != "archived" {
			t.Fatalf("response quiz.status = %v, want archived", got)
		}
		// Parity with publish's response: the question count is populated, not
		// left 0 by quizColumns (Iteration 1 trap).
		if got := respQuiz["question_count"]; got != float64(1) {
			t.Fatalf("response quiz.question_count = %v, want 1", got)
		}
		if got := storedStatus(t, ctx, sqlDB, quizID); got != "archived" {
			t.Fatalf("stored status = %q, want archived", got)
		}
		var auditCount int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM audit_log WHERE action = 'quizzes.archived' AND resource_id = $1`, quizID).Scan(&auditCount); err != nil {
			t.Fatalf("count audit rows: %v", err)
		}
		if auditCount != 1 {
			t.Fatalf("archived audit rows = %d, want 1", auditCount)
		}
	})

	t.Run("the teacher's results view still reads after archiving", func(t *testing.T) {
		// "Analytics retained": archiving must not lock the owner out of the
		// per-student results table.
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/quizzes/"+quizID+"/results", nil, owner)
		if status != 200 {
			t.Fatalf("results after archive = %d %v, want 200", status, body)
		}
		if _, ok := body["results"]; !ok {
			t.Fatalf("results payload missing after archive: %v", body)
		}
	})

	t.Run("a manual-release quiz can still be released after archiving", func(t *testing.T) {
		// The quiz was published manual and never released, so release-results
		// must still work (analytics retained) - and must not un-archive it.
		var before *time.Time
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT results_released_at FROM quizzes WHERE id = $1`, quizID).Scan(&before); err != nil {
			t.Fatalf("read pre-release: %v", err)
		}
		if before != nil {
			t.Fatalf("results already released before the subtest ran")
		}
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/release-results", nil, owner)
		if status != 200 {
			t.Fatalf("release after archive = %d %v, want 200", status, body)
		}
		respQuiz := body["quiz"].(map[string]any)
		if got := respQuiz["status"]; got != "archived" {
			t.Fatalf("release un-archived the quiz: status = %v, want archived", got)
		}
		if respQuiz["results_released_at"] == nil {
			t.Fatalf("results_released_at not set after release")
		}
		if got := storedStatus(t, ctx, sqlDB, quizID); got != "archived" {
			t.Fatalf("stored status after release = %q, want archived", got)
		}
	})

	t.Run("an auto-release quiz still auto-releases after archiving", func(t *testing.T) {
		// The auto path is the default policy and runs in the worker's
		// ReleaseDueResults sweep, which must see an archived quiz too: a teacher
		// can force-close an auto quiz and archive it before that sweep lands,
		// and its results must not strand ("analytics retained").
		autoID := authorMinimalQuiz(t, server, owner, "Auto Then Archived")
		assign(t, server, owner, autoID, pupilID)
		if s, b, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+autoID+"/publish",
			map[string]any{
				"starts_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				"ends_at":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
				"duration_sec": 600,
			}, owner); s != 200 { // release_policy defaults to auto
			t.Fatalf("publish auto = %d %v", s, b)
		}
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET starts_at = now() - interval '2 hours',
			                    ends_at = now() - interval '1 hour' WHERE id = $1`, autoID); err != nil {
			t.Fatalf("rewind auto window: %v", err)
		}
		if _, _, err := quiz.SweepDueQuizzes(ctx, sqlDB); err != nil {
			t.Fatalf("sweep auto to closed: %v", err)
		}
		// Archive it before the release sweep runs (no attempts, so nothing to
		// grade first; the quiz is graded-clean).
		if status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+autoID+"/archive", nil, owner); status != 200 {
			t.Fatalf("archive auto = %d %v, want 200", status, body)
		}
		if got := storedStatus(t, ctx, sqlDB, autoID); got != "archived" {
			t.Fatalf("auto quiz status = %q, want archived", got)
		}
		// The worker's release sweep must still stamp results_released_at.
		if _, err := quiz.ReleaseDueResults(ctx, sqlDB); err != nil {
			t.Fatalf("release due results: %v", err)
		}
		var releasedAt *time.Time
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT results_released_at FROM quizzes WHERE id = $1`, autoID).Scan(&releasedAt); err != nil {
			t.Fatalf("read released_at: %v", err)
		}
		if releasedAt == nil {
			t.Fatalf("archived auto quiz never auto-released - results stranded")
		}
		if got := storedStatus(t, ctx, sqlDB, autoID); got != "archived" {
			t.Fatalf("auto release un-archived the quiz: status = %q, want archived", got)
		}
	})

	t.Run("re-archiving an already-archived quiz is an idempotent no-op", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/archive", nil, owner)
		if status != 200 {
			t.Fatalf("re-archive = %d %v, want 200", status, body)
		}
		if got := body["quiz"].(map[string]any)["status"]; got != "archived" {
			t.Fatalf("re-archive status = %v, want archived", got)
		}
		var auditCount int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM audit_log WHERE action = 'quizzes.archived' AND resource_id = $1`, quizID).Scan(&auditCount); err != nil {
			t.Fatalf("count audit rows: %v", err)
		}
		if auditCount != 1 {
			t.Fatalf("archived audit rows after re-archive = %d, want 1 (no second row)", auditCount)
		}
	})
}
