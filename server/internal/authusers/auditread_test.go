package authusers_test

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/db"
	"macquiz/server/internal/httpserver"
)

// TestAuditReadE2E drives GET /api/v1/audit over real HTTP and Postgres: the
// admin-only gate, the filter set, keyset pagination across pages, and correct
// handling of NULL actor_id/detail columns (which a naive scan into
// *json.RawMessage would 500 on). It seeds audit_log directly so the counts and
// ordering are deterministic regardless of what other mutations logged.
func TestAuditReadE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := freshDatabase(t, ctx, baseURL, "macquiz_auditreadtest")
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
	_, adminBody, admin := call(t, server, "POST", "/api/v1/auth/login",
		map[string]string{"email": "admin@school.test", "password": "admin-password-1"}, nil)
	adminID := adminBody["user"].(map[string]any)["id"].(string)

	t.Run("gate: unauth, teacher, student are refused; admin is allowed", func(t *testing.T) {
		status, body, _ := call(t, server, "GET", "/api/v1/audit", nil, nil)
		if status != 401 || body["code"] != "UNAUTHENTICATED" {
			t.Fatalf("unauth GET /audit = %d %v, want 401 UNAUTHENTICATED", status, body)
		}
		teacher := provisionAndLogin(t, server, admin, "teacher", "teach@school.test")
		status, body, _ = call(t, server, "GET", "/api/v1/audit", nil, teacher)
		if status != 403 || body["code"] != "FORBIDDEN" {
			t.Fatalf("teacher GET /audit = %d %v, want 403 FORBIDDEN", status, body)
		}
		student := provisionAndLogin(t, server, admin, "student", "stud@school.test")
		status, body, _ = call(t, server, "GET", "/api/v1/audit", nil, student)
		if status != 403 || body["code"] != "FORBIDDEN" {
			t.Fatalf("student GET /audit = %d %v, want 403 FORBIDDEN", status, body)
		}
		// The two provisioning calls above each logged a users.created row; an
		// admin read returns 200 with a paginated envelope.
		status, body, _ = call(t, server, "GET", "/api/v1/audit", nil, admin)
		if status != 200 {
			t.Fatalf("admin GET /audit = %d %v, want 200", status, body)
		}
		if _, ok := body["entries"].([]any); !ok {
			t.Fatalf("admin GET /audit body = %v, want an entries array", body)
		}
		if _, ok := body["next_cursor"]; !ok {
			t.Fatalf("admin GET /audit body = %v, want a next_cursor key", body)
		}
	})

	otherActor := "11111111-1111-1111-1111-111111111111"
	resource := "22222222-2222-2222-2222-222222222222"

	t.Run("filters isolate by actor, action, resource, and NULLs scan cleanly", func(t *testing.T) {
		// A row with NULL actor_id and NULL detail - the scan trap.
		seedAudit(t, sqlDB, ctx, nil, "system.tick", "system", nil, false, time.Now())
		// Rows filterable by action/resource_type/resource_id/actor_id.
		seedAudit(t, sqlDB, ctx, &otherActor, "audit.probe", "widget", &resource, true, time.Now())
		seedAudit(t, sqlDB, ctx, &adminID, "audit.probe", "widget", &resource, true, time.Now())

		// The NULL-detail, NULL-actor row must come back without a 500 and
		// with detail serialized as JSON null and actor_id omitted.
		status, body, _ := call(t, server, "GET", "/api/v1/audit?action=system.tick", nil, admin)
		if status != 200 {
			t.Fatalf("filter action=system.tick = %d %v, want 200", status, body)
		}
		entries := body["entries"].([]any)
		if len(entries) != 1 {
			t.Fatalf("system.tick entries = %d, want 1", len(entries))
		}
		e := entries[0].(map[string]any)
		if _, hasActor := e["actor_id"]; hasActor {
			t.Fatalf("NULL actor_id leaked into response: %v", e)
		}
		if e["detail"] != nil {
			t.Fatalf("NULL detail = %v, want JSON null", e["detail"])
		}

		// action=audit.probe returns exactly the two probe rows.
		_, body, _ = call(t, server, "GET", "/api/v1/audit?action=audit.probe", nil, admin)
		if n := len(body["entries"].([]any)); n != 2 {
			t.Fatalf("action=audit.probe entries = %d, want 2", n)
		}
		// actor_id narrows it to the one probe by the other actor.
		_, body, _ = call(t, server, "GET", "/api/v1/audit?action=audit.probe&actor_id="+otherActor, nil, admin)
		entries = body["entries"].([]any)
		if len(entries) != 1 || entries[0].(map[string]any)["actor_id"] != otherActor {
			t.Fatalf("actor_id filter = %v, want the single otherActor probe", entries)
		}
		// resource_type + resource_id both match the probe rows.
		_, body, _ = call(t, server, "GET", "/api/v1/audit?resource_type=widget&resource_id="+resource, nil, admin)
		if n := len(body["entries"].([]any)); n != 2 {
			t.Fatalf("resource filter entries = %d, want 2", n)
		}
	})

	t.Run("from/to bound the time range", func(t *testing.T) {
		base := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
		for i := 0; i < 3; i++ {
			seedAudit(t, sqlDB, ctx, &adminID, "range.probe", "clock", nil, true, base.Add(time.Duration(i)*time.Hour))
		}
		// [12:30, 14:30) captures the 13:00 and 14:00 rows, not the 12:00 one.
		from := base.Add(30 * time.Minute).Format(time.RFC3339)
		to := base.Add(150 * time.Minute).Format(time.RFC3339)
		_, body, _ := call(t, server, "GET",
			"/api/v1/audit?action=range.probe&from="+from+"&to="+to, nil, admin)
		if n := len(body["entries"].([]any)); n != 2 {
			t.Fatalf("range filter entries = %d, want 2", n)
		}
	})

	t.Run("keyset pagination walks pages newest-first with no overlap", func(t *testing.T) {
		const total = 5
		for i := 0; i < total; i++ {
			seedAudit(t, sqlDB, ctx, &adminID, "page.probe", "pager", nil, true, time.Now())
		}
		seen := map[float64]bool{}
		var lastID float64 = 1 << 62 // strictly descending guard
		before := ""
		pages := 0
		for {
			path := "/api/v1/audit?action=page.probe&limit=2"
			if before != "" {
				path += "&before=" + before
			}
			status, body, _ := call(t, server, "GET", path, nil, admin)
			if status != 200 {
				t.Fatalf("page fetch = %d %v, want 200", status, body)
			}
			entries := body["entries"].([]any)
			for _, raw := range entries {
				id := raw.(map[string]any)["id"].(float64)
				if seen[id] {
					t.Fatalf("id %v appeared on two pages", id)
				}
				if id >= lastID {
					t.Fatalf("id %v not strictly below previous %v - order broke", id, lastID)
				}
				seen[id] = true
				lastID = id
			}
			pages++
			if body["next_cursor"] == nil {
				if len(entries) == 0 && pages == 1 {
					t.Fatal("first page was empty")
				}
				break
			}
			before = fmt.Sprintf("%.0f", body["next_cursor"].(float64))
			if pages > 10 {
				t.Fatal("pagination did not terminate")
			}
		}
		if len(seen) != total {
			t.Fatalf("paged over %d distinct rows, want %d", len(seen), total)
		}
	})

	t.Run("malformed filters are 422, never a 500 or a silent default", func(t *testing.T) {
		for _, bad := range []string{
			"actor_id=not-a-uuid",
			"resource_id=nope",
			"from=yesterday",
			"to=soon",
			"limit=0",
			"limit=201",
			"limit=abc",
			"before=-1",
			"before=xyz",
		} {
			status, body, _ := call(t, server, "GET", "/api/v1/audit?"+bad, nil, admin)
			if status != 422 || body["code"] != "VALIDATION_FAILED" {
				t.Fatalf("GET /audit?%s = %d %v, want 422 VALIDATION_FAILED", bad, status, body)
			}
		}
	})
}

// seedAudit inserts one audit_log row directly with full control over the
// nullable columns and the timestamp, so the read tests are deterministic.
func seedAudit(t *testing.T, sqlDB *sql.DB, ctx context.Context,
	actorID *string, action, resourceType string, resourceID *string, withDetail bool, at time.Time) {
	t.Helper()
	var detail any
	if withDetail {
		detail = []byte(`{"k":"v"}`)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO audit_log (actor_id, action, resource_type, resource_id, detail, at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		actorID, action, resourceType, resourceID, detail, at); err != nil {
		t.Fatalf("seed audit row: %v", err)
	}
}

// provisionAndLogin creates a role account, walks it through the forced
// first-login reset, and returns cookies for a fully authenticated session -
// the state needed to prove the audit gate refuses non-admins with 403 (not
// PASSWORD_CHANGE_REQUIRED).
func provisionAndLogin(t *testing.T, server *httptest.Server, admin map[string]string, role, email string) map[string]string {
	t.Helper()
	status, body, _ := call(t, server, "POST", "/api/v1/users",
		map[string]string{"role": role, "email": email, "full_name": "Test " + role}, admin)
	if status != 201 {
		t.Fatalf("provision %s = %d %v, want 201", role, status, body)
	}
	oneTime := body["initial_password"].(string)
	_, _, cookies := call(t, server, "POST", "/api/v1/auth/login",
		map[string]string{"email": email, "password": oneTime}, nil)
	status, _, _ = call(t, server, "POST", "/api/v1/auth/password",
		map[string]string{"current_password": oneTime, "new_password": "reset-owns-this-1"}, cookies)
	if status != 204 {
		t.Fatalf("%s reset = %d, want 204", role, status)
	}
	_, _, cookies = call(t, server, "POST", "/api/v1/auth/login",
		map[string]string{"email": email, "password": "reset-owns-this-1"}, nil)
	return cookies
}
