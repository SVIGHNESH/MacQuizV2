// The import-commit-flow integration test lives in an external test package
// so it can drive the real httpserver router (which imports quiz) without a
// cycle, matching importregisterflow_test.go.
package quiz_test

import (
	"context"
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

// TestCommitImportFlow pins docs/07 section 2 step 5 (commit a validated
// import transactionally) end to end: only a 'ready' import commits, the
// commit writes every parsed row as an ordinary question tagged
// source='import' with import_id, and the import flips to 'committed'. A
// commit attempt against an import still 'validating' (or already
// 'committed') is refused, matching "the commit is all-or-nothing".
//
// It runs in its own database (macquiz_importcommittest) - see
// itest.FreshDatabase.
func TestCommitImportFlow(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_importcommittest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	storage := quiz.LocalImportStorage{Dir: t.TempDir()}
	quizSvc := quiz.NewService(sqlDB, log, storage)
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
	provision(t, ctx, sqlDB, "student", "student@school.test")

	owner := login(t, server, "owner@school.test", "account-password")
	other := login(t, server, "other@school.test", "account-password")
	student := login(t, server, "student@school.test", "account-password")

	const header = "type,question,option_a,option_b,option_c,option_d,option_e,option_f,correct,points\n"

	var quizID string
	t.Run("draft quiz with one manual question already on it", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
			map[string]string{"title": "Commit target"}, owner)
		if status != 201 {
			t.Fatalf("create quiz = %d %v, want 201", status, body)
		}
		quizID = body["quiz"].(map[string]any)["id"].(string)

		status, body, _ = itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/questions",
			map[string]any{
				"type": "truefalse", "body": map[string]any{"text": "Manual one"},
				"correct": true, "points": 1,
			}, owner)
		if status != 201 {
			t.Fatalf("add manual question = %d %v, want 201", status, body)
		}
	})

	var stillValidatingID string
	t.Run("commit refuses an import still validating", func(t *testing.T) {
		csv := header + "single,Pick red,Red,Blue,,,,,a,2\n"
		status, body, _ := postFile(t, server, "/api/v1/quizzes/"+quizID+"/imports",
			"text/csv", csv, owner)
		if status != 201 {
			t.Fatalf("register import = %d %v, want 201", status, body)
		}
		stillValidatingID = body["import"].(map[string]any)["id"].(string)

		status, body, _ = itest.Call(t, server, "POST", "/api/v1/imports/"+stillValidatingID+"/commit", nil, owner)
		if status != 409 || body["code"] != "IMPORT_NOT_READY" {
			t.Fatalf("commit still-validating import = %d %v, want 409 IMPORT_NOT_READY", status, body)
		}
	})

	var importID string
	t.Run("owner registers and validates a clean two-row file", func(t *testing.T) {
		csv := header +
			"single,Pick red,Red,Blue,,,,,a,2\n" +
			"truefalse,Sky is blue,,,,,,,true,1\n"
		status, body, _ := postFile(t, server, "/api/v1/quizzes/"+quizID+"/imports",
			"text/csv", csv, owner)
		if status != 201 {
			t.Fatalf("register import = %d %v, want 201", status, body)
		}
		importID = body["import"].(map[string]any)["id"].(string)

		if err := quiz.ValidateImport(ctx, sqlDB, storage, importID); err != nil {
			t.Fatalf("ValidateImport: %v", err)
		}
	})

	t.Run("student is forbidden", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/imports/"+importID+"/commit", nil, student)
		if status != 403 || body["code"] != "FORBIDDEN" {
			t.Fatalf("student commit import = %d %v, want 403 FORBIDDEN", status, body)
		}
	})

	t.Run("non-owner gets 404", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/imports/"+importID+"/commit", nil, other)
		if status != 404 || body["code"] != "NOT_FOUND" {
			t.Fatalf("non-owner commit import = %d %v, want 404 NOT_FOUND", status, body)
		}
	})

	t.Run("owner commits the ready import", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/imports/"+importID+"/commit", nil, owner)
		if status != 200 {
			t.Fatalf("commit import = %d %v, want 200", status, body)
		}
		imp := body["import"].(map[string]any)
		if imp["status"] != "committed" {
			t.Fatalf("import status = %v, want committed", imp["status"])
		}
		questions, ok := body["questions"].([]any)
		if !ok || len(questions) != 2 {
			t.Fatalf("committed questions = %v, want 2", body["questions"])
		}

		status, getBody, _ := itest.Call(t, server, "GET", "/api/v1/quizzes/"+quizID, nil, owner)
		if status != 200 {
			t.Fatalf("get quiz = %d %v, want 200", status, getBody)
		}
		allQuestions := getBody["questions"].([]any)
		if len(allQuestions) != 3 {
			t.Fatalf("quiz question count = %d, want 3 (1 manual + 2 imported)", len(allQuestions))
		}

		var importedCount int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM questions WHERE quiz_id = $1 AND source = 'import' AND import_id = $2`,
			quizID, importID).Scan(&importedCount); err != nil {
			t.Fatalf("count imported questions: %v", err)
		}
		if importedCount != 2 {
			t.Fatalf("imported questions in db = %d, want 2", importedCount)
		}
	})

	t.Run("committing again is refused", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/imports/"+importID+"/commit", nil, owner)
		if status != 409 || body["code"] != "IMPORT_NOT_READY" {
			t.Fatalf("re-commit import = %d %v, want 409 IMPORT_NOT_READY", status, body)
		}
	})

	t.Run("a committed.imports audit row was written", func(t *testing.T) {
		var got int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM audit_log WHERE action = 'imports.committed' AND resource_id = $1`,
			importID).Scan(&got); err != nil {
			t.Fatalf("count imports.committed: %v", err)
		}
		if got != 1 {
			t.Fatalf("imports.committed audit rows = %d, want 1", got)
		}
	})
}
