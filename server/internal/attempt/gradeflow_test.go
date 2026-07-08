package attempt_test

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

// TestGradeFlowE2E pins the grading side of the attempt lifecycle (docs/04
// section 4, docs/12 Milestone 4): the grade job committed with the manual
// submit, the deterministic score over all four question types at the pinned
// snapshot version, idempotent reruns, the sweep pass grading what it
// terminates, and the player payload still withholding the score.
//
// It runs in its own database (macquiz_gradetest) - see itest.FreshDatabase.
func TestGradeFlowE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_gradetest")
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
	provision(t, ctx, sqlDB, "student", "scholar@school.test")
	provision(t, ctx, sqlDB, "student", "vanisher@school.test")

	teacher := login(t, server, "owner@school.test", "account-password")
	scholar := login(t, server, "scholar@school.test", "account-password")
	vanisher := login(t, server, "vanisher@school.test", "account-password")

	// The quiz under test: all four question types with distinct weights, so
	// the expected score is unambiguous about which answers earned points.
	status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
		map[string]string{"title": "Grading Gauntlet"}, teacher)
	if status != 201 {
		t.Fatalf("create quiz = %d %v", status, body)
	}
	quizID := body["quiz"].(map[string]any)["id"].(string)

	addQuestion := func(q map[string]any) string {
		t.Helper()
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", q, teacher)
		if status != 201 {
			t.Fatalf("add question = %d %v", status, body)
		}
		return body["question"].(map[string]any)["id"].(string)
	}
	singleID := addQuestion(map[string]any{
		"type": "single", "body": map[string]string{"text": "Pick b."},
		"options": []map[string]string{{"key": "a", "text": "A"}, {"key": "b", "text": "B"}},
		"correct": "b", "points": 2,
	})
	multiID := addQuestion(map[string]any{
		"type": "multi", "body": map[string]string{"text": "Pick a and c."},
		"options": []map[string]string{{"key": "a", "text": "A"}, {"key": "b", "text": "B"}, {"key": "c", "text": "C"}},
		"correct": []string{"a", "c"}, "points": 3,
	})
	truefalseID := addQuestion(map[string]any{
		"type": "truefalse", "body": map[string]string{"text": "Grading is deterministic."},
		"correct": true,
	})
	shortID := addQuestion(map[string]any{
		"type": "short", "body": map[string]string{"text": "Capital of France?"},
		"correct": map[string]any{"accepted": []string{"Paris"}}, "points": 2,
	})

	status, _, _ = itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
		map[string]any{"student_ids": []string{
			userID(t, ctx, sqlDB, "scholar@school.test"),
			userID(t, ctx, sqlDB, "vanisher@school.test"),
		}}, teacher)
	if status != 200 {
		t.Fatalf("assign = %d", status)
	}
	status, _, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", map[string]any{
		"starts_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"ends_at":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		"duration_sec": 600,
	}, teacher)
	if status != 200 {
		t.Fatalf("publish = %d", status)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE quizzes SET starts_at = now() - interval '1 minute' WHERE id = $1`, quizID); err != nil {
		t.Fatalf("backdate starts_at: %v", err)
	}

	start := func(cookies map[string]string) string {
		t.Helper()
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, cookies)
		if status != 200 && status != 201 {
			t.Fatalf("start = %d %v", status, body)
		}
		return body["attempt"].(map[string]any)["id"].(string)
	}
	save := func(cookies map[string]string, attemptID, questionID string, response any) {
		t.Helper()
		status, body, _ := itest.Call(t, server, "PUT",
			"/api/v1/attempts/"+attemptID+"/answers/"+questionID,
			map[string]any{"response": response}, cookies)
		if status != 200 {
			t.Fatalf("autosave = %d %v", status, body)
		}
	}
	attemptScore := func(id string) (status string, score *float64) {
		t.Helper()
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT status, score FROM attempts WHERE id = $1`, id).Scan(&status, &score); err != nil {
			t.Fatalf("read attempt %s: %v", id, err)
		}
		return status, score
	}
	gradeJobs := func(attemptID string) int {
		t.Helper()
		var jobs int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM river_job
			 WHERE kind = 'grade_attempt' AND args->>'attempt_id' = $1`, attemptID).Scan(&jobs); err != nil {
			t.Fatalf("count grade jobs: %v", err)
		}
		return jobs
	}

	scholarID := start(scholar)
	// Right on single (+2), a subset on multi (all-or-nothing, +0), right on
	// truefalse (+1), and a messy-but-normalizing short answer (+2): score 5.
	save(scholar, scholarID, singleID, "b")
	save(scholar, scholarID, multiID, []string{"a"})
	save(scholar, scholarID, truefalseID, true)
	save(scholar, scholarID, shortID, "  PARIS ")

	t.Run("manual submit commits the grading job with the terminal flip", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+scholarID+"/submit", nil, scholar)
		if status != 200 {
			t.Fatalf("submit = %d %v", status, body)
		}
		if jobs := gradeJobs(scholarID); jobs != 1 {
			t.Fatalf("grade jobs after submit = %d, want 1", jobs)
		}
		// A repeat submit is the idempotent no-op; it must not enqueue again.
		if status, _, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+scholarID+"/submit", nil, scholar); status != 200 {
			t.Fatalf("repeat submit = %d", status)
		}
		if jobs := gradeJobs(scholarID); jobs != 1 {
			t.Fatalf("grade jobs after repeat submit = %d, want 1", jobs)
		}
	})

	t.Run("grading scores all four types against the snapshot key", func(t *testing.T) {
		graded, err := attempt.GradeSubmitted(ctx, sqlDB)
		if err != nil || graded != 1 {
			t.Fatalf("grade = (%d, %v), want (1, nil)", graded, err)
		}
		if st, score := attemptScore(scholarID); st != "graded" || score == nil || *score != 5 {
			t.Fatalf("scholar attempt = %s/%v, want graded/5", st, score)
		}
		wantAnswers := map[string]struct {
			correct bool
			points  float64
		}{
			singleID:    {true, 2},
			multiID:     {false, 0},
			truefalseID: {true, 1},
			shortID:     {true, 2},
		}
		for qid, want := range wantAnswers {
			var correct bool
			var points float64
			if err := sqlDB.QueryRowContext(ctx,
				`SELECT is_correct, points_awarded FROM attempt_answers
				 WHERE attempt_id = $1 AND question_id = $2`, scholarID, qid).Scan(&correct, &points); err != nil {
				t.Fatalf("read graded answer %s: %v", qid, err)
			}
			if correct != want.correct || points != want.points {
				t.Fatalf("answer %s graded (%v, %v), want (%v, %v)",
					qid, correct, points, want.correct, want.points)
			}
		}
	})

	t.Run("regrading is a no-op once graded", func(t *testing.T) {
		graded, err := attempt.GradeSubmitted(ctx, sqlDB)
		if err != nil || graded != 0 {
			t.Fatalf("regrade = (%d, %v), want (0, nil)", graded, err)
		}
		if st, score := attemptScore(scholarID); st != "graded" || score == nil || *score != 5 {
			t.Fatalf("scholar attempt after regrade = %s/%v, want graded/5 unchanged", st, score)
		}
	})

	t.Run("the sweep pass grades the disappearing student it auto-submits", func(t *testing.T) {
		// The vanisher answered one question and closed the laptop; the same
		// worker pass that auto-submits past deadline+grace grades the work,
		// with no grade job needed (worker.sweepDue ordering).
		vanisherID := start(vanisher)
		save(vanisher, vanisherID, truefalseID, true)
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE attempts SET deadline_at = now() - interval '6 seconds' WHERE id = $1`, vanisherID); err != nil {
			t.Fatalf("expire attempt: %v", err)
		}
		auto, forced, err := attempt.SweepDueAttempts(ctx, sqlDB)
		if err != nil || auto != 1 || forced != 0 {
			t.Fatalf("sweep = (%d, %d, %v), want (1, 0, nil)", auto, forced, err)
		}
		graded, err := attempt.GradeSubmitted(ctx, sqlDB)
		if err != nil || graded != 1 {
			t.Fatalf("grade after sweep = (%d, %v), want (1, nil)", graded, err)
		}
		if st, score := attemptScore(vanisherID); st != "graded" || score == nil || *score != 1 {
			t.Fatalf("vanisher attempt = %s/%v, want graded/1", st, score)
		}
		if jobs := gradeJobs(vanisherID); jobs != 0 {
			t.Fatalf("sweep-terminated attempt has %d grade jobs, want 0", jobs)
		}
	})

	t.Run("the player payload still withholds the score", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/attempts/"+scholarID, nil, scholar)
		if status != 200 {
			t.Fatalf("resume = %d %v", status, body)
		}
		a := body["attempt"].(map[string]any)
		if a["status"] != "graded" {
			t.Fatalf("attempt status = %v, want graded", a["status"])
		}
		if _, leaked := a["score"]; leaked {
			t.Fatalf("player payload leaks the score before results release: %v", a)
		}
		for _, q := range body["questions"].([]any) {
			if _, leaked := q.(map[string]any)["correct"]; leaked {
				t.Fatalf("player payload leaks the answer key after grading: %v", q)
			}
		}
	})
}
