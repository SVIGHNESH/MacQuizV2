package analytics_test

import (
	"context"
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

// TestAnalyticsListsE2E pins the list-shaped analytics reads behind the
// admin and teacher analytics tabs: GET /analytics/teachers and
// /analytics/students (admin-only, one row per account) and GET
// /analytics/teachers/{id}/students (the teacher-scoped student-performance
// roster). It seeds two teachers and three students, runs real attempts
// through grading and the rollup, and asserts rows, scores, scoping, and
// every role gate.
//
// It runs in its own database (macquiz_analyticsliststest) - see
// itest.FreshDatabase.
func TestAnalyticsListsE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_analyticsliststest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	authHandler := authusers.NewHandler(authSvc, false)
	router := httpserver.New(httpserver.BuildInfo{Version: "test"}, httpserver.Deps{
		DB:        sqlDB,
		Auth:      authHandler,
		Quiz:      quiz.NewHandler(quiz.NewService(sqlDB, log, quiz.LocalImportStorage{Dir: t.TempDir()}), authSvc),
		Attempt:   attempt.NewHandler(attempt.NewService(sqlDB, log), authSvc),
		Analytics: analytics.NewHandler(analytics.NewService(sqlDB, log), authSvc),
	})
	server := httptest.NewServer(router)
	defer server.Close()

	if err := authSvc.EnsureBootstrapAdmin(ctx, "admin@school.test", "admin-password-1", "Root Admin"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	provision(t, ctx, sqlDB, "teacher", "owner@school.test")
	provision(t, ctx, sqlDB, "teacher", "other@school.test")
	provision(t, ctx, sqlDB, "student", "scholar@school.test")
	provision(t, ctx, sqlDB, "student", "learner@school.test")
	provision(t, ctx, sqlDB, "student", "idle@school.test")

	admin := login(t, server, "admin@school.test", "admin-password-1")
	owner := login(t, server, "owner@school.test", "account-password")
	other := login(t, server, "other@school.test", "account-password")
	scholar := login(t, server, "scholar@school.test", "account-password")
	learner := login(t, server, "learner@school.test", "account-password")

	ownerID := userID(t, ctx, sqlDB, "owner@school.test")
	scholarID := userID(t, ctx, sqlDB, "scholar@school.test")
	learnerID := userID(t, ctx, sqlDB, "learner@school.test")

	// buildQuiz mirrors the rollup flow test: a single (4 pts, correct "b")
	// plus a truefalse (6 pts, correct true) - max 10, so raw scores read as
	// percentages - assigned, published, and backdated live.
	buildQuiz := func(title string, studentIDs ...string) (quizID, singleID, tfID string) {
		t.Helper()
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
			map[string]string{"title": title}, owner)
		if status != 201 {
			t.Fatalf("create quiz = %d %v", status, body)
		}
		quizID = body["quiz"].(map[string]any)["id"].(string)

		addQuestion := func(q map[string]any) string {
			status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", q, owner)
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

		if status, _, _ := itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
			map[string]any{"student_ids": studentIDs}, owner); status != 200 {
			t.Fatalf("assign = %d", status)
		}
		if status, _, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", map[string]any{
			"starts_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"ends_at":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
			"duration_sec": 600,
		}, owner); status != 200 {
			t.Fatalf("publish = %d", status)
		}
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET starts_at = now() - interval '1 minute' WHERE id = $1`, quizID); err != nil {
			t.Fatalf("backdate starts_at: %v", err)
		}
		return quizID, singleID, tfID
	}

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

	// Algebra: scholar aces (10/10), learner takes 4/10 and earns two
	// guardrail violations. Biology: scholar only, aced. idle never attempts
	// anything and belongs to no assignment.
	algebra, aSingle, aTF := buildQuiz("Algebra", scholarID, learnerID)
	attemptQuiz(scholar, algebra, map[string]any{aSingle: "b", aTF: true})
	learnerAttempt := attemptQuiz(learner, algebra, map[string]any{aSingle: "b", aTF: false})
	biology, bSingle, bTF := buildQuiz("Biology", scholarID)
	attemptQuiz(scholar, biology, map[string]any{bSingle: "b", bTF: true})

	if graded, err := attempt.GradeSubmitted(ctx, sqlDB); err != nil || graded != 3 {
		t.Fatalf("grade = (%d, %v), want (3, nil)", graded, err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE attempts SET violation_count = 2 WHERE id = $1`, learnerAttempt); err != nil {
		t.Fatalf("stamp violations: %v", err)
	}
	for _, id := range []string{algebra, biology} {
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET status = 'closed'::quiz_status, ends_at = now() WHERE id = $1`, id); err != nil {
			t.Fatalf("close quiz: %v", err)
		}
	}
	if rolled, err := analytics.RollupDue(ctx, sqlDB); err != nil || rolled != 2 {
		t.Fatalf("rollup = (%d, %v), want (2, nil)", rolled, err)
	}

	// A cohort for the group_ids filter column: scholar only.
	status, body, _ := itest.Call(t, server, "POST", "/api/v1/groups",
		map[string]string{"name": "Cohort A"}, admin)
	if status != 201 {
		t.Fatalf("create group = %d %v", status, body)
	}
	groupID := body["group"].(map[string]any)["id"].(string)
	if status, _, _ := itest.Call(t, server, "PUT", "/api/v1/groups/"+groupID+"/members",
		map[string]any{"student_ids": []string{scholarID}}, admin); status != 200 {
		t.Fatalf("set members = %d", status)
	}

	approx := func(got any, want float64) bool {
		f, ok := got.(float64)
		return ok && math.Abs(f-want) < 0.001
	}

	t.Run("teacher list is admin-only and mirrors per-teacher stats", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/analytics/teachers", nil, owner)
		if status != 403 {
			t.Fatalf("teacher GET /analytics/teachers = %d %v, want 403", status, body)
		}

		status, body, _ = itest.Call(t, server, "GET", "/api/v1/analytics/teachers", nil, admin)
		if status != 200 {
			t.Fatalf("admin GET /analytics/teachers = %d %v, want 200", status, body)
		}
		teachers := body["teachers"].([]any)
		if len(teachers) != 2 {
			t.Fatalf("teachers = %d rows, want 2", len(teachers))
		}
		idle := teachers[0].(map[string]any) // other@ sorts before owner@
		busy := teachers[1].(map[string]any)
		if idle["email"] != "other@school.test" || idle["quizzes_created"] != float64(0) ||
			idle["avg_participation"] != nil {
			t.Fatalf("idle teacher row = %v, want zero counts and null averages", idle)
		}
		if busy["email"] != "owner@school.test" ||
			busy["quizzes_created"] != float64(2) || busy["quizzes_conducted"] != float64(2) ||
			busy["total_attempts"] != float64(3) {
			t.Fatalf("busy teacher row = %v, want 2 created / 2 conducted / 3 attempts", busy)
		}
		// Both quizzes had every assignee attempt -> participation 1.0; class
		// score = mean of quiz means: Algebra (10+4)/2 = 7, Biology 10 -> 8.5.
		if !approx(busy["avg_participation"], 1) || !approx(busy["avg_class_score"], 8.5) {
			t.Fatalf("busy teacher averages = %v/%v, want 1.0/8.5",
				busy["avg_participation"], busy["avg_class_score"])
		}
	})

	t.Run("student list is admin-only and summarizes the rollup", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/analytics/students", nil, scholar)
		if status != 403 {
			t.Fatalf("student GET /analytics/students = %d %v, want 403", status, body)
		}

		status, body, _ = itest.Call(t, server, "GET", "/api/v1/analytics/students", nil, admin)
		if status != 200 {
			t.Fatalf("admin GET /analytics/students = %d %v, want 200", status, body)
		}
		students := body["students"].([]any)
		if len(students) != 3 {
			t.Fatalf("students = %d rows, want 3", len(students))
		}
		byEmail := map[string]map[string]any{}
		for _, s := range students {
			row := s.(map[string]any)
			byEmail[row["email"].(string)] = row
		}
		idle := byEmail["idle@school.test"]
		if idle["quizzes_taken"] != float64(0) || idle["avg_accuracy"] != nil || idle["updated_at"] != nil {
			t.Fatalf("idle student row = %v, want zero quizzes and null averages", idle)
		}
		sch := byEmail["scholar@school.test"]
		if sch["quizzes_taken"] != float64(2) || !approx(sch["avg_accuracy"], 1) {
			t.Fatalf("scholar row = %v, want 2 quizzes at accuracy 1.0", sch)
		}
		groups := sch["group_ids"].([]any)
		if len(groups) != 1 || groups[0] != groupID {
			t.Fatalf("scholar group_ids = %v, want [%s]", groups, groupID)
		}
		lrn := byEmail["learner@school.test"]
		if lrn["quizzes_taken"] != float64(1) || !approx(lrn["avg_accuracy"], 0.4) ||
			len(lrn["group_ids"].([]any)) != 0 {
			t.Fatalf("learner row = %v, want 1 quiz at accuracy 0.4 and no cohorts", lrn)
		}
	})

	t.Run("teacher-students roster is scoped to the owner's quizzes", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET",
			"/api/v1/analytics/teachers/"+ownerID+"/students", nil, owner)
		if status != 200 {
			t.Fatalf("owner roster = %d %v, want 200", status, body)
		}
		students := body["students"].([]any)
		if len(students) != 2 {
			t.Fatalf("roster = %d rows, want 2 (idle was never assigned)", len(students))
		}
		lrn := students[0].(map[string]any) // learner@ sorts before scholar@
		sch := students[1].(map[string]any)
		if lrn["email"] != "learner@school.test" ||
			lrn["assigned_quizzes"] != float64(1) || lrn["completed_quizzes"] != float64(1) ||
			!approx(lrn["avg_score_percent"], 40) || lrn["total_violations"] != float64(2) {
			t.Fatalf("learner roster row = %v, want 1/1 at 40%% with 2 violations", lrn)
		}
		if sch["email"] != "scholar@school.test" ||
			sch["assigned_quizzes"] != float64(2) || sch["completed_quizzes"] != float64(2) ||
			!approx(sch["avg_score_percent"], 100) || sch["total_violations"] != float64(0) {
			t.Fatalf("scholar roster row = %v, want 2/2 at 100%%", sch)
		}
		quizzes := sch["quizzes"].([]any)
		if len(quizzes) != 2 {
			t.Fatalf("scholar breakdown = %d quizzes, want 2", len(quizzes))
		}
		first := quizzes[0].(map[string]any) // title order: Algebra, Biology
		if first["title"] != "Algebra" || first["status"] != "closed" ||
			!approx(first["score_percent"], 100) || first["submitted_at"] == nil {
			t.Fatalf("scholar Algebra entry = %v, want closed at 100%%", first)
		}

		// A teacher with no quizzes owns an empty roster, not an error.
		status, body, _ = itest.Call(t, server, "GET",
			"/api/v1/analytics/teachers/"+userID(t, ctx, sqlDB, "other@school.test")+"/students", nil, other)
		if status != 200 || len(body["students"].([]any)) != 0 {
			t.Fatalf("idle teacher roster = %d %v, want 200 with empty list", status, body)
		}
	})

	t.Run("roster gates: stranger 404, student 403, admin 200", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET",
			"/api/v1/analytics/teachers/"+ownerID+"/students", nil, other)
		if status != 404 {
			t.Fatalf("stranger teacher roster = %d %v, want 404", status, body)
		}
		status, body, _ = itest.Call(t, server, "GET",
			"/api/v1/analytics/teachers/"+ownerID+"/students", nil, scholar)
		if status != 403 {
			t.Fatalf("student roster = %d %v, want 403", status, body)
		}
		status, body, _ = itest.Call(t, server, "GET",
			"/api/v1/analytics/teachers/"+ownerID+"/students", nil, admin)
		if status != 200 || len(body["students"].([]any)) != 2 {
			t.Fatalf("admin roster = %d %v, want 200 with 2 rows", status, body)
		}
		// An admin aiming at a student id (not a teacher) reads 404.
		status, body, _ = itest.Call(t, server, "GET",
			"/api/v1/analytics/teachers/"+learnerID+"/students", nil, admin)
		if status != 404 {
			t.Fatalf("roster for a student id = %d %v, want 404", status, body)
		}
	})
}
