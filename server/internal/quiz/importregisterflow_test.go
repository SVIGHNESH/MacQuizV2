// The import-register-flow integration test lives in an external test
// package so it can drive the real httpserver router (which imports quiz)
// without a cycle.
package quiz_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/db"
	"macquiz/server/internal/httpserver"
	"macquiz/server/internal/itest"
	"macquiz/server/internal/quiz"
)

// TestRegisterImportFlow pins docs/07 section 2 step 2 (register a bulk
// upload) end to end: a teacher's file lands through ImportUploadStore, an
// imports row is created in 'validating' with an import_validate job
// enqueued, and the worker (quiz.ValidateImport) can read the very file the
// handler wrote back out through the same storage. Ownership and draft-only
// gates match every other authoring mutation: a non-owner and a non-draft
// quiz both refuse before any file is written, and a file over the docs/07
// 10 MB cap is rejected with a field error.
//
// It runs in its own database (macquiz_importregistertest) - see
// itest.FreshDatabase.
func TestRegisterImportFlow(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_importregistertest")
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
			map[string]string{"title": "Bulk import target"}, owner)
		if status != 201 {
			t.Fatalf("create quiz = %d %v, want 201", status, body)
		}
		quizID = body["quiz"].(map[string]any)["id"].(string)
	})

	t.Run("student is forbidden", func(t *testing.T) {
		status, body, _ := postFile(t, server, "/api/v1/quizzes/"+quizID+"/imports",
			"text/csv", header, student)
		if status != 403 || body["code"] != "FORBIDDEN" {
			t.Fatalf("student register import = %d %v, want 403 FORBIDDEN", status, body)
		}
	})

	t.Run("non-owner gets 404", func(t *testing.T) {
		status, body, _ := postFile(t, server, "/api/v1/quizzes/"+quizID+"/imports",
			"text/csv", header, other)
		if status != 404 || body["code"] != "NOT_FOUND" {
			t.Fatalf("non-owner register import = %d %v, want 404 NOT_FOUND", status, body)
		}
	})

	t.Run("file over the 10 MB cap is rejected", func(t *testing.T) {
		huge := header + strings.Repeat("x", quiz.MaxImportFileBytes+1)
		status, body, _ := postFile(t, server, "/api/v1/quizzes/"+quizID+"/imports",
			"text/csv", huge, owner)
		if status != 422 || body["code"] != "VALIDATION_FAILED" {
			t.Fatalf("oversized import = %d %v, want 422 VALIDATION_FAILED", status, body)
		}
		if _, ok := body["fields"].(map[string]any)["file"]; !ok {
			t.Fatalf("oversized import fields = %v, want a file error", body["fields"])
		}
	})

	var importID string
	t.Run("owner registers a clean file", func(t *testing.T) {
		csv := header + "single,Pick red,Red,Blue,,,,,a,2\n"
		status, body, _ := postFile(t, server, "/api/v1/quizzes/"+quizID+"/imports",
			"text/csv", csv, owner)
		if status != 201 {
			t.Fatalf("register import = %d %v, want 201", status, body)
		}
		imp := body["import"].(map[string]any)
		if imp["status"] != "validating" || imp["quiz_id"] != quizID {
			t.Fatalf("import = %v, want validating for quiz %s", imp, quizID)
		}
		importID = imp["id"].(string)
	})

	t.Run("published quizzes refuse new imports", func(t *testing.T) {
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE quizzes SET status = 'scheduled' WHERE id = $1`, quizID); err != nil {
			t.Fatalf("force status: %v", err)
		}
		status, body, _ := postFile(t, server, "/api/v1/quizzes/"+quizID+"/imports",
			"text/csv", header, owner)
		if status != 409 || body["code"] != "QUIZ_NOT_EDITABLE" {
			t.Fatalf("register import on scheduled quiz = %d %v, want 409 QUIZ_NOT_EDITABLE", status, body)
		}
	})

	t.Run("a job was enqueued and the worker can read the file back", func(t *testing.T) {
		var jobCount int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM river_job WHERE kind = 'import_validate'`).Scan(&jobCount); err != nil {
			t.Fatalf("count import_validate jobs: %v", err)
		}
		if jobCount != 1 {
			t.Fatalf("import_validate jobs = %d, want 1", jobCount)
		}

		if err := quiz.ValidateImport(ctx, sqlDB, storage, importID); err != nil {
			t.Fatalf("ValidateImport: %v", err)
		}
		var status string
		var rowCount int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT status, row_count FROM imports WHERE id = $1`, importID).Scan(&status, &rowCount); err != nil {
			t.Fatalf("load import: %v", err)
		}
		if status != "ready" || rowCount != 1 {
			t.Fatalf("import after validation = status %q row_count %d, want ready 1", status, rowCount)
		}
	})

	t.Run("every registration left an audit row", func(t *testing.T) {
		var got int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM audit_log WHERE action = 'imports.registered'`).Scan(&got); err != nil {
			t.Fatalf("count imports.registered: %v", err)
		}
		if got != 1 {
			t.Fatalf("imports.registered audit rows = %d, want 1", got)
		}
	})
}

// postFile sends a raw-body request the way a bulk-upload client would,
// bypassing itest.Call's JSON encoding.
func postFile(t *testing.T, server *httptest.Server, path, contentType, body string, cookies map[string]string) (int, map[string]any, map[string]string) {
	t.Helper()
	req, err := http.NewRequest("POST", server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", contentType)
	for name, value := range cookies {
		req.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	decoded := map[string]any{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("POST %s: body is not JSON: %v (%q)", path, err, raw)
		}
	}
	merged := map[string]string{}
	for k, v := range cookies {
		merged[k] = v
	}
	for _, c := range resp.Cookies() {
		merged[c.Name] = c.Value
	}
	return resp.StatusCode, decoded, merged
}
