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

// TestLiveAssignmentFlowE2E pins the quiz-system-design.html section 8
// "late invite" gap ThingsToDo.txt did not originally track: PUT
// /quizzes/:id/assignments now stays open once a quiz is live instead of
// answering QUIZ_NOT_EDITABLE for every audience edit. Adding a student is
// unconditional (late invite); removing one is allowed unless they have an
// in-progress attempt, in which case the whole replacement is refused
// (409 ASSIGNMENT_IN_PROGRESS) and the audience is left untouched - kick is
// the only sanctioned way to interrupt a live attempt.
//
// It runs in its own database (macquiz_liveassigntest).
func TestLiveAssignmentFlowE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_liveassigntest")
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
	provision(t, ctx, sqlDB, "teacher", "owner3@school.test")
	provision(t, ctx, sqlDB, "student", "started@school.test")
	provision(t, ctx, sqlDB, "student", "idle@school.test")
	provision(t, ctx, sqlDB, "student", "late@school.test")

	teacher := login(t, server, "owner3@school.test", "account-password")
	started := login(t, server, "started@school.test", "account-password")
	startedID := userID(t, ctx, sqlDB, "started@school.test")
	idleID := userID(t, ctx, sqlDB, "idle@school.test")
	lateID := userID(t, ctx, sqlDB, "late@school.test")

	status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
		map[string]string{"title": "Late Invite Under Test"}, teacher)
	if status != 201 {
		t.Fatalf("create quiz = %d %v", status, body)
	}
	quizID := body["quiz"].(map[string]any)["id"].(string)
	if status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions", map[string]any{
		"type": "truefalse", "body": map[string]string{"text": "Late invites are allowed live."},
		"correct": true,
	}, teacher); status != 201 {
		t.Fatalf("add question = %d %v", status, body)
	}
	if status, _, _ = itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
		map[string]any{"student_ids": []string{startedID, idleID}}, teacher); status != 200 {
		t.Fatalf("initial assign = %d", status)
	}
	if status, _, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish", map[string]any{
		"starts_at":    time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"ends_at":      time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
		"duration_sec": 120,
	}, teacher); status != 200 {
		t.Fatalf("publish = %d", status)
	}
	// Backdate starts_at so the quiz reads live lazily, same as kickflow_test.go.
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE quizzes SET starts_at = now() - interval '1 minute' WHERE id = $1`, quizID); err != nil {
		t.Fatalf("backdate starts_at: %v", err)
	}

	startedAttempt := start(t, server, quizID, started)

	t.Run("adding a student while live is a late invite", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
			map[string]any{"student_ids": []string{startedID, idleID, lateID}}, teacher)
		if status != 200 {
			t.Fatalf("late invite = %d %v", status, body)
		}
		students := body["students"].([]any)
		if len(students) != 3 {
			t.Fatalf("audience after late invite = %d, want 3", len(students))
		}
	})

	t.Run("removing a student with no in-progress attempt is allowed", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
			map[string]any{"student_ids": []string{startedID, lateID}}, teacher)
		if status != 200 {
			t.Fatalf("drop idle student = %d %v", status, body)
		}
		students := body["students"].([]any)
		if len(students) != 2 {
			t.Fatalf("audience after dropping idle student = %d, want 2", len(students))
		}
		for _, s := range students {
			if s.(map[string]any)["id"] == idleID {
				t.Fatalf("idle (never-started) student was not removed")
			}
		}
	})

	t.Run("removing a student with an in-progress attempt is refused", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
			map[string]any{"student_ids": []string{lateID}}, teacher)
		if status != 409 {
			t.Fatalf("drop in-progress student = %d %v, want 409", status, body)
		}
		if got := body["code"]; got != "ASSIGNMENT_IN_PROGRESS" {
			t.Fatalf("error code = %v, want ASSIGNMENT_IN_PROGRESS", got)
		}
		// The refused call changed nothing: the started student is still assigned.
		status, listBody, _ := itest.Call(t, server, "GET", "/api/v1/quizzes/"+quizID+"/assignments", nil, teacher)
		if status != 200 {
			t.Fatalf("list assignments = %d %v", status, listBody)
		}
		students := listBody["students"].([]any)
		found := false
		for _, s := range students {
			if s.(map[string]any)["id"] == startedID {
				found = true
			}
		}
		if !found {
			t.Fatalf("audience after refused removal lost the in-progress student: %v", students)
		}
	})

	t.Run("once the attempt is kicked, the student can be removed", func(t *testing.T) {
		if status, _, _ := itest.Call(t, server, "POST", "/api/v1/attempts/"+startedAttempt+"/kick",
			map[string]any{"reason": "freeing the seat for late invite test"}, teacher); status != 200 {
			t.Fatalf("kick = %d", status)
		}
		status, body, _ := itest.Call(t, server, "PUT", "/api/v1/quizzes/"+quizID+"/assignments",
			map[string]any{"student_ids": []string{lateID}}, teacher)
		if status != 200 {
			t.Fatalf("drop kicked student = %d %v", status, body)
		}
		students := body["students"].([]any)
		if len(students) != 1 {
			t.Fatalf("audience after dropping kicked student = %d, want 1", len(students))
		}
	})
}
