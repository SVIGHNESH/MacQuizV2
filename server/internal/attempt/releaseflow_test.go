package attempt_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"macquiz/server/internal/analytics"
	"macquiz/server/internal/attempt"
	"macquiz/server/internal/authusers"
	"macquiz/server/internal/db"
	"macquiz/server/internal/httpserver"
	"macquiz/server/internal/itest"
	"macquiz/server/internal/quiz"
)

// TestReleaseFlowE2E pins the results-release half of Milestone 4 (docs/01
// open question 1, docs/08 section 3): the publish-time policy, the withheld
// score before release, the teacher's results table and explicit release,
// the released student review with the answer key, and the worker pass
// auto-releasing an auto-policy quiz only after its grading lands.
//
// It runs in its own database (macquiz_releasetest) - see itest.FreshDatabase.
func TestReleaseFlowE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_releasetest")
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
	provision(t, ctx, sqlDB, "student", "alpha@school.test")
	provision(t, ctx, sqlDB, "student", "beta@school.test")

	teacher := login(t, server, "owner@school.test", "account-password")
	alpha := login(t, server, "alpha@school.test", "account-password")
	beta := login(t, server, "beta@school.test", "account-password")
	alphaUserID := userID(t, ctx, sqlDB, "alpha@school.test")
	betaUserID := userID(t, ctx, sqlDB, "beta@school.test")

	// buildQuiz publishes a two-question quiz (single worth 2, truefalse
	// worth 1) assigned to both students and backdates starts_at so attempts
	// can begin immediately.
	buildQuiz := func(title string, publishExtra map[string]any) (quizID, singleID, truefalseID string) {
		t.Helper()
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
			map[string]string{"title": title}, teacher)
		if status != 201 {
			t.Fatalf("create quiz = %d %v", status, body)
		}
		quizID = body["quiz"].(map[string]any)["id"].(string)

		addQuestion := func(q map[string]any) string {
			t.Helper()
			status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", q, teacher)
			if status != 201 {
				t.Fatalf("add question = %d %v", status, body)
			}
			return body["question"].(map[string]any)["id"].(string)
		}
		singleID = addQuestion(map[string]any{
			"type": "single", "body": map[string]string{"text": "Pick b."},
			"options": []map[string]string{{"key": "a", "text": "A"}, {"key": "b", "text": "B"}},
			"correct": "b", "points": 2,
		})
		truefalseID = addQuestion(map[string]any{
			"type": "truefalse", "body": map[string]string{"text": "Releases are gated."},
			"correct": true,
		})

		status, _, _ = itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
			map[string]any{"student_ids": []string{alphaUserID, betaUserID}}, teacher)
		if status != 200 {
			t.Fatalf("assign = %d", status)
		}
		publish := map[string]any{
			"starts_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"ends_at":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
			"duration_sec": 600,
		}
		for k, v := range publishExtra {
			publish[k] = v
		}
		status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", publish, teacher)
		if status != 200 {
			t.Fatalf("publish = %d %v", status, body)
		}
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET starts_at = now() - interval '1 minute' WHERE id = $1`, quizID); err != nil {
			t.Fatalf("backdate starts_at: %v", err)
		}
		return quizID, singleID, truefalseID
	}
	start := func(cookies map[string]string, quizID string) string {
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
	submit := func(cookies map[string]string, attemptID string) {
		t.Helper()
		if status, body, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+attemptID+"/submit", nil, cookies); status != 200 {
			t.Fatalf("submit = %d %v", status, body)
		}
	}
	closeQuiz := func(quizID string) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET ends_at = now() - interval '1 second' WHERE id = $1`, quizID); err != nil {
			t.Fatalf("backdate ends_at: %v", err)
		}
	}
	// workerPass mirrors worker.sweepDue: quiz flips, attempt sweep,
	// grading, then the auto-policy release.
	workerPass := func() int64 {
		t.Helper()
		if _, _, err := quiz.SweepDueQuizzes(ctx, sqlDB); err != nil {
			t.Fatalf("sweep quizzes: %v", err)
		}
		if _, _, err := attempt.SweepDueAttempts(ctx, sqlDB); err != nil {
			t.Fatalf("sweep attempts: %v", err)
		}
		if _, err := attempt.GradeSubmitted(ctx, sqlDB); err != nil {
			t.Fatalf("grade submitted: %v", err)
		}
		released, err := quiz.ReleaseDueResults(ctx, sqlDB)
		if err != nil {
			t.Fatalf("release due results: %v", err)
		}
		if _, err := analytics.RollupDue(ctx, sqlDB); err != nil {
			t.Fatalf("rollup due: %v", err)
		}
		return released
	}

	quizID, singleID, truefalseID := buildQuiz("Manual Release", map[string]any{"release_policy": "manual"})

	t.Run("publish stores the release policy", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/quizzes/"+quizID, nil, teacher)
		if status != 200 {
			t.Fatalf("get quiz = %d %v", status, body)
		}
		q := body["quiz"].(map[string]any)
		if q["release_policy"] != "manual" {
			t.Fatalf("release_policy = %v, want manual", q["release_policy"])
		}
		if q["results_released_at"] != nil {
			t.Fatalf("results_released_at = %v, want null at publish", q["results_released_at"])
		}
	})

	// Alpha scores 3/3, beta 1/3; both graded before the quiz closes.
	alphaAttempt := start(alpha, quizID)
	save(alpha, alphaAttempt, singleID, "b")
	save(alpha, alphaAttempt, truefalseID, true)
	submit(alpha, alphaAttempt)
	betaAttempt := start(beta, quizID)
	save(beta, betaAttempt, truefalseID, true)
	submit(beta, betaAttempt)
	if graded, err := attempt.GradeSubmitted(ctx, sqlDB); err != nil || graded != 2 {
		t.Fatalf("grade = (%d, %v), want (2, nil)", graded, err)
	}

	t.Run("the assigned list withholds the score before release", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/quizzes/assigned", nil, alpha)
		if status != 200 {
			t.Fatalf("assigned = %d %v", status, body)
		}
		q := body["quizzes"].([]any)[0].(map[string]any)
		if q["results_released_at"] != nil {
			t.Fatalf("results_released_at = %v, want null", q["results_released_at"])
		}
		a := q["attempts"].([]any)[0].(map[string]any)
		if a["status"] != "graded" || a["score"] != nil {
			t.Fatalf("attempt summary = %v/%v, want graded with null score", a["status"], a["score"])
		}
	})

	t.Run("the student result answers 409 before release", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/attempts/"+alphaAttempt+"/result", nil, alpha)
		if status != 409 || body["code"] != "RESULTS_NOT_RELEASED" {
			t.Fatalf("result before release = %d %v, want 409 RESULTS_NOT_RELEASED", status, body)
		}
	})

	t.Run("release before close answers 409 QUIZ_NOT_CLOSED", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/release-results", nil, teacher)
		if status != 409 || body["code"] != "QUIZ_NOT_CLOSED" {
			t.Fatalf("early release = %d %v, want 409 QUIZ_NOT_CLOSED", status, body)
		}
	})

	t.Run("the teacher results table shows scores before release", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/quizzes/"+quizID+"/results", nil, teacher)
		if status != 200 {
			t.Fatalf("results = %d %v", status, body)
		}
		rows := body["results"].([]any)
		if len(rows) != 2 {
			t.Fatalf("results rows = %d, want 2", len(rows))
		}
		scores := map[string]float64{}
		for _, raw := range rows {
			r := raw.(map[string]any)
			if r["max_score"].(float64) != 3 {
				t.Fatalf("max_score = %v, want 3", r["max_score"])
			}
			scores[r["student_id"].(string)] = r["score"].(float64)
		}
		if scores[alphaUserID] != 3 || scores[betaUserID] != 1 {
			t.Fatalf("scores = %v, want alpha 3 and beta 1", scores)
		}
	})

	t.Run("a manual-policy quiz never auto-releases", func(t *testing.T) {
		closeQuiz(quizID)
		if released := workerPass(); released != 0 {
			t.Fatalf("worker pass released %d manual-policy quizzes, want 0", released)
		}
		var releasedAt *time.Time
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT results_released_at FROM quizzes WHERE id = $1`, quizID).Scan(&releasedAt); err != nil {
			t.Fatalf("read release stamp: %v", err)
		}
		if releasedAt != nil {
			t.Fatalf("manual quiz released at %v by the sweep", releasedAt)
		}
	})

	t.Run("students cannot release results", func(t *testing.T) {
		status, _, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/release-results", nil, alpha)
		if status != 403 {
			t.Fatalf("student release = %d, want 403", status)
		}
	})

	t.Run("the teacher's release is idempotent and audited", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/release-results", nil, teacher)
		if status != 200 {
			t.Fatalf("release = %d %v", status, body)
		}
		first := body["quiz"].(map[string]any)["results_released_at"]
		if first == nil {
			t.Fatalf("release did not stamp results_released_at: %v", body)
		}
		status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/release-results", nil, teacher)
		if status != 200 || body["quiz"].(map[string]any)["results_released_at"] != first {
			t.Fatalf("repeat release = %d %v, want the same stamp %v", status, body, first)
		}
		var audits int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM audit_log WHERE action = 'quizzes.results_released' AND resource_id = $1`,
			quizID).Scan(&audits); err != nil {
			t.Fatalf("count audit rows: %v", err)
		}
		if audits != 1 {
			t.Fatalf("release audit rows = %d, want exactly 1 (idempotent repeat writes none)", audits)
		}
	})

	t.Run("the released review carries score, key, and per-question grading", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/attempts/"+alphaAttempt+"/result", nil, alpha)
		if status != 200 {
			t.Fatalf("result = %d %v", status, body)
		}
		if body["score"].(float64) != 3 || body["max_score"].(float64) != 3 {
			t.Fatalf("score = %v/%v, want 3/3", body["score"], body["max_score"])
		}
		// The quiz_stats rollup ran in the earlier "never auto-releases"
		// subtest's workerPass(). Alpha (3/3) lands in the top bucket, beta
		// (1/3) in a lower one: percentile-rank = (below + 0.5*same) / total
		// * 100 = (1 + 0.5) / 2 * 100 = 75.
		if percentile, ok := body["percentile"].(float64); !ok || percentile != 75 {
			t.Fatalf("percentile = %v, want 75", body["percentile"])
		}
		for _, raw := range body["questions"].([]any) {
			q := raw.(map[string]any)
			if _, hasKey := q["correct"]; !hasKey {
				t.Fatalf("released question withholds the answer key: %v", q)
			}
			if q["is_correct"] != true {
				t.Fatalf("question %v graded %v, want true", q["id"], q["is_correct"])
			}
		}
		// The assigned list now shows the score too.
		status, body, _ = itest.Call(t, server, "GET", "/api/v1/quizzes/assigned", nil, alpha)
		if status != 200 {
			t.Fatalf("assigned = %d %v", status, body)
		}
		a := body["quizzes"].([]any)[0].(map[string]any)["attempts"].([]any)[0].(map[string]any)
		if a["score"] != 3.0 {
			t.Fatalf("assigned-list score after release = %v, want 3", a["score"])
		}
	})

	t.Run("a released result stays owner-only", func(t *testing.T) {
		status, _, _ := itest.Call(t, server, "GET", "/api/v1/attempts/"+alphaAttempt+"/result", nil, beta)
		if status != 404 {
			t.Fatalf("cross-student result = %d, want 404", status)
		}
	})

	t.Run("an auto-policy quiz releases in the worker pass that grades it", func(t *testing.T) {
		autoQuizID, autoSingleID, _ := buildQuiz("Auto Release", nil)
		autoAttempt := start(alpha, autoQuizID)
		save(alpha, autoAttempt, autoSingleID, "b")
		// The student disappears; the quiz closes with the attempt open. One
		// worker pass must close, force-submit, grade, and release.
		closeQuiz(autoQuizID)
		if released := workerPass(); released != 1 {
			t.Fatalf("worker pass released %d quizzes, want 1", released)
		}
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/attempts/"+autoAttempt+"/result", nil, alpha)
		if status != 200 {
			t.Fatalf("auto-released result = %d %v", status, body)
		}
		if body["score"].(float64) != 2 {
			t.Fatalf("auto-released score = %v, want 2", body["score"])
		}
		kind := body["attempt"].(map[string]any)["submit_kind"]
		if kind != "forced" {
			t.Fatalf("submit_kind = %v, want forced", kind)
		}
	})
}
