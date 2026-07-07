package quiz_test

import (
	"context"
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

// TestForceCloseE2E pins the Milestone 6 live-quiz teacher control
// "force-close early" (docs/06 section 1): POST /quizzes/:id/close ends a live
// or scheduled quiz immediately - owner-teacher only (a student is 403, a
// non-owning teacher and an admin, who cannot author, both fail before the
// row is touched), a draft (never opened) answers 409 QUIZ_NOT_LIVE. A real
// force-close flips the stored row to closed, brings ends_at to now(), writes
// one audit row, and enqueues one immediate close_quiz job so the same worker
// chain a timed close runs force-submits every still-open attempt with
// kind='forced'. Re-closing an already-closed quiz is an idempotent no-op.
//
// It runs in its own database (macquiz_forceclosetest) - see itest.FreshDatabase.
func TestForceCloseE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_forceclosetest")
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

	// A published quiz, rewound and swept into the stored 'live' state so
	// force-close has a real live window to end (publish rejects a past
	// starts_at, so the window is set in the future then rewound in SQL,
	// exactly as the scheduler suite does).
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
	if got := storedStatus(t, ctx, sqlDB, quizID); got != "live" {
		t.Fatalf("stored status after sweep = %q, want live", got)
	}
	var version int
	if err := sqlDB.QueryRowContext(ctx, `SELECT version FROM quizzes WHERE id = $1`, quizID).Scan(&version); err != nil {
		t.Fatalf("read version: %v", err)
	}

	t.Run("only the owning teacher may force-close", func(t *testing.T) {
		// None of these reach the UPDATE, so the live quiz is untouched and
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
				status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+tc.target+"/close", nil, tc.cookies)
				if status != tc.want {
					t.Fatalf("force-close = %d %v, want %d", status, body, tc.want)
				}
			})
		}
		if got := storedStatus(t, ctx, sqlDB, quizID); got != "live" {
			t.Fatalf("stored status after refused force-closes = %q, want live", got)
		}
	})

	t.Run("a draft quiz cannot be force-closed", func(t *testing.T) {
		draftID := authorMinimalQuiz(t, server, owner, "Never Published")
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+draftID+"/close", nil, owner)
		if status != 409 {
			t.Fatalf("force-close draft = %d %v, want 409", status, body)
		}
		if code := body["code"]; code != "QUIZ_NOT_LIVE" {
			t.Fatalf("force-close draft code = %v, want QUIZ_NOT_LIVE", code)
		}
	})

	// An in-progress attempt on the live quiz, inserted directly, so the close
	// chain has an open attempt to force-submit.
	var attemptID string
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO attempts (quiz_id, student_id, attempt_no, quiz_version, started_at, deadline_at)
		 VALUES ($1, $2, 1, $3, now(), now() + interval '30 minutes')
		 RETURNING id`, quizID, pupilID, version).Scan(&attemptID); err != nil {
		t.Fatalf("insert in-progress attempt: %v", err)
	}

	t.Run("the owner force-closes the live quiz", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/close", nil, owner)
		if status != 200 {
			t.Fatalf("force-close = %d %v, want 200", status, body)
		}
		respQuiz := body["quiz"].(map[string]any)
		if got := respQuiz["status"]; got != "closed" {
			t.Fatalf("response quiz.status = %v, want closed", got)
		}
		// Parity with publish's response: the question count is populated, not
		// left 0 by quizColumns (Iteration 1 trap).
		if got := respQuiz["question_count"]; got != float64(1) {
			t.Fatalf("response quiz.question_count = %v, want 1", got)
		}
		if got := storedStatus(t, ctx, sqlDB, quizID); got != "closed" {
			t.Fatalf("stored status = %q, want closed", got)
		}
		// ends_at was pulled forward to now(): it must no longer be in the future.
		var endsInFuture bool
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT ends_at > now() FROM quizzes WHERE id = $1`, quizID).Scan(&endsInFuture); err != nil {
			t.Fatalf("read ends_at: %v", err)
		}
		if endsInFuture {
			t.Fatalf("ends_at still in the future after force-close")
		}
		// Exactly one audit row, carrying the pre-close status.
		var auditCount int
		var fromStatus string
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*), coalesce(max(detail->>'from_status'), '') FROM audit_log
			 WHERE action = 'quizzes.force_closed' AND resource_id = $1`, quizID).Scan(&auditCount, &fromStatus); err != nil {
			t.Fatalf("count audit rows: %v", err)
		}
		if auditCount != 1 {
			t.Fatalf("force_closed audit rows = %d, want 1", auditCount)
		}
		if fromStatus != "live" {
			t.Fatalf("audit from_status = %q, want live", fromStatus)
		}
		// One immediate close_quiz job (scheduled at ~now, distinct from the
		// original future-dated one enqueued at publish).
		var immediateJobs int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM river_job
			 WHERE kind = 'close_quiz' AND args->>'quiz_id' = $1 AND scheduled_at <= now()`, quizID).Scan(&immediateJobs); err != nil {
			t.Fatalf("count immediate close jobs: %v", err)
		}
		if immediateJobs != 1 {
			t.Fatalf("immediate close_quiz jobs = %d, want 1", immediateJobs)
		}
	})

	t.Run("the close chain force-submits the open attempt", func(t *testing.T) {
		// The status flip is what SweepDueAttempts keys the force-submit on:
		// running it (as the enqueued close_quiz job would) terminates the
		// still-open attempt as kind='forced'.
		if _, _, err := attempt.SweepDueAttempts(ctx, sqlDB); err != nil {
			t.Fatalf("sweep due attempts: %v", err)
		}
		var status, submitKind string
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT status, coalesce(submit_kind::text, '') FROM attempts WHERE id = $1`, attemptID).Scan(&status, &submitKind); err != nil {
			t.Fatalf("read attempt: %v", err)
		}
		if status != "submitted" || submitKind != "forced" {
			t.Fatalf("attempt = %q/%q, want submitted/forced", status, submitKind)
		}
	})

	t.Run("re-closing an already-closed quiz is an idempotent no-op", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/close", nil, owner)
		if status != 200 {
			t.Fatalf("re-close = %d %v, want 200", status, body)
		}
		var auditCount int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM audit_log WHERE action = 'quizzes.force_closed' AND resource_id = $1`, quizID).Scan(&auditCount); err != nil {
			t.Fatalf("count audit rows: %v", err)
		}
		if auditCount != 1 {
			t.Fatalf("force_closed audit rows after re-close = %d, want 1 (no second row)", auditCount)
		}
		var immediateJobs int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM river_job
			 WHERE kind = 'close_quiz' AND args->>'quiz_id' = $1 AND scheduled_at <= now()`, quizID).Scan(&immediateJobs); err != nil {
			t.Fatalf("count immediate close jobs: %v", err)
		}
		if immediateJobs != 1 {
			t.Fatalf("immediate close_quiz jobs after re-close = %d, want 1 (no second job)", immediateJobs)
		}
	})
}
