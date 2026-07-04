package authusers_test

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
)

// TestAdminProvisioningE2E drives the rest of Milestone 1 over real HTTP and
// a real Postgres: POST/PATCH /users with generated first-login credentials,
// groups and membership, the policy gate on every admin route, and the audit
// trail. It runs in its own database (macquiz_admintest) so it can never
// race the other DB-backed tests when `go test ./...` runs packages in
// parallel.
func TestAdminProvisioningE2E(t *testing.T) {
	baseURL := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	sqlDB := freshDatabase(t, ctx, baseURL, "macquiz_admintest")
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

	t.Run("routes are gated", func(t *testing.T) {
		status, body, _ := call(t, server, "GET", "/api/v1/users", nil, nil)
		if status != 401 || body["code"] != "UNAUTHENTICATED" {
			t.Fatalf("unauthenticated GET /users = %d %v, want 401", status, body)
		}
	})

	var teacher map[string]any
	var teacherPassword string
	t.Run("provision teacher", func(t *testing.T) {
		status, body, _ := call(t, server, "POST", "/api/v1/users",
			map[string]string{"role": "admin", "email": "x@school.test", "full_name": "X"}, admin)
		if status != 422 {
			t.Fatalf("provision admin role = %d %v, want 422", status, body)
		}

		status, body, _ = call(t, server, "POST", "/api/v1/users",
			map[string]string{"role": "teacher", "email": "teach@school.test", "full_name": "Terry Teacher"}, admin)
		if status != 201 {
			t.Fatalf("provision teacher = %d %v, want 201", status, body)
		}
		teacher = body["user"].(map[string]any)
		teacherPassword, _ = body["initial_password"].(string)
		if teacherPassword == "" {
			t.Fatal("provisioning did not return the one-time initial_password")
		}
		if teacher["must_change_password"] != true || teacher["role"] != "teacher" {
			t.Fatalf("provisioned teacher = %v, want must_change_password=true role=teacher", teacher)
		}

		status, body, _ = call(t, server, "POST", "/api/v1/users",
			map[string]string{"role": "teacher", "email": "teach@school.test", "full_name": "Dupe"}, admin)
		if status != 422 {
			t.Fatalf("duplicate email = %d %v, want 422", status, body)
		}
	})

	t.Run("provisioned credential works once then forces reset", func(t *testing.T) {
		status, _, cookies := call(t, server, "POST", "/api/v1/auth/login",
			map[string]string{"email": "teach@school.test", "password": teacherPassword}, nil)
		if status != 200 {
			t.Fatalf("teacher first login = %d, want 200", status)
		}
		// Module routes stay closed until the reset...
		status, body, _ := call(t, server, "GET", "/api/v1/users", nil, cookies)
		if status != 403 || body["code"] != "PASSWORD_CHANGE_REQUIRED" {
			t.Fatalf("pre-reset GET /users = %d %v, want 403 PASSWORD_CHANGE_REQUIRED", status, body)
		}
		status, _, _ = call(t, server, "POST", "/api/v1/auth/password",
			map[string]string{"current_password": teacherPassword, "new_password": "terry-owns-this-1"}, cookies)
		if status != 204 {
			t.Fatalf("teacher reset = %d, want 204", status)
		}
		// ...and after the reset a teacher is still not an admin.
		_, _, cookies = call(t, server, "POST", "/api/v1/auth/login",
			map[string]string{"email": "teach@school.test", "password": "terry-owns-this-1"}, nil)
		status, body, _ = call(t, server, "GET", "/api/v1/users", nil, cookies)
		if status != 403 || body["code"] != "FORBIDDEN" {
			t.Fatalf("teacher GET /users = %d %v, want 403 FORBIDDEN", status, body)
		}
		status, _, _ = call(t, server, "POST", "/api/v1/groups",
			map[string]string{"name": "Sneaky"}, cookies)
		if status != 403 {
			t.Fatalf("teacher POST /groups = %d, want 403", status)
		}
	})

	var studentA, studentB string
	t.Run("provision students and list with filters", func(t *testing.T) {
		for _, s := range []struct{ email, name string }{
			{"ada@school.test", "Ada Student"},
			{"ben@school.test", "Ben Student"},
		} {
			status, body, _ := call(t, server, "POST", "/api/v1/users",
				map[string]string{"role": "student", "email": s.email, "full_name": s.name}, admin)
			if status != 201 {
				t.Fatalf("provision %s = %d %v, want 201", s.email, status, body)
			}
			id := body["user"].(map[string]any)["id"].(string)
			if studentA == "" {
				studentA = id
			} else {
				studentB = id
			}
		}
		status, body, _ := call(t, server, "GET", "/api/v1/users?role=student", nil, admin)
		if status != 200 || len(body["users"].([]any)) != 2 {
			t.Fatalf("GET /users?role=student = %d %v, want 200 with 2 users", status, body)
		}
		status, body, _ = call(t, server, "GET", "/api/v1/users?role=wizard", nil, admin)
		if status != 422 {
			t.Fatalf("GET /users?role=wizard = %d %v, want 422", status, body)
		}
	})

	t.Run("patch user", func(t *testing.T) {
		teacherID := teacher["id"].(string)
		status, body, _ := call(t, server, "PATCH", "/api/v1/users/"+teacherID,
			map[string]any{"full_name": "Terry Renamed"}, admin)
		if status != 200 || body["user"].(map[string]any)["full_name"] != "Terry Renamed" {
			t.Fatalf("rename = %d %v, want 200 with new name", status, body)
		}

		// Disabling revokes live sessions: the teacher's token dies now, not
		// at expiry.
		_, _, teacherCookies := call(t, server, "POST", "/api/v1/auth/login",
			map[string]string{"email": "teach@school.test", "password": "terry-owns-this-1"}, nil)
		status, _, _ = call(t, server, "PATCH", "/api/v1/users/"+teacherID,
			map[string]any{"status": "disabled"}, admin)
		if status != 200 {
			t.Fatalf("disable = %d, want 200", status)
		}
		status, _, _ = call(t, server, "GET", "/api/v1/auth/me", nil, teacherCookies)
		if status != 401 {
			t.Fatalf("disabled teacher /me = %d, want 401", status)
		}
		status, _, _ = call(t, server, "POST", "/api/v1/auth/login",
			map[string]string{"email": "teach@school.test", "password": "terry-owns-this-1"}, nil)
		if status != 401 {
			t.Fatalf("disabled teacher login = %d, want 401", status)
		}

		// Credential reset issues a fresh one-time password.
		status, body, _ = call(t, server, "PATCH", "/api/v1/users/"+studentA,
			map[string]any{"reset_password": true}, admin)
		if status != 200 {
			t.Fatalf("reset password = %d %v, want 200", status, body)
		}
		newPassword, _ := body["initial_password"].(string)
		if newPassword == "" || body["user"].(map[string]any)["must_change_password"] != true {
			t.Fatalf("reset response = %v, want initial_password and must_change_password=true", body)
		}
		status, _, _ = call(t, server, "POST", "/api/v1/auth/login",
			map[string]string{"email": "ada@school.test", "password": newPassword}, nil)
		if status != 200 {
			t.Fatalf("login with reset credential = %d, want 200", status)
		}

		// Self-lockout guard and 404s.
		status, _, _ = call(t, server, "PATCH", "/api/v1/users/"+adminID,
			map[string]any{"status": "disabled"}, admin)
		if status != 422 {
			t.Fatalf("self-disable = %d, want 422", status)
		}
		status, _, _ = call(t, server, "PATCH", "/api/v1/users/00000000-0000-0000-0000-000000000000",
			map[string]any{"full_name": "Ghost"}, admin)
		if status != 404 {
			t.Fatalf("patch unknown user = %d, want 404", status)
		}
		status, _, _ = call(t, server, "PATCH", "/api/v1/users/not-a-uuid",
			map[string]any{"full_name": "Ghost"}, admin)
		if status != 404 {
			t.Fatalf("patch garbage id = %d, want 404", status)
		}
	})

	t.Run("groups and membership", func(t *testing.T) {
		status, body, _ := call(t, server, "POST", "/api/v1/groups",
			map[string]string{"name": "Class 10-B"}, admin)
		if status != 201 {
			t.Fatalf("create group = %d %v, want 201", status, body)
		}
		groupID := body["group"].(map[string]any)["id"].(string)

		status, body, _ = call(t, server, "PUT", "/api/v1/groups/"+groupID+"/members",
			map[string]any{"student_ids": []string{studentA, studentB}}, admin)
		if status != 200 || body["group"].(map[string]any)["member_count"] != float64(2) {
			t.Fatalf("set members = %d %v, want 200 with member_count=2", status, body)
		}

		// A teacher id in the list rejects the whole set atomically.
		status, _, _ = call(t, server, "PUT", "/api/v1/groups/"+groupID+"/members",
			map[string]any{"student_ids": []string{studentA, teacher["id"].(string)}}, admin)
		if status != 422 {
			t.Fatalf("non-student member = %d, want 422", status)
		}
		status, body, _ = call(t, server, "GET", "/api/v1/groups", nil, admin)
		if status != 200 {
			t.Fatalf("list groups = %d, want 200", status)
		}
		groups := body["groups"].([]any)
		if len(groups) != 1 || groups[0].(map[string]any)["member_count"] != float64(2) {
			t.Fatalf("groups after failed swap = %v, want the original 2 members intact", groups)
		}

		// Empty list clears the group.
		status, body, _ = call(t, server, "PUT", "/api/v1/groups/"+groupID+"/members",
			map[string]any{"student_ids": []string{}}, admin)
		if status != 200 || body["group"].(map[string]any)["member_count"] != float64(0) {
			t.Fatalf("clear members = %d %v, want 200 with member_count=0", status, body)
		}
		status, _, _ = call(t, server, "PUT", "/api/v1/groups/00000000-0000-0000-0000-000000000000/members",
			map[string]any{"student_ids": []string{}}, admin)
		if status != 404 {
			t.Fatalf("members of unknown group = %d, want 404", status)
		}
	})

	t.Run("audit trail", func(t *testing.T) {
		for action, want := range map[string]int{
			"users.created":      3, // teacher + two students
			"users.updated":      3, // rename, disable, reset
			"groups.created":     1,
			"groups.members_set": 2, // set two, clear (failed swap rolled back)
		} {
			var n int
			if err := sqlDB.QueryRowContext(ctx,
				`SELECT count(*) FROM audit_log WHERE action = $1`, action).Scan(&n); err != nil || n != want {
				t.Fatalf("audit rows for %s = %d (err %v), want %d", action, n, err, want)
			}
		}
	})
}
