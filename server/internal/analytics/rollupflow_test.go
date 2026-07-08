package analytics_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"math"
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

// TestRollupFlowE2E pins the Milestone 8 rollup-on-close job (docs/07 section
// 4): the inline sweep step analytics.RollupDue writes one quiz_stats row per
// closed-and-fully-graded quiz - score distribution, mean, median,
// participation - exactly once, skipping a quiz whose grading has not yet
// settled and a quiz that already has a row.
//
// It runs in its own database (macquiz_rolluptest) - see itest.FreshDatabase.
func TestRollupFlowE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_rolluptest")
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
	provision(t, ctx, sqlDB, "student", "scholar@school.test")
	provision(t, ctx, sqlDB, "student", "learner@school.test")
	provision(t, ctx, sqlDB, "student", "absent@school.test")

	teacher := login(t, server, "owner@school.test", "account-password")
	scholar := login(t, server, "scholar@school.test", "account-password")
	learner := login(t, server, "learner@school.test", "account-password")

	// buildQuiz creates a live quiz with a single (4 pts) + truefalse (6 pts)
	// pair - a max score of 10, so a raw score reads directly as a percentage
	// and the distribution bucket is unambiguous - assigns the given students,
	// publishes, and backdates it live. It returns the quiz and its two
	// question ids.
	buildQuiz := func(title string, studentEmails ...string) (quizID, singleID, tfID string) {
		t.Helper()
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
			map[string]string{"title": title}, teacher)
		if status != 201 {
			t.Fatalf("create quiz = %d %v", status, body)
		}
		quizID = body["quiz"].(map[string]any)["id"].(string)

		addQuestion := func(q map[string]any) string {
			status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", q, teacher)
			if status != 201 {
				t.Fatalf("add question = %d %v", status, body)
			}
			return body["question"].(map[string]any)["id"].(string)
		}
		singleID = addQuestion(map[string]any{
			"type": "single", "body": map[string]string{"text": "Pick b."},
			"options": []map[string]string{{"key": "a", "text": "A"}, {"key": "b", "text": "B"}},
			"correct": "b", "points": 4,
		})
		tfID = addQuestion(map[string]any{
			"type": "truefalse", "body": map[string]string{"text": "True?"},
			"correct": true, "points": 6,
		})

		ids := make([]string, len(studentEmails))
		for i, e := range studentEmails {
			ids[i] = userID(t, ctx, sqlDB, e)
		}
		if status, _, _ := itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
			map[string]any{"student_ids": ids}, teacher); status != 200 {
			t.Fatalf("assign = %d", status)
		}
		if status, _, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", map[string]any{
			"starts_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"ends_at":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
			"duration_sec": 600,
		}, teacher); status != 200 {
			t.Fatalf("publish = %d", status)
		}
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET starts_at = now() - interval '1 minute' WHERE id = $1`, quizID); err != nil {
			t.Fatalf("backdate starts_at: %v", err)
		}
		return quizID, singleID, tfID
	}

	// attemptQuiz starts an attempt, saves the given answers, and submits it.
	attemptQuiz := func(cookies map[string]string, quizID string, answers map[string]any) string {
		t.Helper()
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/attempts", nil, cookies)
		if status != 200 && status != 201 {
			t.Fatalf("start = %d %v", status, body)
		}
		attemptID := body["attempt"].(map[string]any)["id"].(string)
		for qid, resp := range answers {
			if status, b, _ := itest.Call(t, server, "PUT",
				"/api/v1/attempts/"+attemptID+"/answers/"+qid,
				map[string]any{"response": resp}, cookies); status != 200 {
				t.Fatalf("autosave = %d %v", status, b)
			}
		}
		if status, b, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+attemptID+"/submit", nil, cookies); status != 200 {
			t.Fatalf("submit = %d %v", status, b)
		}
		return attemptID
	}

	setStatus := func(quizID, status string) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET status = $1::quiz_status, ends_at = now() WHERE id = $2`, status, quizID); err != nil {
			t.Fatalf("set status %s: %v", status, err)
		}
	}

	// The graded quiz under test: scholar aces it (10/10), learner gets the
	// single only (4/10), absent never attempts. Assigned 3, attempted 2.
	gradedQuiz, gSingle, gTF := buildQuiz("Graded", "scholar@school.test", "learner@school.test", "absent@school.test")
	attemptQuiz(scholar, gradedQuiz, map[string]any{gSingle: "b", gTF: true})
	attemptQuiz(learner, gradedQuiz, map[string]any{gSingle: "b"})

	// An archived quiz is a terminal superset of closed and must roll up too.
	archivedQuiz, aSingle, aTF := buildQuiz("Archived", "scholar@school.test")
	attemptQuiz(scholar, archivedQuiz, map[string]any{aSingle: "b", aTF: true})

	if graded, err := attempt.GradeSubmitted(ctx, sqlDB); err != nil || graded != 3 {
		t.Fatalf("grade = (%d, %v), want (3, nil)", graded, err)
	}
	setStatus(gradedQuiz, "closed")
	setStatus(archivedQuiz, "archived")

	// A closed quiz with zero attempts must still get a row, or it stays
	// perpetually due and recomputes every sweep.
	emptyQuiz, _, _ := buildQuiz("Empty", "scholar@school.test")
	setStatus(emptyQuiz, "closed")

	// A closed quiz whose grading has not settled (a submitted, ungraded
	// attempt) must be skipped until its scores stop moving.
	ungradedQuiz, uSingle, uTF := buildQuiz("Ungraded", "scholar@school.test")
	attemptQuiz(scholar, ungradedQuiz, map[string]any{uSingle: "b", uTF: true})
	setStatus(ungradedQuiz, "closed")

	hasStats := func(quizID string) bool {
		t.Helper()
		var n int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM quiz_stats WHERE quiz_id = $1`, quizID).Scan(&n); err != nil {
			t.Fatalf("count quiz_stats: %v", err)
		}
		return n == 1
	}

	t.Run("rolls up every closed-and-graded quiz, skipping the ungraded one", func(t *testing.T) {
		rolled, err := analytics.RollupDue(ctx, sqlDB)
		if err != nil || rolled != 3 {
			t.Fatalf("rollup = (%d, %v), want (3, nil)", rolled, err)
		}
		if !hasStats(gradedQuiz) || !hasStats(archivedQuiz) || !hasStats(emptyQuiz) {
			t.Fatalf("graded/archived/empty quiz missing a quiz_stats row")
		}
		if hasStats(ungradedQuiz) {
			t.Fatalf("ungraded quiz was rolled up before grading settled")
		}
	})

	t.Run("the graded quiz row carries the right summary", func(t *testing.T) {
		var mean, median, participation sql.NullFloat64
		var distribution, itemAnalysis, integrity []byte
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT distribution, mean, median, participation, item_analysis, integrity
			 FROM quiz_stats WHERE quiz_id = $1`, gradedQuiz).Scan(
			&distribution, &mean, &median, &participation, &itemAnalysis, &integrity); err != nil {
			t.Fatalf("read quiz_stats: %v", err)
		}
		// item_analysis and integrity are a follow-up brick: left NULL for now.
		if itemAnalysis != nil || integrity != nil {
			t.Fatalf("item_analysis/integrity = %s/%s, want null/null (deferred)", itemAnalysis, integrity)
		}
		// Scores 10 and 4 -> mean 7, median 7.
		if !mean.Valid || mean.Float64 != 7 {
			t.Fatalf("mean = %v, want 7", mean)
		}
		if !median.Valid || median.Float64 != 7 {
			t.Fatalf("median = %v, want 7", median)
		}
		// 2 of 3 assigned attempted.
		if !participation.Valid || math.Abs(participation.Float64-2.0/3.0) > 1e-9 {
			t.Fatalf("participation = %v, want 0.6667", participation)
		}
		// 10/10 -> top bucket 9; 4/10 -> bucket 4.
		var buckets []int
		if err := json.Unmarshal(distribution, &buckets); err != nil {
			t.Fatalf("decode distribution: %v", err)
		}
		if len(buckets) != 10 || buckets[9] != 1 || buckets[4] != 1 {
			t.Fatalf("distribution = %v, want bucket[4]=1 bucket[9]=1", buckets)
		}
	})

	t.Run("the empty quiz row is null-scored with zero participation", func(t *testing.T) {
		var mean, median, participation sql.NullFloat64
		var distribution []byte
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT distribution, mean, median, participation FROM quiz_stats WHERE quiz_id = $1`,
			emptyQuiz).Scan(&distribution, &mean, &median, &participation); err != nil {
			t.Fatalf("read quiz_stats: %v", err)
		}
		if mean.Valid || median.Valid {
			t.Fatalf("empty quiz mean/median = %v/%v, want null/null", mean, median)
		}
		if !participation.Valid || participation.Float64 != 0 {
			t.Fatalf("empty quiz participation = %v, want 0", participation)
		}
		var buckets []int
		if err := json.Unmarshal(distribution, &buckets); err != nil {
			t.Fatalf("decode distribution: %v", err)
		}
		for _, c := range buckets {
			if c != 0 {
				t.Fatalf("empty quiz distribution = %v, want all zero", buckets)
			}
		}
	})

	t.Run("rerunning the rollup writes nothing new", func(t *testing.T) {
		rolled, err := analytics.RollupDue(ctx, sqlDB)
		if err != nil || rolled != 0 {
			t.Fatalf("re-rollup = (%d, %v), want (0, nil)", rolled, err)
		}
	})

	t.Run("the ungraded quiz rolls up once its grading lands", func(t *testing.T) {
		if graded, err := attempt.GradeSubmitted(ctx, sqlDB); err != nil || graded != 1 {
			t.Fatalf("grade ungraded = (%d, %v), want (1, nil)", graded, err)
		}
		rolled, err := analytics.RollupDue(ctx, sqlDB)
		if err != nil || rolled != 1 {
			t.Fatalf("rollup after grade = (%d, %v), want (1, nil)", rolled, err)
		}
		if !hasStats(ungradedQuiz) {
			t.Fatalf("ungraded quiz still has no row after grading + rollup")
		}
	})
}
