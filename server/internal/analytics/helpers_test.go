package analytics_test

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"testing"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/itest"
)

// provision, login and userID mirror the per-package test helpers in the quiz
// and attempt suites: each *_test package keeps its own copy since they are
// not exported.

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

func userID(t *testing.T, ctx context.Context, sqlDB *sql.DB, email string) string {
	t.Helper()
	var id string
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT id FROM users WHERE email = $1`, email).Scan(&id); err != nil {
		t.Fatalf("resolve %s: %v", email, err)
	}
	return id
}
