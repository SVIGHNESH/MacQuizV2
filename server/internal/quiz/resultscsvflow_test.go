package quiz_test

import (
	"context"
	"encoding/csv"
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

// getCSV performs a raw GET request with cookies (unlike itest.Call, which
// always decodes the body as JSON) and returns the status, content type,
// Content-Disposition header, and raw body.
func getCSV(t *testing.T, server *httptest.Server, path string, cookies map[string]string) (int, string, string, []byte) {
	t.Helper()
	req, err := http.NewRequest("GET", server.URL+path, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for name, value := range cookies {
		req.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, resp.Header.Get("Content-Type"), resp.Header.Get("Content-Disposition"), raw
}

// TestResultsCSVExport pins docs/07 section 4's "CSV exports" (Milestone 8's
// last documented-but-unbuilt gap): GET /quizzes/:id/results.csv renders the
// same per-student results table as GET /quizzes/:id/results as a
// downloadable text/csv gradebook, gated by the same owner-or-admin-only
// authorization as the JSON view.
//
// It runs in its own database (macquiz_resultscsvtest) - see
// itest.FreshDatabase.
func TestResultsCSVExport(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, baseURL, "macquiz_resultscsvtest")
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
	provision(t, ctx, sqlDB, "teacher", "other@school.test")
	provision(t, ctx, sqlDB, "student", "pupil@school.test")
	admin := login(t, server, "admin@school.test", "admin-password-1")
	owner := login(t, server, "owner@school.test", "account-password")
	other := login(t, server, "other@school.test", "account-password")
	student := login(t, server, "pupil@school.test", "account-password")
	pupilID := userID(t, ctx, sqlDB, "pupil@school.test")

	quizID := authorMinimalQuiz(t, server, owner, "Quiz \"Export\", v1")
	assign(t, server, owner, quizID, pupilID)
	if status, body, _ := itest.Call(t, server, "POST", "/api/v1/quizzes/"+quizID+"/publish",
		map[string]any{
			"starts_at":      time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
			"ends_at":        time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339),
			"duration_sec":   600,
			"release_policy": "manual",
		}, owner); status != 200 {
		t.Fatalf("publish = %d %v", status, body)
	}

	// Give the assigned student a graded, submitted attempt so the export has
	// a real score row, not just an all-null placeholder line.
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO attempts (quiz_id, student_id, attempt_no, quiz_version, deadline_at, submitted_at, submit_kind, score, status)
		 VALUES ($1, $2, 1, 1, now() + interval '1 hour', now(), 'manual', 7.5, 'submitted')`,
		quizID, pupilID); err != nil {
		t.Fatalf("seed attempt: %v", err)
	}

	t.Run("only the owning teacher may export", func(t *testing.T) {
		// The route sits behind requireTeacher (docs/08: admins cannot author),
		// so a student and an admin never reach the service at all - both are
		// 403, same as every other teacher-only authoring route. Only a
		// non-owning teacher clears the middleware and hits the service's
		// ownership check, which answers existence-safe 404.
		cases := []struct {
			name    string
			cookies map[string]string
			target  string
			want    int
		}{
			{"student is forbidden", student, quizID, 403},
			{"admin cannot author", admin, quizID, 403},
			{"non-owning teacher is not found", other, quizID, 404},
			{"unknown quiz is not found", owner, "00000000-0000-0000-0000-000000000000", 404},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				status, _, _, _ := getCSV(t, server, "/api/v1/quizzes/"+tc.target+"/results.csv", tc.cookies)
				if status != tc.want {
					t.Fatalf("export = %d, want %d", status, tc.want)
				}
			})
		}
	})

	t.Run("the owner downloads the gradebook CSV", func(t *testing.T) {
		status, contentType, disposition, raw := getCSV(t, server, "/api/v1/quizzes/"+quizID+"/results.csv", owner)
		if status != 200 {
			t.Fatalf("export = %d, want 200", status)
		}
		if !strings.HasPrefix(contentType, "text/csv") {
			t.Fatalf("content-type = %q, want text/csv prefix", contentType)
		}
		if !strings.Contains(disposition, "attachment") || !strings.HasSuffix(disposition, `.csv"`) {
			t.Fatalf("content-disposition = %q, want an attachment .csv filename", disposition)
		}
		// The title's quote and comma must not have leaked into the header value
		// unescaped or corrupted it.
		if strings.ContainsAny(disposition, `",`) && !strings.HasPrefix(disposition, `attachment; filename="`) {
			t.Fatalf("content-disposition looks malformed: %q", disposition)
		}

		rows, err := csv.NewReader(strings.NewReader(string(raw))).ReadAll()
		if err != nil {
			t.Fatalf("parse csv: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("csv rows = %d, want 2 (header + one student)", len(rows))
		}
		wantHeader := []string{
			"student_name", "email", "attempt_no", "status", "submit_kind",
			"started_at", "submitted_at", "score", "max_score",
		}
		if strings.Join(rows[0], ",") != strings.Join(wantHeader, ",") {
			t.Fatalf("csv header = %v, want %v", rows[0], wantHeader)
		}
		data := rows[1]
		if data[1] != "pupil@school.test" {
			t.Fatalf("csv email = %q, want pupil@school.test", data[1])
		}
		if data[3] != "submitted" {
			t.Fatalf("csv status = %q, want submitted", data[3])
		}
		if data[7] != "7.5" {
			t.Fatalf("csv score = %q, want 7.5", data[7])
		}
	})
}
