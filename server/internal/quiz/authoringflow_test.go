// The authoring-flow integration test lives in an external test package so
// it can drive the real httpserver router (which imports quiz) without a
// cycle.
package quiz_test

import (
	"context"
	"database/sql"
	"fmt"
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

// TestAuthoringFlowE2E drives Milestone 2's exit criteria over real HTTP and
// a real Postgres: a teacher creates a draft quiz with questions of all four
// types, edits, reorders, and deletes them; validation rejects the docs/07
// rule breakers; non-owners get 404s; published quizzes refuse edits; and
// every mutation leaves its audit row.
//
// It runs in its own database (macquiz_quiztest) - see itest.FreshDatabase.
func TestAuthoringFlowE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_quiztest")
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
	provision(t, ctx, sqlDB, "teacher", "teacher1@school.test")
	provision(t, ctx, sqlDB, "teacher", "teacher2@school.test")
	provision(t, ctx, sqlDB, "student", "student@school.test")

	admin := login(t, server, "admin@school.test", "admin-password-1")
	teacher1 := login(t, server, "teacher1@school.test", "account-password")
	teacher2 := login(t, server, "teacher2@school.test", "account-password")
	student := login(t, server, "student@school.test", "account-password")

	t.Run("authoring is teacher-only", func(t *testing.T) {
		for name, cookies := range map[string]map[string]string{"admin": admin, "student": student} {
			status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
				map[string]string{"title": "Nope"}, cookies)
			if status != 403 || body["code"] != "FORBIDDEN" {
				t.Fatalf("%s POST /quizzes = %d %v, want 403 FORBIDDEN", name, status, body)
			}
		}
	})

	var quizID string
	t.Run("draft quiz crud", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
			map[string]string{"title": " "}, teacher1)
		if status != 422 {
			t.Fatalf("blank title = %d %v, want 422", status, body)
		}

		status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes",
			map[string]string{"title": "Fractions Unit Test"}, teacher1)
		if status != 201 {
			t.Fatalf("create quiz = %d %v, want 201", status, body)
		}
		q := body["quiz"].(map[string]any)
		quizID = q["id"].(string)
		if q["status"] != "draft" || q["version"] != float64(0) {
			t.Fatalf("new quiz = %v, want draft version 0", q)
		}

		status, body, _ = itest.Call(t, server, "GET", "/api/v1/quizzes", nil, teacher1)
		if status != 200 || len(body["quizzes"].([]any)) != 1 {
			t.Fatalf("list quizzes = %d %v, want 1 quiz", status, body)
		}

		status, body, _ = itest.Call(t, server, "PATCH", "/api/v1/quizzes/"+quizID,
			map[string]any{"title": "Fractions Final", "shuffle_questions": true}, teacher1)
		if status != 200 || body["quiz"].(map[string]any)["title"] != "Fractions Final" {
			t.Fatalf("patch quiz = %d %v, want retitled quiz", status, body)
		}

		status, body, _ = itest.Call(t, server, "PATCH", "/api/v1/quizzes/"+quizID,
			map[string]any{"max_attempts": 0}, teacher1)
		if status != 422 {
			t.Fatalf("max_attempts 0 = %d %v, want 422", status, body)
		}
	})

	questionIDs := make([]string, 0, 4)
	t.Run("question crud with validation", func(t *testing.T) {
		valid := []map[string]any{
			{"type": "single", "body": map[string]any{"text": "1/2 + 1/4 = ?"},
				"options": []map[string]string{{"key": "a", "text": "3/4"}, {"key": "b", "text": "2/6"}},
				"correct": "a", "points": 2},
			{"type": "multi", "body": map[string]any{"text": "Which equal 1/2?"},
				"options": []map[string]string{{"key": "a", "text": "2/4"}, {"key": "b", "text": "3/5"}, {"key": "c", "text": "4/8"}},
				"correct": []string{"a", "c"}},
			{"type": "truefalse", "body": map[string]any{"text": "1/3 is larger than 1/2"},
				"correct": false},
			{"type": "short", "body": map[string]any{"text": "Simplify 6/8"},
				"correct": map[string]any{"accepted": []string{"3/4", "0.75"}}},
		}
		for i, in := range valid {
			status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", in, teacher1)
			if status != 201 {
				t.Fatalf("add question %d = %d %v, want 201", i, status, body)
			}
			q := body["question"].(map[string]any)
			if q["position"] != float64(i+1) {
				t.Fatalf("question %d position = %v, want %d", i, q["position"], i+1)
			}
			questionIDs = append(questionIDs, q["id"].(string))
		}

		invalid := []struct {
			name  string
			in    map[string]any
			field string
		}{
			{"correct not among options", map[string]any{
				"type": "single", "body": map[string]any{"text": "x"},
				"options": []map[string]string{{"key": "a", "text": "1"}, {"key": "b", "text": "2"}},
				"correct": "z"}, "correct"},
			{"one option", map[string]any{
				"type": "single", "body": map[string]any{"text": "x"},
				"options": []map[string]string{{"key": "a", "text": "only"}},
				"correct": "a"}, "options"},
			{"zero points", map[string]any{
				"type": "truefalse", "body": map[string]any{"text": "x"},
				"correct": true, "points": 0}, "points"},
		}
		for _, tc := range invalid {
			status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", tc.in, teacher1)
			if status != 422 || body["code"] != "VALIDATION_FAILED" {
				t.Fatalf("%s = %d %v, want 422 VALIDATION_FAILED", tc.name, status, body)
			}
			if _, ok := body["fields"].(map[string]any)[tc.field]; !ok {
				t.Fatalf("%s fields = %v, want error on %q", tc.name, body["fields"], tc.field)
			}
		}

		status, body, _ := itest.Call(t, server, "PATCH", "/api/v1/questions/"+questionIDs[2],
			map[string]any{"type": "truefalse", "body": map[string]any{"text": "1/3 is smaller than 1/2"},
				"correct": true, "points": 3}, teacher1)
		if status != 200 || body["question"].(map[string]any)["points"] != float64(3) {
			t.Fatalf("patch question = %d %v, want 200 with points 3", status, body)
		}

		status, body, _ = itest.Call(t, server, "GET", "/api/v1/quizzes/"+quizID, nil, teacher1)
		if status != 200 {
			t.Fatalf("get quiz = %d %v, want 200", status, body)
		}
		questions := body["questions"].([]any)
		if len(questions) != 4 {
			t.Fatalf("question count = %d, want 4", len(questions))
		}
		// The owner-facing view carries the answer key.
		if questions[0].(map[string]any)["correct"] != "a" {
			t.Fatalf("owner view question 1 = %v, want correct \"a\"", questions[0])
		}
	})

	t.Run("non-owners get 404, never 403", func(t *testing.T) {
		paths := map[string]string{
			"GET quiz":        "GET /api/v1/quizzes/" + quizID,
			"PATCH question":  "PATCH /api/v1/questions/" + questionIDs[0],
			"DELETE quiz":     "DELETE /api/v1/quizzes/" + quizID,
			"POST question":   "POST /api/v1/quizzes/" + quizID + "/questions",
			"PUT order":       "PUT /api/v1/quizzes/" + quizID + "/questions/order",
			"garbage quiz id": "GET /api/v1/quizzes/not-a-uuid",
		}
		bodies := map[string]any{
			"PATCH question": map[string]any{"type": "truefalse",
				"body": map[string]any{"text": "x"}, "correct": true},
			"POST question": map[string]any{"type": "truefalse",
				"body": map[string]any{"text": "x"}, "correct": true},
			"PUT order": map[string]any{"question_ids": questionIDs},
		}
		for name, methodPath := range paths {
			var method, path string
			_, _ = fmt.Sscanf(methodPath, "%s %s", &method, &path)
			status, body, _ := itest.Call(t, server, method, path, bodies[name], teacher2)
			if status != 404 || body["code"] != "NOT_FOUND" {
				t.Fatalf("teacher2 %s = %d %v, want 404 NOT_FOUND", name, status, body)
			}
		}
	})

	t.Run("reorder rewrites dense positions", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/questions/order",
			map[string]any{"question_ids": questionIDs[:3]}, teacher1)
		if status != 422 {
			t.Fatalf("partial reorder = %d %v, want 422", status, body)
		}

		reversed := []string{questionIDs[3], questionIDs[2], questionIDs[1], questionIDs[0]}
		status, body, _ = itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/questions/order",
			map[string]any{"question_ids": reversed}, teacher1)
		if status != 200 {
			t.Fatalf("reorder = %d %v, want 200", status, body)
		}
		questions := body["questions"].([]any)
		for i, want := range reversed {
			q := questions[i].(map[string]any)
			if q["id"] != want || q["position"] != float64(i+1) {
				t.Fatalf("after reorder questions[%d] = %v, want id %s at position %d", i, q, want, i+1)
			}
		}
	})

	t.Run("delete question re-densifies positions", func(t *testing.T) {
		status, _, _ := itest.Call(t, server, "DELETE", "/api/v1/questions/"+questionIDs[2], nil, teacher1)
		if status != 204 {
			t.Fatalf("delete question = %d, want 204", status)
		}
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/quizzes/"+quizID, nil, teacher1)
		if status != 200 {
			t.Fatalf("get quiz = %d, want 200", status)
		}
		questions := body["questions"].([]any)
		if len(questions) != 3 {
			t.Fatalf("question count after delete = %d, want 3", len(questions))
		}
		for i, q := range questions {
			if q.(map[string]any)["position"] != float64(i+1) {
				t.Fatalf("positions not dense after delete: %v", questions)
			}
		}
	})

	t.Run("published quizzes refuse edits", func(t *testing.T) {
		// Publishing is Milestone 3; flip the status directly to prove the
		// draft gate ahead of it.
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET status = 'scheduled' WHERE id = $1`, quizID); err != nil {
			t.Fatalf("force status: %v", err)
		}
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions",
			map[string]any{"type": "truefalse", "body": map[string]any{"text": "x"}, "correct": true}, teacher1)
		if status != 409 || body["code"] != "QUIZ_NOT_EDITABLE" {
			t.Fatalf("add question to scheduled quiz = %d %v, want 409 QUIZ_NOT_EDITABLE", status, body)
		}
		status, body, _ = itest.Call(t, server, "DELETE", "/api/v1/quizzes/"+quizID, nil, teacher1)
		if status != 409 {
			t.Fatalf("delete scheduled quiz = %d %v, want 409", status, body)
		}
		// Reading still works: the owner sees their scheduled quiz.
		status, _, _ = itest.Call(t, server, "GET", "/api/v1/quizzes/"+quizID, nil, teacher1)
		if status != 200 {
			t.Fatalf("get scheduled quiz = %d, want 200", status)
		}
	})

	t.Run("draft quiz delete cascades", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
			map[string]string{"title": "Scratch"}, teacher1)
		if status != 201 {
			t.Fatalf("create scratch quiz = %d %v, want 201", status, body)
		}
		scratchID := body["quiz"].(map[string]any)["id"].(string)
		status, _, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+scratchID+"/questions",
			map[string]any{"type": "truefalse", "body": map[string]any{"text": "x"}, "correct": true}, teacher1)
		if status != 201 {
			t.Fatalf("add scratch question = %d, want 201", status)
		}
		status, _, _ = itest.Call(t, server, "DELETE", "/api/v1/quizzes/"+scratchID, nil, teacher1)
		if status != 204 {
			t.Fatalf("delete scratch quiz = %d, want 204", status)
		}
		status, _, _ = itest.Call(t, server, "GET", "/api/v1/quizzes/"+scratchID, nil, teacher1)
		if status != 404 {
			t.Fatalf("get deleted quiz = %d, want 404", status)
		}
		var orphans int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM questions WHERE quiz_id = $1`, scratchID).Scan(&orphans); err != nil || orphans != 0 {
			t.Fatalf("orphaned questions = %d (err %v), want 0", orphans, err)
		}
	})

	t.Run("every mutation left an audit row", func(t *testing.T) {
		want := map[string]int{
			"quizzes.created":     2,
			"quizzes.updated":     1,
			"quizzes.deleted":     1,
			"questions.created":   5,
			"questions.updated":   1,
			"questions.deleted":   1,
			"questions.reordered": 1,
		}
		for action, count := range want {
			var got int
			if err := sqlDB.QueryRowContext(ctx,
				`SELECT count(*) FROM audit_log WHERE action = $1`, action).Scan(&got); err != nil {
				t.Fatalf("count %s: %v", action, err)
			}
			if got != count {
				t.Fatalf("audit rows for %s = %d, want %d", action, got, count)
			}
		}
	})
}

// provision inserts an account with a known password and no forced reset,
// standing in for the POST /users + password-change dance so the test stays
// inside the login rate-limit budget.
func provision(t *testing.T, ctx context.Context, sqlDB *sql.DB, role, email string) {
	t.Helper()
	hash, err := authusers.HashPassword("account-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO users (role, email, password_hash, full_name, created_by, must_change_password)
		 VALUES ($1, $2, $3, $4, (SELECT id FROM users WHERE role = 'admin'), false)`,
		role, email, hash, email); err != nil {
		t.Fatalf("provision %s: %v", email, err)
	}
}

func login(t *testing.T, server *httptest.Server, email, password string) map[string]string {
	t.Helper()
	status, body, cookies := itest.Call(t, server, "POST", "/api/v1/auth/login",
		map[string]string{"email": email, "password": password}, nil)
	if status != 200 {
		t.Fatalf("login %s = %d %v, want 200", email, status, body)
	}
	return cookies
}
