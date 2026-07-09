package quiz_test

import (
	"context"
	"database/sql"
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

// TestLifecycleFlowE2E drives the Milestone 3 publish path over real HTTP
// and a real Postgres: a teacher assigns an audience (direct ids plus a
// group, expanded to rows), publish preconditions reject incomplete quizzes,
// publish snapshots the question set with its answer key into an immutable
// version, republish bumps the version, the assigned student sees the quiz
// in GET /quizzes/assigned with a lazily derived live status, and the
// unassigned student sees nothing.
//
// It runs in its own database (macquiz_lifecycletest) - see itest.FreshDatabase.
func TestLifecycleFlowE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_lifecycletest")
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
	provision(t, ctx, sqlDB, "student", "direct@school.test")
	provision(t, ctx, sqlDB, "student", "grouped@school.test")
	provision(t, ctx, sqlDB, "student", "outsider@school.test")

	teacher := login(t, server, "owner@school.test", "account-password")
	direct := login(t, server, "direct@school.test", "account-password")
	outsider := login(t, server, "outsider@school.test", "account-password")

	directID := userID(t, ctx, sqlDB, "direct@school.test")
	groupedID := userID(t, ctx, sqlDB, "grouped@school.test")

	// A group holding the second student, to prove group expansion.
	var groupID string
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO groups (name, created_by)
		 VALUES ('Class 10-B', (SELECT id FROM users WHERE role = 'admin')) RETURNING id`).Scan(&groupID); err != nil {
		t.Fatalf("create group: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO group_members (group_id, student_id) VALUES ($1, $2)`, groupID, groupedID); err != nil {
		t.Fatalf("add group member: %v", err)
	}

	// The quiz under test, with one question.
	var quizID string
	status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
		map[string]string{"title": "Trig Checkpoint"}, teacher)
	if status != 201 {
		t.Fatalf("create quiz = %d %v", status, body)
	}
	quizID = body["quiz"].(map[string]any)["id"].(string)
	status, _, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", map[string]any{
		"type": "single", "body": map[string]string{"text": "sin(90°)?"},
		"options": []map[string]string{{"key": "a", "text": "0"}, {"key": "b", "text": "1"}},
		"correct": "b",
	}, teacher)
	if status != 201 {
		t.Fatalf("add question = %d", status)
	}

	window := map[string]any{
		"starts_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"ends_at":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		"duration_sec": 600,
	}

	t.Run("publish requires an audience", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", window, teacher)
		if status != 422 {
			t.Fatalf("publish without audience = %d %v, want 422", status, body)
		}
		fields := body["fields"].(map[string]any)
		if fields["assignments"] == nil {
			t.Fatalf("want an assignments field error, got %v", fields)
		}
	})

	t.Run("assignments expand groups to rows", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
			map[string]any{"student_ids": []string{directID}, "group_ids": []string{groupID}}, teacher)
		if status != 200 {
			t.Fatalf("set assignments = %d %v", status, body)
		}
		if got := len(body["students"].([]any)); got != 2 {
			t.Fatalf("audience size = %d, want 2 (direct + group-expanded)", got)
		}

		// Removing the student from the group must NOT revoke the assignment
		// (docs/03: expansion happens at assignment time).
		if _, err := sqlDB.ExecContext(ctx,
			`DELETE FROM group_members WHERE group_id = $1`, groupID); err != nil {
			t.Fatalf("empty group: %v", err)
		}
		status, body, _ = itest.Call(t, server, "GET", "/api/v1/quizzes/"+quizID+"/assignments", nil, teacher)
		if status != 200 || len(body["students"].([]any)) != 2 {
			t.Fatalf("audience after group edit = %d %v, want the same 2 students", status, body)
		}
	})

	t.Run("assignments reject non-student ids", func(t *testing.T) {
		teacherID := userID(t, ctx, sqlDB, "owner@school.test")
		status, body, _ := itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
			map[string]any{"student_ids": []string{teacherID}}, teacher)
		if status != 422 {
			t.Fatalf("assign a teacher = %d %v, want 422", status, body)
		}
	})

	t.Run("window validation", func(t *testing.T) {
		bad := map[string]any{
			"starts_at":    time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
			"ends_at":      time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
			"duration_sec": 5,
		}
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", bad, teacher)
		if status != 422 {
			t.Fatalf("publish with bad window = %d %v, want 422", status, body)
		}
		fields := body["fields"].(map[string]any)
		for _, f := range []string{"starts_at", "ends_at", "duration_sec"} {
			if fields[f] == nil {
				t.Errorf("want a %s field error, got %v", f, fields)
			}
		}
	})

	t.Run("publish snapshots and schedules", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", window, teacher)
		if status != 200 {
			t.Fatalf("publish = %d %v", status, body)
		}
		q := body["quiz"].(map[string]any)
		if q["status"] != "scheduled" || q["version"].(float64) != 1 {
			t.Fatalf("published quiz = %v, want scheduled v1", q)
		}

		// The snapshot row exists, is version 1, and carries the answer key.
		var correct string
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT questions->0->>'correct' FROM quiz_versions WHERE quiz_id = $1 AND version = 1`,
			quizID).Scan(&correct); err != nil {
			t.Fatalf("read snapshot: %v", err)
		}
		if correct != "b" {
			t.Fatalf("snapshot answer key = %q, want %q", correct, "b")
		}

		// Snapshots are immutable: UPDATE and DELETE are rejected by trigger.
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quiz_versions SET guardrails = '{}' WHERE quiz_id = $1`, quizID); err == nil {
			t.Fatal("snapshot UPDATE succeeded, want append-only rejection")
		}

		// Question edits are locked now.
		status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", map[string]any{
			"type": "truefalse", "body": map[string]string{"text": "Too late?"}, "correct": true,
		}, teacher)
		if status != 409 || body["code"] != "QUIZ_NOT_EDITABLE" {
			t.Fatalf("edit scheduled quiz = %d %v, want 409 QUIZ_NOT_EDITABLE", status, body)
		}
	})

	t.Run("republish while scheduled bumps the version", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", window, teacher)
		if status != 200 {
			t.Fatalf("republish = %d %v", status, body)
		}
		if v := body["quiz"].(map[string]any)["version"].(float64); v != 2 {
			t.Fatalf("republished version = %v, want 2", v)
		}
		var snapshots int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM quiz_versions WHERE quiz_id = $1`, quizID).Scan(&snapshots); err != nil || snapshots != 2 {
			t.Fatalf("snapshot count = %d (err %v), want 2", snapshots, err)
		}
	})

	t.Run("assigned student sees the quiz, unassigned does not", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/quizzes/assigned", nil, direct)
		if status != 200 {
			t.Fatalf("assigned list = %d %v", status, body)
		}
		quizzes := body["quizzes"].([]any)
		if len(quizzes) != 1 {
			t.Fatalf("assigned quizzes = %d, want 1", len(quizzes))
		}
		q := quizzes[0].(map[string]any)
		if q["status"] != "scheduled" || q["question_count"].(float64) != 1 {
			t.Fatalf("assigned quiz = %v, want scheduled with 1 question", q)
		}
		if _, leaked := q["owner_id"]; leaked {
			t.Fatalf("student view leaks owner_id: %v", q)
		}

		status, body, _ = itest.Call(t, server, "GET", "/api/v1/quizzes/assigned", nil, outsider)
		if status != 200 || len(body["quizzes"].([]any)) != 0 {
			t.Fatalf("outsider list = %d %v, want empty", status, body)
		}

		// The list is student-only; the teacher's role gate answers 403.
		status, _, _ = itest.Call(t, server, "GET", "/api/v1/quizzes/assigned", nil, teacher)
		if status != 403 {
			t.Fatalf("teacher GET /quizzes/assigned = %d, want 403", status)
		}
	})

	t.Run("passed starts_at reads live without a scheduler", func(t *testing.T) {
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET starts_at = now() - interval '1 minute' WHERE id = $1`, quizID); err != nil {
			t.Fatalf("backdate starts_at: %v", err)
		}
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/quizzes/assigned", nil, direct)
		if status != 200 {
			t.Fatalf("assigned list = %d %v", status, body)
		}
		if got := body["quizzes"].([]any)[0].(map[string]any)["status"]; got != "live" {
			t.Fatalf("derived status = %v, want live", got)
		}
		// The owner's list derives the same state.
		status, body, _ = itest.Call(t, server, "GET", "/api/v1/quizzes", nil, teacher)
		if status != 200 || body["quizzes"].([]any)[0].(map[string]any)["status"] != "live" {
			t.Fatalf("teacher list status = %d %v, want live", status, body)
		}
	})

	t.Run("live quiz audience allows late invite but publish stays refused", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
			map[string]any{"student_ids": []string{directID}}, teacher)
		// The stored status is still 'scheduled' until the scheduler flips
		// it, so the write is still allowed; flip it and retry.
		if status != 200 {
			t.Fatalf("assignment while stored-scheduled = %d %v, want 200", status, body)
		}
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET status = 'live' WHERE id = $1`, quizID); err != nil {
			t.Fatalf("flip to live: %v", err)
		}
		// Adding a student while live is a late invite (docs/06 section 1):
		// nobody has an in-progress attempt yet in this authoring-only test,
		// so the audience edit succeeds even though the quiz is live.
		status, body, _ = itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
			map[string]any{"student_ids": []string{directID, groupedID}}, teacher)
		if status != 200 {
			t.Fatalf("late invite while live = %d %v, want 200", status, body)
		}
		if got := len(body["students"].([]any)); got != 2 {
			t.Fatalf("audience after late invite while live = %d, want 2", got)
		}
		// Publish itself is still a draft/scheduled-only affordance.
		status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", window, teacher)
		if status != 409 {
			t.Fatalf("publish while live = %d %v, want 409", status, body)
		}
	})

	t.Run("lifecycle mutations left audit rows", func(t *testing.T) {
		want := map[string]int{
			"quizzes.published":       2,
			"quizzes.assignments_set": 3,
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

func userID(t *testing.T, ctx context.Context, sqlDB *sql.DB, email string) string {
	t.Helper()
	var id string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT id FROM users WHERE email = $1`, email).Scan(&id); err != nil {
		t.Fatalf("resolve %s: %v", email, err)
	}
	return id
}
