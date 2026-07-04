// The auth-flow integration test lives in an external test package so it can
// drive the real httpserver router (which imports authusers) without a cycle.
package authusers_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"
	"time"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/db"
	"macquiz/server/internal/httpserver"
)

// TestAuthFlowE2E drives Milestone 1's exit criteria for auth over real HTTP
// and a real Postgres: bootstrap admin, login, /me, refresh rotation with
// reuse detection, the forced first-login password reset, logout, and the
// login rate limit.
//
// It runs in its own database (macquiz_authtest) so it can never race the
// migration lifecycle test in internal/db, which repeatedly drops the schema
// of the main test database while `go test ./...` runs packages in parallel.
func TestAuthFlowE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := freshDatabase(t, ctx, baseURL, "macquiz_authtest")
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

	// Bootstrap is idempotent: the second call must be a no-op.
	for range 2 {
		if err := svc.EnsureBootstrapAdmin(ctx, "admin@school.test", "admin-password-1", "Root Admin"); err != nil {
			t.Fatalf("bootstrap admin: %v", err)
		}
	}
	var admins int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE role = 'admin'`).Scan(&admins); err != nil || admins != 1 {
		t.Fatalf("admin count = %d (err %v), want exactly 1", admins, err)
	}

	t.Run("readyz", func(t *testing.T) {
		status, _, _ := call(t, server, "GET", "/readyz", nil, nil)
		if status != 200 {
			t.Fatalf("GET /readyz = %d, want 200", status)
		}
	})

	t.Run("login", func(t *testing.T) {
		status, body, _ := call(t, server, "POST", "/api/v1/auth/login",
			map[string]string{"email": "admin@school.test", "password": "wrong"}, nil)
		if status != 401 || body["code"] != "INVALID_CREDENTIALS" {
			t.Fatalf("bad-password login = %d %v, want 401 INVALID_CREDENTIALS", status, body)
		}

		status, body, cookies := call(t, server, "POST", "/api/v1/auth/login",
			map[string]string{"email": "admin@school.test", "password": "admin-password-1"}, nil)
		if status != 200 {
			t.Fatalf("login = %d %v, want 200", status, body)
		}
		if cookies["mq_access"] == "" || cookies["mq_refresh"] == "" {
			t.Fatalf("login did not set both session cookies, got %v", cookies)
		}
		user := body["user"].(map[string]any)
		if user["role"] != "admin" || user["must_change_password"] != false {
			t.Fatalf("login user = %v, want admin with must_change_password=false", user)
		}
		if _, leaked := user["password_hash"]; leaked {
			t.Fatal("login response contains password_hash")
		}

		status, body, _ = call(t, server, "GET", "/api/v1/auth/me", nil, cookies)
		if status != 200 || body["user"].(map[string]any)["email"] != "admin@school.test" {
			t.Fatalf("GET /me = %d %v, want 200 for admin", status, body)
		}
	})

	t.Run("me requires auth", func(t *testing.T) {
		status, body, _ := call(t, server, "GET", "/api/v1/auth/me", nil, nil)
		if status != 401 || body["code"] != "UNAUTHENTICATED" {
			t.Fatalf("unauthenticated /me = %d %v, want 401 UNAUTHENTICATED", status, body)
		}
	})

	t.Run("refresh rotation and reuse detection", func(t *testing.T) {
		_, _, first := call(t, server, "POST", "/api/v1/auth/login",
			map[string]string{"email": "admin@school.test", "password": "admin-password-1"}, nil)

		status, _, second := call(t, server, "POST", "/api/v1/auth/refresh", nil, first)
		if status != 200 {
			t.Fatalf("refresh = %d, want 200", status)
		}
		if second["mq_refresh"] == "" || second["mq_refresh"] == first["mq_refresh"] {
			t.Fatal("refresh did not rotate the refresh token")
		}

		// Replaying the pre-rotation token is reuse: it must fail AND kill
		// the successor (the whole family), per docs/08-security.md.
		status, _, _ = call(t, server, "POST", "/api/v1/auth/refresh", nil, first)
		if status != 401 {
			t.Fatalf("reused refresh token = %d, want 401", status)
		}
		status, _, _ = call(t, server, "POST", "/api/v1/auth/refresh", nil, second)
		if status != 401 {
			t.Fatalf("successor token after reuse = %d, want 401 (family revoked)", status)
		}
	})

	t.Run("forced first-login password reset", func(t *testing.T) {
		hash, err := authusers.HashPassword("issued-by-admin")
		if err != nil {
			t.Fatalf("hash: %v", err)
		}
		// Direct provisioning stand-in until POST /users lands; the
		// must_change_password column defaults to true for exactly this case.
		if _, err := sqlDB.ExecContext(ctx,
			`INSERT INTO users (role, email, password_hash, full_name, created_by)
			 VALUES ('teacher', 'teacher@school.test', $1, 'First Teacher',
			         (SELECT id FROM users WHERE role = 'admin'))`, hash); err != nil {
			t.Fatalf("provision teacher: %v", err)
		}

		status, body, cookies := call(t, server, "POST", "/api/v1/auth/login",
			map[string]string{"email": "teacher@school.test", "password": "issued-by-admin"}, nil)
		if status != 200 || body["user"].(map[string]any)["must_change_password"] != true {
			t.Fatalf("teacher first login = %d %v, want 200 with must_change_password=true", status, body)
		}

		// The gate middleware blocks module routes until the reset happens.
		gate := authusers.RequirePasswordChanged(
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
		gated := svc.RequireAuth(gate)
		req := httptest.NewRequest("GET", "/probe", nil)
		req.AddCookie(&http.Cookie{Name: "mq_access", Value: cookies["mq_access"]})
		rec := httptest.NewRecorder()
		gated.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Fatalf("gated route before reset = %d, want 403", rec.Code)
		}

		status, body, _ = call(t, server, "POST", "/api/v1/auth/password",
			map[string]string{"current_password": "issued-by-admin", "new_password": "short"}, cookies)
		if status != 422 {
			t.Fatalf("weak new password = %d %v, want 422", status, body)
		}
		status, body, _ = call(t, server, "POST", "/api/v1/auth/password",
			map[string]string{"current_password": "wrong", "new_password": "long-enough-password"}, cookies)
		if status != 401 {
			t.Fatalf("wrong current password = %d %v, want 401", status, body)
		}
		status, _, _ = call(t, server, "POST", "/api/v1/auth/password",
			map[string]string{"current_password": "issued-by-admin", "new_password": "my-own-password-9"}, cookies)
		if status != 204 {
			t.Fatalf("password change = %d, want 204", status)
		}

		// The change revoked every session; the old refresh token is dead.
		status, _, _ = call(t, server, "POST", "/api/v1/auth/refresh", nil, cookies)
		if status != 401 {
			t.Fatalf("refresh after password change = %d, want 401", status)
		}

		status, body, cookies = call(t, server, "POST", "/api/v1/auth/login",
			map[string]string{"email": "teacher@school.test", "password": "my-own-password-9"}, nil)
		if status != 200 || body["user"].(map[string]any)["must_change_password"] != false {
			t.Fatalf("re-login = %d %v, want 200 with must_change_password=false", status, body)
		}
		rec = httptest.NewRecorder()
		req = httptest.NewRequest("GET", "/probe", nil)
		req.AddCookie(&http.Cookie{Name: "mq_access", Value: cookies["mq_access"]})
		gated.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("gated route after reset = %d, want 200", rec.Code)
		}

		var audits int
		if err := sqlDB.QueryRowContext(ctx,
			`SELECT count(*) FROM audit_log WHERE action = 'auth.password_changed'`,
		).Scan(&audits); err != nil || audits != 1 {
			t.Fatalf("password-change audit rows = %d (err %v), want 1", audits, err)
		}
	})

	t.Run("logout", func(t *testing.T) {
		_, _, cookies := call(t, server, "POST", "/api/v1/auth/login",
			map[string]string{"email": "admin@school.test", "password": "admin-password-1"}, nil)
		status, _, _ := call(t, server, "POST", "/api/v1/auth/logout", nil, cookies)
		if status != 204 {
			t.Fatalf("logout = %d, want 204", status)
		}
		status, _, _ = call(t, server, "POST", "/api/v1/auth/refresh", nil, cookies)
		if status != 401 {
			t.Fatalf("refresh after logout = %d, want 401", status)
		}
	})

	t.Run("login rate limit per account", func(t *testing.T) {
		var status int
		for range 6 {
			status, _, _ = call(t, server, "POST", "/api/v1/auth/login",
				map[string]string{"email": "nobody@school.test", "password": "x"}, nil)
		}
		if status != 429 {
			t.Fatalf("6th login for one account = %d, want 429", status)
		}
	})
}

// freshDatabase drops and recreates a dedicated test database, returning a
// connection to it (closed via t.Cleanup).
func freshDatabase(t *testing.T, ctx context.Context, baseURL, name string) *sql.DB {
	t.Helper()
	admin, err := db.Open(ctx, baseURL)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()
	if _, err := admin.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE)", name)); err != nil {
		t.Fatalf("drop test database: %v", err)
	}
	if _, err := admin.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s", name)); err != nil {
		t.Fatalf("create test database: %v", err)
	}

	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse database url: %v", err)
	}
	u.Path = "/" + name
	testDB, err := db.Open(ctx, u.String())
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { testDB.Close() })
	return testDB
}

// call performs one JSON request with explicit cookie control (no jar), so
// tests can replay old tokens deliberately. It returns the status, the
// decoded body (nil when empty), and cookies merged with Set-Cookie updates.
func call(t *testing.T, server *httptest.Server, method, path string, body any, cookies map[string]string) (int, map[string]any, map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequest(method, server.URL+path, &buf)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for name, value := range cookies {
		req.AddCookie(&http.Cookie{Name: name, Value: value})
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()

	merged := map[string]string{}
	for k, v := range cookies {
		merged[k] = v
	}
	for _, c := range resp.Cookies() {
		merged[c.Name] = c.Value
	}
	raw, _ := io.ReadAll(resp.Body)
	var decoded map[string]any
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("%s %s: body is not JSON: %v (%q)", method, path, err, raw)
		}
	}
	return resp.StatusCode, decoded, merged
}
