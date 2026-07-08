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
		DB:        sqlDB,
		Auth:      authusers.NewHandler(authSvc, false),
		Quiz:      quiz.NewHandler(quiz.NewService(sqlDB, log), authSvc),
		Attempt:   attempt.NewHandler(attempt.NewService(sqlDB, log), authSvc),
		Analytics: analytics.NewHandler(analytics.NewService(sqlDB, log), authSvc),
	})
	server := httptest.NewServer(router)
	defer server.Close()

	if err := authSvc.EnsureBootstrapAdmin(ctx, "admin@school.test", "admin-password-1", "Root Admin"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	provision(t, ctx, sqlDB, "teacher", "owner@school.test")
	provision(t, ctx, sqlDB, "teacher", "stranger@school.test")
	provision(t, ctx, sqlDB, "student", "scholar@school.test")
	provision(t, ctx, sqlDB, "student", "learner@school.test")
	provision(t, ctx, sqlDB, "student", "absent@school.test")

	teacher := login(t, server, "owner@school.test", "account-password")
	stranger := login(t, server, "stranger@school.test", "account-password")
	admin := login(t, server, "admin@school.test", "admin-password-1")
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

	// The graded quiz under test: scholar aces it (10/10); learner answers
	// the single right (b) but the truefalse wrong (false), so learner keeps
	// 4/10 while the truefalse question gains a right/wrong split - real
	// discrimination to measure. absent never attempts. Assigned 3,
	// attempted 2.
	gradedQuiz, gSingle, gTF := buildQuiz("Graded", "scholar@school.test", "learner@school.test", "absent@school.test")
	attemptQuiz(scholar, gradedQuiz, map[string]any{gSingle: "b", gTF: true})
	learnerAttempt := attemptQuiz(learner, gradedQuiz, map[string]any{gSingle: "b", gTF: false})

	// An archived quiz is a terminal superset of closed and must roll up too.
	archivedQuiz, aSingle, aTF := buildQuiz("Archived", "scholar@school.test")
	attemptQuiz(scholar, archivedQuiz, map[string]any{aSingle: "b", aTF: true})

	if graded, err := attempt.GradeSubmitted(ctx, sqlDB); err != nil || graded != 3 {
		t.Fatalf("grade = (%d, %v), want (3, nil)", graded, err)
	}
	// Stamp integrity events on learner's (already-graded) attempt: two
	// violations and a kick. submit_kind, not status, marks the kick - grading
	// has flipped the attempt to 'graded'. This exercises the integrity tally
	// and the flagged/kicked counters that a clean run leaves at zero.
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE attempts SET violation_count = 2, submit_kind = 'kicked' WHERE id = $1`, learnerAttempt); err != nil {
		t.Fatalf("stamp integrity: %v", err)
	}
	// Stamp learner's two answer times (1000 + 3000 ms) so avg_time_per_question
	// has a non-zero value to pin - autosave leaves time_spent_ms at its default.
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE attempt_answers SET time_spent_ms = CASE WHEN question_id = $2 THEN 1000 ELSE 3000 END
		 WHERE attempt_id = $1`, learnerAttempt, gSingle); err != nil {
		t.Fatalf("stamp answer times: %v", err)
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
		// item_analysis: one row per answered question, over each student's
		// best graded attempt. The single (both answered 'b' correctly) has a
		// perfect p-value and a null point-biserial - no right/wrong split to
		// correlate. The truefalse splits scholar-right(score 10)/learner-wrong
		// (score 4), so p-value is 0.5 and point-biserial is a perfect +1 (the
		// higher scorer got it right).
		var items []map[string]any
		if err := json.Unmarshal(itemAnalysis, &items); err != nil {
			t.Fatalf("decode item_analysis: %v", err)
		}
		byQ := map[string]map[string]any{}
		for _, it := range items {
			byQ[it["question_id"].(string)] = it
		}
		single, tf := byQ[gSingle], byQ[gTF]
		if single == nil || tf == nil {
			t.Fatalf("item_analysis missing a question: %v", items)
		}
		if single["p_value"].(float64) != 1 || single["point_biserial"] != nil {
			t.Fatalf("single item = %v, want p_value 1 point_biserial null", single)
		}
		if tf["p_value"].(float64) != 0.5 || math.Abs(tf["point_biserial"].(float64)-1) > 1e-9 {
			t.Fatalf("truefalse item = %v, want p_value 0.5 point_biserial 1", tf)
		}
		// The truefalse's option-pick tallies one 'true' and one 'false'.
		picks := tf["option_pick_rates"].(map[string]any)
		if picks["true"].(float64) != 1 || picks["false"].(float64) != 1 {
			t.Fatalf("truefalse picks = %v, want one true one false", picks)
		}

		// integrity: learner's attempt carries two violations and a kick.
		var integ map[string]any
		if err := json.Unmarshal(integrity, &integ); err != nil {
			t.Fatalf("decode integrity: %v", err)
		}
		if integ["kicked_attempts"].(float64) != 1 ||
			integ["flagged_students"].(float64) != 1 ||
			integ["total_violations"].(float64) != 2 {
			t.Fatalf("integrity = %v, want 1 kicked / 1 flagged / 2 violations", integ)
		}
		perStudent := integ["per_student"].([]any)
		if len(perStudent) != 1 {
			t.Fatalf("integrity per_student = %v, want one entry", perStudent)
		}
		flagged := perStudent[0].(map[string]any)
		if flagged["violations"].(float64) != 2 || flagged["kicked"] != true {
			t.Fatalf("flagged student = %v, want 2 violations kicked", flagged)
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
		var distribution, itemAnalysis, integrity []byte
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT distribution, mean, median, participation, item_analysis, integrity
			 FROM quiz_stats WHERE quiz_id = $1`,
			emptyQuiz).Scan(&distribution, &mean, &median, &participation, &itemAnalysis, &integrity); err != nil {
			t.Fatalf("read quiz_stats: %v", err)
		}
		// No answers and no attempts: item_analysis is an empty array (never
		// null) and integrity is present with every counter zero.
		if string(itemAnalysis) != "[]" {
			t.Fatalf("empty quiz item_analysis = %s, want []", itemAnalysis)
		}
		var integ map[string]any
		if err := json.Unmarshal(integrity, &integ); err != nil {
			t.Fatalf("decode integrity: %v", err)
		}
		if integ["kicked_attempts"].(float64) != 0 || len(integ["per_student"].([]any)) != 0 {
			t.Fatalf("empty quiz integrity = %v, want zero counters", integ)
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

	// student_stats is the cross-quiz rollup upserted for every assigned
	// student when a quiz closes. By now scholar has three terminal quizzes
	// graded (Graded, Archived, Ungraded) out of four assigned (Empty was never
	// attempted); learner has one (Graded, kicked-then-graded 4/10); absent was
	// assigned Graded but never attempted. Pending is created after this, so it
	// is not yet counted.
	readStudentStats := func(email string) (trend []map[string]any, avgTime, completion sql.NullFloat64, found bool) {
		t.Helper()
		var trendJSON []byte
		err := sqlDB.QueryRowContext(ctx,
			`SELECT accuracy_trend, avg_time_per_question, completion_rate
			 FROM student_stats WHERE student_id = $1`, userID(t, ctx, sqlDB, email)).Scan(&trendJSON, &avgTime, &completion)
		if err == sql.ErrNoRows {
			return nil, avgTime, completion, false
		}
		if err != nil {
			t.Fatalf("read student_stats for %s: %v", email, err)
		}
		if err := json.Unmarshal(trendJSON, &trend); err != nil {
			t.Fatalf("decode accuracy_trend for %s: %v", email, err)
		}
		return trend, avgTime, completion, true
	}

	t.Run("student_stats rolls up per assigned student across quizzes", func(t *testing.T) {
		// scholar: 3 of 4 terminal assigned quizzes graded -> completion 0.75,
		// a three-point accuracy trend (all perfect scores), avg answer time 0
		// (autosave left every time_spent_ms at its default).
		trend, avgTime, completion, found := readStudentStats("scholar@school.test")
		if !found {
			t.Fatalf("scholar has no student_stats row")
		}
		if len(trend) != 3 {
			t.Fatalf("scholar accuracy_trend = %v, want three entries", trend)
		}
		if !completion.Valid || math.Abs(completion.Float64-0.75) > 1e-9 {
			t.Fatalf("scholar completion_rate = %v, want 0.75", completion)
		}
		if !avgTime.Valid || avgTime.Float64 != 0 {
			t.Fatalf("scholar avg_time_per_question = %v, want 0", avgTime)
		}

		// learner: the sole assigned quiz graded -> completion 1.0, a one-point
		// trend at 0.4 (scored 4/10), avg time (1000+3000)/2 = 2000.
		trend, avgTime, completion, found = readStudentStats("learner@school.test")
		if !found {
			t.Fatalf("learner has no student_stats row")
		}
		if len(trend) != 1 || math.Abs(trend[0]["accuracy"].(float64)-0.4) > 1e-9 {
			t.Fatalf("learner accuracy_trend = %v, want one entry at 0.4", trend)
		}
		if !completion.Valid || completion.Float64 != 1 {
			t.Fatalf("learner completion_rate = %v, want 1", completion)
		}
		if !avgTime.Valid || avgTime.Float64 != 2000 {
			t.Fatalf("learner avg_time_per_question = %v, want 2000", avgTime)
		}

		// absent: assigned Graded, never attempted -> completion 0, an empty
		// trend, and a null avg time (no answers to average).
		trend, avgTime, completion, found = readStudentStats("absent@school.test")
		if !found {
			t.Fatalf("absent has no student_stats row (a no-show must still roll up)")
		}
		if len(trend) != 0 {
			t.Fatalf("absent accuracy_trend = %v, want empty", trend)
		}
		if !completion.Valid || completion.Float64 != 0 {
			t.Fatalf("absent completion_rate = %v, want 0", completion)
		}
		if avgTime.Valid {
			t.Fatalf("absent avg_time_per_question = %v, want null", avgTime)
		}
	})

	// GET /analytics/quizzes/:id serves the rolled-up row (docs/04 section 2).
	// A live quiz never rolled up gives the read endpoint a genuine no-row
	// case that is neither missing nor forbidden.
	pendingQuiz, _, _ := buildQuiz("Pending", "scholar@school.test")

	t.Run("the owner reads the graded quiz's rolled-up stats", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/analytics/quizzes/"+gradedQuiz, nil, teacher)
		if status != 200 {
			t.Fatalf("owner GET analytics = %d %v, want 200", status, body)
		}
		if body["quiz_id"] != gradedQuiz {
			t.Fatalf("quiz_id = %v, want %s", body["quiz_id"], gradedQuiz)
		}
		if body["mean"].(float64) != 7 || body["median"].(float64) != 7 {
			t.Fatalf("mean/median = %v/%v, want 7/7", body["mean"], body["median"])
		}
		if math.Abs(body["participation"].(float64)-2.0/3.0) > 1e-9 {
			t.Fatalf("participation = %v, want 0.6667", body["participation"])
		}
		// The jsonb columns pass through: item_analysis is the array and
		// integrity the object RollupDue stored.
		if len(body["item_analysis"].([]any)) != 2 {
			t.Fatalf("item_analysis = %v, want two questions", body["item_analysis"])
		}
		integ := body["integrity"].(map[string]any)
		if integ["kicked_attempts"].(float64) != 1 || integ["total_violations"].(float64) != 2 {
			t.Fatalf("integrity = %v, want 1 kicked / 2 violations", integ)
		}
	})

	t.Run("an admin may read any quiz's stats", func(t *testing.T) {
		if status, body, _ := itest.Call(t, server, "GET", "/api/v1/analytics/quizzes/"+gradedQuiz, nil, admin); status != 200 {
			t.Fatalf("admin GET analytics = %d %v, want 200", status, body)
		}
	})

	t.Run("a non-owning teacher gets 404, not the stats", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/analytics/quizzes/"+gradedQuiz, nil, stranger)
		if status != 404 {
			t.Fatalf("stranger GET analytics = %d %v, want 404", status, body)
		}
	})

	t.Run("a student is blocked at the role gate", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/analytics/quizzes/"+gradedQuiz, nil, scholar)
		if status != 403 {
			t.Fatalf("student GET analytics = %d %v, want 403", status, body)
		}
	})

	t.Run("a quiz not yet rolled up reads as 404", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/analytics/quizzes/"+pendingQuiz, nil, teacher)
		if status != 404 {
			t.Fatalf("pending GET analytics = %d %v, want 404", status, body)
		}
	})

	t.Run("a malformed id reads as 404", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/analytics/quizzes/not-a-uuid", nil, teacher)
		if status != 404 {
			t.Fatalf("garbage GET analytics = %d %v, want 404", status, body)
		}
	})

	// GET /analytics/students/:id serves the cross-quiz profile student_stats
	// holds (docs/04 section 2). Its audience differs from the quiz endpoint: a
	// student may read their own (so there is no staff gate), while a caller who
	// may not see the subject 404s rather than 403 - existence never leaks.
	scholarID := userID(t, ctx, sqlDB, "scholar@school.test")
	learnerID := userID(t, ctx, sqlDB, "learner@school.test")

	t.Run("a student reads their own profile", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/analytics/students/"+scholarID, nil, scholar)
		if status != 200 {
			t.Fatalf("scholar GET own analytics = %d %v, want 200", status, body)
		}
		if body["student_id"] != scholarID {
			t.Fatalf("student_id = %v, want %s", body["student_id"], scholarID)
		}
		// scholar: three terminal quizzes graded of four assigned -> 0.75,
		// three-point trend (all perfect), avg time 0 (see the rollup subtest).
		if math.Abs(body["completion_rate"].(float64)-0.75) > 1e-9 {
			t.Fatalf("completion_rate = %v, want 0.75", body["completion_rate"])
		}
		if len(body["accuracy_trend"].([]any)) != 3 {
			t.Fatalf("accuracy_trend = %v, want three entries", body["accuracy_trend"])
		}
	})

	t.Run("a student cannot read another student's profile - 404, not 403", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/analytics/students/"+learnerID, nil, scholar)
		if status != 404 {
			t.Fatalf("scholar GET learner analytics = %d %v, want 404", status, body)
		}
	})

	t.Run("a teacher reads an assigned student's profile", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/analytics/students/"+scholarID, nil, teacher)
		if status != 200 {
			t.Fatalf("owner GET assigned student = %d %v, want 200", status, body)
		}
		if body["student_id"] != scholarID {
			t.Fatalf("student_id = %v, want %s", body["student_id"], scholarID)
		}
	})

	t.Run("a non-owning teacher cannot read an unassigned student - 404", func(t *testing.T) {
		// stranger owns no quiz scholar is assigned to, so the assignment EXISTS
		// is false and Can denies -> the existence-safe 404.
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/analytics/students/"+scholarID, nil, stranger)
		if status != 404 {
			t.Fatalf("stranger GET unassigned student = %d %v, want 404", status, body)
		}
	})

	t.Run("an admin may read any student's profile", func(t *testing.T) {
		if status, body, _ := itest.Call(t, server, "GET", "/api/v1/analytics/students/"+scholarID, nil, admin); status != 200 {
			t.Fatalf("admin GET student analytics = %d %v, want 200", status, body)
		}
	})

	t.Run("a student with no rollup yet reads as 404", func(t *testing.T) {
		// A fresh student assigned to nothing has no student_stats row; their own
		// read is authorized but has nothing to show - the same 404 as unknown.
		provision(t, ctx, sqlDB, "student", "newcomer@school.test")
		newcomer := login(t, server, "newcomer@school.test", "account-password")
		newcomerID := userID(t, ctx, sqlDB, "newcomer@school.test")
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/analytics/students/"+newcomerID, nil, newcomer)
		if status != 404 {
			t.Fatalf("newcomer GET own analytics = %d %v, want 404", status, body)
		}
	})

	t.Run("a malformed student id reads as 404", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/analytics/students/not-a-uuid", nil, admin)
		if status != 404 {
			t.Fatalf("garbage GET student analytics = %d %v, want 404", status, body)
		}
	})
}
