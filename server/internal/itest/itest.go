// Package itest holds helpers for DB-backed HTTP integration tests. Every
// such test runs in its own dedicated database because `go test ./...` runs
// packages in parallel and the db package's migration lifecycle test
// repeatedly drops the main test database's schema.
package itest

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"macquiz/server/internal/db"
)

// FreshDatabase drops and recreates a dedicated test database, returning a
// connection to it (closed via t.Cleanup).
func FreshDatabase(t *testing.T, ctx context.Context, baseURL, name string) *sql.DB {
	t.Helper()
	admin, err := db.Open(ctx, baseURL, 0)
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
	testDB, err := db.Open(ctx, u.String(), 0)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() { testDB.Close() })
	return testDB
}

// Call performs one JSON request with explicit cookie control (no jar), so
// tests can replay old tokens deliberately. It returns the status, the
// decoded body (nil when empty), and cookies merged with Set-Cookie updates.
func Call(t *testing.T, server *httptest.Server, method, path string, body any, cookies map[string]string) (int, map[string]any, map[string]string) {
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
