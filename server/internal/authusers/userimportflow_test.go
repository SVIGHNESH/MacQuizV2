package authusers_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
)

// TestUserImportE2E drives POST /users/import over real HTTP and a real
// Postgres: the policy gate, both roster formats, the all-or-nothing
// transaction, and the row-level error report. It runs in its own database
// so it can never race the other DB-backed tests.
func TestUserImportE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := freshDatabase(t, ctx, baseURL, "macquiz_userimporttest")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := authusers.NewService(sqlDB, "test-secret", log)
	handler := authusers.NewHandler(svc, false)
	router := httpserver.New(httpserver.BuildInfo{Version: "test"},
		httpserver.Deps{DB: sqlDB, Auth: handler})
	server := httptest.NewServer(router)
	defer server.Close()

	if err := svc.EnsureBootstrapAdmin(ctx, "admin@school.test", "admin-password-1", "Root Admin"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	_, _, admin := call(t, server, "POST", "/api/v1/auth/login",
		map[string]string{"email": "admin@school.test", "password": "admin-password-1"}, nil)

	roster := "role,email,full_name\n" +
		"student,ada@school.test,Ada Student\n" +
		"student,ben@school.test,\"Student, Ben\"\n" +
		"teacher,terry@school.test,Terry Teacher\n"

	t.Run("routes are gated", func(t *testing.T) {
		status, body := uploadRoster(t, server, []byte(roster), nil)
		if status != 401 {
			t.Fatalf("unauthenticated import = %d %v, want 401", status, body)
		}
	})

	var adaPassword string
	t.Run("csv roster provisions every row", func(t *testing.T) {
		status, body := uploadRoster(t, server, []byte(roster), admin)
		if status != 201 {
			t.Fatalf("import = %d %v, want 201", status, body)
		}
		users := body["users"].([]any)
		if len(users) != 3 {
			t.Fatalf("created %d users, want 3", len(users))
		}
		first := users[0].(map[string]any)
		u := first["user"].(map[string]any)
		if u["email"] != "ada@school.test" || u["role"] != "student" || u["must_change_password"] != true {
			t.Fatalf("first user = %v, want ada, student, must_change_password", u)
		}
		adaPassword, _ = first["initial_password"].(string)
		if adaPassword == "" {
			t.Fatal("import did not return the one-time initial_password")
		}
	})

	t.Run("generated credential works and forces reset", func(t *testing.T) {
		status, body, cookies := call(t, server, "POST", "/api/v1/auth/login",
			map[string]string{"email": "ada@school.test", "password": adaPassword}, nil)
		if status != 200 {
			t.Fatalf("imported student login = %d %v, want 200", status, body)
		}
		status, body, _ = call(t, server, "GET", "/api/v1/users", nil, cookies)
		if status != 403 || body["code"] != "PASSWORD_CHANGE_REQUIRED" {
			t.Fatalf("pre-reset call = %d %v, want 403 PASSWORD_CHANGE_REQUIRED", status, body)
		}
	})

	t.Run("non-admins are forbidden", func(t *testing.T) {
		_, _, cookies := call(t, server, "POST", "/api/v1/auth/login",
			map[string]string{"email": "ada@school.test", "password": adaPassword}, nil)
		status, _, _ := call(t, server, "POST", "/api/v1/auth/password",
			map[string]string{"current_password": adaPassword, "new_password": "ada-owns-this-1"}, cookies)
		if status != 204 {
			t.Fatalf("reset = %d, want 204", status)
		}
		_, _, cookies = call(t, server, "POST", "/api/v1/auth/login",
			map[string]string{"email": "ada@school.test", "password": "ada-owns-this-1"}, nil)
		status, body := uploadRoster(t, server, []byte(roster), cookies)
		if status != 403 || body["code"] != "FORBIDDEN" {
			t.Fatalf("student import = %d %v, want 403 FORBIDDEN", status, body)
		}
	})

	t.Run("xlsx roster provisions too", func(t *testing.T) {
		data := rosterXLSX(t, [][]string{
			{"role", "email", "full_name"},
			{"student", "xena@school.test", "Xena Student"},
		})
		status, body := uploadRoster(t, server, data, admin)
		if status != 201 || len(body["users"].([]any)) != 1 {
			t.Fatalf("xlsx import = %d %v, want 201 with 1 user", status, body)
		}
	})

	t.Run("taken emails come back as row errors and nothing commits", func(t *testing.T) {
		mixed := "role,email,full_name\n" +
			"student,fresh@school.test,Fresh Face\n" +
			"student,ada@school.test,Ada Again\n"
		status, body := uploadRoster(t, server, []byte(mixed), admin)
		if status != 422 {
			t.Fatalf("conflicting import = %d %v, want 422", status, body)
		}
		rowErrs := body["row_errors"].([]any)
		if len(rowErrs) != 1 {
			t.Fatalf("row_errors = %v, want exactly the taken email", rowErrs)
		}
		re := rowErrs[0].(map[string]any)
		if re["row"] != float64(2) || re["column"] != "email" || re["message"] != "already in use" {
			t.Fatalf("row error = %v", re)
		}

		// All-or-nothing: the valid row must not have been created.
		status, listBody, _ := call(t, server, "GET", "/api/v1/users", nil, admin)
		if status != 200 {
			t.Fatalf("list users = %d, want 200", status)
		}
		for _, u := range listBody["users"].([]any) {
			if u.(map[string]any)["email"] == "fresh@school.test" {
				t.Fatal("fresh@school.test was created despite the failed import")
			}
		}
	})

	t.Run("file-level problems reject the upload", func(t *testing.T) {
		status, body := uploadRoster(t, server, []byte("email,full_name\na@school.test,A\n"), admin)
		fields, _ := body["fields"].(map[string]any)
		if status != 422 || !strings.Contains(fmt.Sprint(fields["file"]), "missing required column") {
			t.Fatalf("missing column = %d %v, want 422 file error", status, body)
		}

		status, body = uploadRoster(t, server, []byte("role,email,full_name\n,,\n"), admin)
		fields, _ = body["fields"].(map[string]any)
		if status != 422 || fields["file"] != "the file has no account rows" {
			t.Fatalf("empty roster = %d %v, want 422 no-rows error", status, body)
		}

		big := bytes.Repeat([]byte("student,someone@school.test,Some One\n"), 1<<15)
		status, body = uploadRoster(t, server, append([]byte("role,email,full_name\n"), big...), admin)
		fields, _ = body["fields"].(map[string]any)
		if status != 422 || fields["file"] != "must be 1 MB or smaller" {
			t.Fatalf("oversized roster = %d %v, want 422 size error", status, body)
		}
	})
}

// uploadRoster posts a raw roster file body, the wire shape of POST
// /users/import (the client sends the file bytes, not JSON).
func uploadRoster(t *testing.T, server *httptest.Server, data []byte, cookies map[string]string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest("POST", server.URL+"/api/v1/users/import", bytes.NewReader(data))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "text/csv")
	for name, value := range cookies {
		req.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /users/import: %v", err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil && err != io.EOF {
		t.Fatalf("decode response: %v", err)
	}
	return resp.StatusCode, body
}
