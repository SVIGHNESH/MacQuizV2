// The import-get-flow integration test lives in an external test package so
// it can drive the real httpserver router (which imports quiz) without a
// cycle, matching importregisterflow_test.go and importcommitflow_test.go.
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

// TestGetImportFlow pins docs/07 section 2 step 4 (the review UI polls an
// import's status): a freshly registered import reads back 'validating', the
// worker resolves it to 'ready' or 'failed' (with the row-level
// error_report populated on failure), a student is forbidden, and a
// non-owning teacher gets 404 rather than a 403 that would leak existence.
//
// It runs in its own database (macquiz_importgettest) - see
// itest.FreshDatabase.
func TestGetImportFlow(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_importgettest")
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
	t.Run("draft quiz", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes",
			map[string]string{"title": "Get-import target"}, owner)
		if status != 201 {
			t.Fatalf("create quiz = %d %v, want 201", status, body)
		}
		quizID = body["quiz"].(map[string]any)["id"].(string)
	})

	var importID string
	t.Run("register reads back as validating", func(t *testing.T) {
		csv := header + "single,Pick red,Red,Blue,,,,,a,2\n"
		status, body, _ := postFile(t, server, "/api/v1/quizzes/"+quizID+"/imports",
			"text/csv", csv, owner)
		if status != 201 {
			t.Fatalf("register import = %d %v, want 201", status, body)
		}
		importID = body["import"].(map[string]any)["id"].(string)

		status, body, _ = itest.Call(t, server, "GET", "/api/v1/imports/"+importID, nil, owner)
		if status != 200 {
			t.Fatalf("get import = %d %v, want 200", status, body)
		}
		imp := body["import"].(map[string]any)
		if imp["status"] != "validating" {
			t.Fatalf("import status = %v, want validating", imp["status"])
		}
		if imp["row_count"] != nil {
			t.Fatalf("row_count = %v, want nil before validation", imp["row_count"])
		}
	})

	t.Run("student is forbidden", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/imports/"+importID, nil, student)
		if status != 403 || body["code"] != "FORBIDDEN" {
			t.Fatalf("student get import = %d %v, want 403 FORBIDDEN", status, body)
		}
	})

	t.Run("non-owner gets 404", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/imports/"+importID, nil, other)
		if status != 404 || body["code"] != "NOT_FOUND" {
			t.Fatalf("non-owner get import = %d %v, want 404 NOT_FOUND", status, body)
		}
	})

	t.Run("unknown id gets 404", func(t *testing.T) {
		status, body, _ := itest.Call(t, server, "GET",
			"/api/v1/imports/00000000-0000-0000-0000-000000000000", nil, owner)
		if status != 404 || body["code"] != "NOT_FOUND" {
			t.Fatalf("unknown import = %d %v, want 404 NOT_FOUND", status, body)
		}
	})

	t.Run("worker resolves it to ready and the review UI sees that", func(t *testing.T) {
		if err := quiz.ValidateImport(ctx, sqlDB, storage, importID); err != nil {
			t.Fatalf("ValidateImport: %v", err)
		}
		status, body, _ := itest.Call(t, server, "GET", "/api/v1/imports/"+importID, nil, owner)
		if status != 200 {
			t.Fatalf("get import = %d %v, want 200", status, body)
		}
		imp := body["import"].(map[string]any)
		if imp["status"] != "ready" {
			t.Fatalf("import status = %v, want ready", imp["status"])
		}
		if imp["row_count"] != float64(1) {
			t.Fatalf("row_count = %v, want 1", imp["row_count"])
		}
	})

	var failedImportID string
	t.Run("a bad file resolves to failed with a row-level error report", func(t *testing.T) {
		csv := header + "single,Pick red,Red,Blue,,,,,z,2\n" // 'z' is not among the options
		status, body, _ := postFile(t, server, "/api/v1/quizzes/"+quizID+"/imports",
			"text/csv", csv, owner)
		if status != 201 {
			t.Fatalf("register import = %d %v, want 201", status, body)
		}
		failedImportID = body["import"].(map[string]any)["id"].(string)

		if err := quiz.ValidateImport(ctx, sqlDB, storage, failedImportID); err != nil {
			t.Fatalf("ValidateImport: %v", err)
		}

		status, body, _ = itest.Call(t, server, "GET", "/api/v1/imports/"+failedImportID, nil, owner)
		if status != 200 {
			t.Fatalf("get import = %d %v, want 200", status, body)
		}
		imp := body["import"].(map[string]any)
		if imp["status"] != "failed" {
			t.Fatalf("import status = %v, want failed", imp["status"])
		}
		errs, ok := imp["error_report"].([]any)
		if !ok || len(errs) != 1 {
			t.Fatalf("error_report = %v, want 1 row error", imp["error_report"])
		}
	})
}
