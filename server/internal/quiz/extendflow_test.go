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

// TestExtendE2E pins the Milestone 6 live-quiz teacher control "extend ends_at"
// (docs/04: POST /quizzes/:id/extend "Live only"; docs/06 section 1: once Live
// the teacher can only extend, force-close, or kick). Extend moves ends_at
// later - owner-teacher only (a student is 403, an admin who cannot author and
// a non-owning teacher fail before the row is touched), a quiz that is not
// effectively live answers 409 QUIZ_NOT_LIVE, and a new ends_at not in the
// future or not later than the current close is a 422. A real extend flips
// ends_at forward, writes one audit row, enqueues a fresh close_quiz job at the
// new ends_at, and - the core property - hands every in-progress attempt back
// the time the old window clamped off by moving deadline_at forward (never
// earlier, so no attempt is auto-submitted early).
//
// It runs in its own database (macquiz_extendtest) - see itest.FreshDatabase.
func TestExtendE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_extendtest")
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

	// A published quiz swept into the stored 'live' state with a per-attempt
	// duration (1 hour) longer than the remaining window (20 minutes), so an
	// attempt starting now has its deadline_at clamped to the window - exactly
	// the case extend must repair.
	quizID := authorMinimalQuiz(t, server, owner, "Live Session")
	assign(t, server, owner, quizID, pupilID)
	if status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish",
		map[string]any{
			"starts_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"ends_at":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
			"duration_sec": 3600,
		}, owner); status != 200 {
		t.Fatalf("publish = %d %v", status, body)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE quizzes SET starts_at = now() - interval '1 minute',
		                    ends_at = now() + interval '20 minutes' WHERE id = $1`, quizID); err != nil {
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

	futureEnds := func() string { return time.Now().Add(90 * time.Minute).UTC().Format(time.RFC3339) }

	t.Run("only the owning teacher may extend", func(t *testing.T) {
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
				status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+tc.target+"/extend",
					map[string]any{"ends_at": futureEnds()}, tc.cookies)
				if status != tc.want {
					t.Fatalf("extend = %d %v, want %d", status, body, tc.want)
				}
			})
		}
	})

	t.Run("extend requires a future ends_at later than the current close", func(t *testing.T) {
		cases := []struct {
			name string
			body map[string]any
		}{
			{"missing ends_at", map[string]any{}},
			{"ends_at in the past", map[string]any{"ends_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}},
			{"ends_at before the current close", map[string]any{"ends_at": time.Now().Add(5 * time.Minute).UTC().Format(time.RFC3339)}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/extend", tc.body, owner)
				if status != 422 {
					t.Fatalf("extend = %d %v, want 422", status, body)
				}
			})
		}
		// Every refusal above left the window untouched.
		var stillTwentyish bool
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT ends_at < now() + interval '25 minutes' FROM quizzes WHERE id = $1`, quizID).Scan(&stillTwentyish); err != nil {
			t.Fatalf("read ends_at: %v", err)
		}
		if !stillTwentyish {
			t.Fatalf("ends_at moved despite refused extends")
		}
	})

	t.Run("a quiz that is not live cannot be extended", func(t *testing.T) {
		// A never-published draft reads as draft, not live.
		draftID := authorMinimalQuiz(t, server, owner, "Never Published")
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+draftID+"/extend",
			map[string]any{"ends_at": futureEnds()}, owner)
		if status != 409 || body["code"] != "QUIZ_NOT_LIVE" {
			t.Fatalf("extend draft = %d %v, want 409 QUIZ_NOT_LIVE", status, body)
		}
	})

	t.Run("a quiz whose window has passed reads as closed and cannot be extended", func(t *testing.T) {
		// Stored 'live' but with ends_at pulled into the past and NOT swept, so
		// only the read-time effectiveStatus knows it is over - the gate must
		// reject it (past-window is not extendable), pinned so it is deliberate.
		passedID := authorMinimalQuiz(t, server, owner, "Already Over")
		assign(t, server, owner, passedID, pupilID)
		if status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+passedID+"/publish",
			map[string]any{
				"starts_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
				"ends_at":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
				"duration_sec": 600,
			}, owner); status != 200 {
			t.Fatalf("publish = %d %v", status, body)
		}
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET status = 'live', starts_at = now() - interval '2 hours',
			                    ends_at = now() - interval '1 minute' WHERE id = $1`, passedID); err != nil {
			t.Fatalf("age window: %v", err)
		}
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+passedID+"/extend",
			map[string]any{"ends_at": futureEnds()}, owner)
		if status != 409 || body["code"] != "QUIZ_NOT_LIVE" {
			t.Fatalf("extend passed-window = %d %v, want 409 QUIZ_NOT_LIVE", status, body)
		}
	})

	// Two in-progress attempts inserted the way Start would (deadline_at =
	// least(started_at + duration, ends_at)): one clamped to the 20-minute
	// window (started now, so started+1h is clamped), one unclamped (started
	// 50 minutes ago, so started+1h < the window and least() leaves it alone).
	var clampedID, unclampedID string
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO attempts (quiz_id, student_id, attempt_no, quiz_version, started_at, deadline_at)
		 VALUES ($1, $2, 1, $3, now(), least(now() + make_interval(secs => 3600), (SELECT ends_at FROM quizzes WHERE id = $1)))
		 RETURNING id`, quizID, pupilID, version).Scan(&clampedID); err != nil {
		t.Fatalf("insert clamped attempt: %v", err)
	}
	provision(t, ctx, sqlDB, "student", "pupil2@school.test")
	pupil2ID := userID(t, ctx, sqlDB, "pupil2@school.test")
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO attempts (quiz_id, student_id, attempt_no, quiz_version, started_at, deadline_at)
		 VALUES ($1, $2, 1, $3, now() - interval '50 minutes', least((now() - interval '50 minutes') + make_interval(secs => 3600), (SELECT ends_at FROM quizzes WHERE id = $1)))
		 RETURNING id`, quizID, pupil2ID, version).Scan(&unclampedID); err != nil {
		t.Fatalf("insert unclamped attempt: %v", err)
	}
	var unclampedBefore time.Time
	if err := sqlDB.QueryRowContext(ctx, `SELECT deadline_at FROM attempts WHERE id = $1`, unclampedID).Scan(&unclampedBefore); err != nil {
		t.Fatalf("read unclamped deadline: %v", err)
	}

	// Three hours out, distinct from the original publish window (2h) so the
	// fresh close_quiz job is not confused with the one enqueued at publish.
	newEnds := time.Now().Add(3 * time.Hour).UTC()

	t.Run("the owner extends the live quiz", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/extend",
			map[string]any{"ends_at": newEnds.Format(time.RFC3339)}, owner)
		if status != 200 {
			t.Fatalf("extend = %d %v, want 200", status, body)
		}
		respQuiz := body["quiz"].(map[string]any)
		if got := respQuiz["status"]; got != "live" {
			t.Fatalf("response quiz.status = %v, want live", got)
		}
		// Parity with publish's response: the question count is populated, not
		// left 0 by quizColumns (Iteration 1 trap).
		if got := respQuiz["question_count"]; got != float64(1) {
			t.Fatalf("response quiz.question_count = %v, want 1", got)
		}
		// ends_at is now the extended value (roughly two hours out, well past
		// the original 20-minute window).
		var farOut bool
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT ends_at > now() + interval '90 minutes' FROM quizzes WHERE id = $1`, quizID).Scan(&farOut); err != nil {
			t.Fatalf("read ends_at: %v", err)
		}
		if !farOut {
			t.Fatalf("ends_at not extended")
		}
		// Exactly one audit row carrying the before/after window under the
		// changes convention (docs/08 section 7).
		var auditCount int
		var windowMovedLater bool
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*),
			        coalesce(bool_or((detail->'changes'->'ends_at'->>'to')::timestamptz
			                       > (detail->'changes'->'ends_at'->>'from')::timestamptz), false)
			 FROM audit_log
			 WHERE action = 'quizzes.extended' AND resource_id = $1`, quizID).Scan(&auditCount, &windowMovedLater); err != nil {
			t.Fatalf("count audit rows: %v", err)
		}
		if auditCount != 1 {
			t.Fatalf("extended audit rows = %d, want 1", auditCount)
		}
		if !windowMovedLater {
			t.Fatalf("audit ends_at diff does not record the old window moving later")
		}
		// A fresh close_quiz job scheduled at the new ends_at (distinct from the
		// original one enqueued at publish, which sits ~2h from publish time).
		var freshJobs int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM river_job
			 WHERE kind = 'close_quiz' AND args->>'quiz_id' = $1
			   AND scheduled_at BETWEEN $2::timestamptz - interval '1 minute' AND $2::timestamptz + interval '1 minute'`,
			quizID, newEnds.Format(time.RFC3339)).Scan(&freshJobs); err != nil {
			t.Fatalf("count fresh close jobs: %v", err)
		}
		if freshJobs != 1 {
			t.Fatalf("fresh close_quiz jobs at new ends_at = %d, want 1", freshJobs)
		}
	})

	t.Run("extend moves clamped deadlines forward without submitting early", func(t *testing.T) {
		// The clamped attempt gets its full personal budget back: deadline_at is
		// now least(started_at + 1h, new ends_at) = started_at + 1h, well past
		// the old 20-minute clamp.
		var clampedFarOut bool
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT deadline_at > now() + interval '50 minutes' FROM attempts WHERE id = $1`, clampedID).Scan(&clampedFarOut); err != nil {
			t.Fatalf("read clamped deadline: %v", err)
		}
		if !clampedFarOut {
			t.Fatalf("clamped attempt deadline was not moved forward")
		}
		// The unclamped attempt (started + duration was already inside the
		// window) is untouched by least().
		var unclampedAfter time.Time
		if err := sqlDB.QueryRowContext(ctx, `SELECT deadline_at FROM attempts WHERE id = $1`, unclampedID).Scan(&unclampedAfter); err != nil {
			t.Fatalf("read unclamped deadline: %v", err)
		}
		if !unclampedAfter.Equal(unclampedBefore) {
			t.Fatalf("unclamped deadline changed: %v -> %v", unclampedBefore, unclampedAfter)
		}
		// The safety property: because deadline_at only moved forward, a sweep
		// running now (as a stale per-attempt deadline job would) auto-submits
		// neither attempt - no one is terminated early by the extend.
		if _, _, err := attempt.SweepDueAttempts(ctx, sqlDB); err != nil {
			t.Fatalf("sweep due attempts: %v", err)
		}
		for _, id := range []string{clampedID, unclampedID} {
			var status string
			if err := sqlDB.QueryRowContext(ctx, `SELECT status FROM attempts WHERE id = $1`, id).Scan(&status); err != nil {
				t.Fatalf("read attempt status: %v", err)
			}
			if status != "in_progress" {
				t.Fatalf("attempt %s status after sweep = %q, want in_progress (no early submit)", id, status)
			}
		}
	})
}
