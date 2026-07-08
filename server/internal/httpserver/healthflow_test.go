package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"macquiz/server/internal/db"
	"macquiz/server/internal/itest"
)

// fakeRedis lets tests control the Redis half of the /healthz check without a
// real Redis instance.
type fakeRedis struct{ err error }

func (f fakeRedis) Ping(ctx context.Context) error { return f.err }

// TestHealthzDependencyChecks exercises docs/10-operations.md section 2's
// requirement ("/healthz checks DB connectivity, Redis connectivity, and
// queue depth") against a real Postgres: a healthy DB with a queue backlog
// reports queue_lag_seconds, a failing Redis flips the response to 503, and a
// closed DB does the same.
func TestHealthzDependencyChecks(t *testing.T) {
	url := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, url, "macquiz_healthz_test")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	t.Run("db and redis healthy, no queue backlog", func(t *testing.T) {
		h := New(BuildInfo{Version: "test"}, Deps{DB: sqlDB, Redis: fakeRedis{}})
		srv := httptest.NewServer(h)
		defer srv.Close()

		status, body := getRaw(t, srv, "/healthz")
		if status != 200 {
			t.Fatalf("status = %d, want 200: %v", status, body)
		}
		if body.Status != "ok" || body.Checks.Database != "ok" || body.Checks.Redis != "ok" {
			t.Fatalf("unexpected body: %+v", body)
		}
		if body.Checks.QueueLagSeconds == nil || *body.Checks.QueueLagSeconds != 0 {
			t.Fatalf("queue_lag_seconds = %v, want 0", body.Checks.QueueLagSeconds)
		}
	})

	t.Run("overdue job reported as queue lag", func(t *testing.T) {
		if _, err := sqlDB.ExecContext(ctx, `
			INSERT INTO river_job (state, queue, priority, args, kind, scheduled_at, max_attempts)
			VALUES ('available', 'default', 1, '{}', 'test_kind', NOW() - INTERVAL '30 seconds', 1)
		`); err != nil {
			t.Fatalf("insert overdue river_job: %v", err)
		}

		h := New(BuildInfo{Version: "test"}, Deps{DB: sqlDB, Redis: fakeRedis{}})
		srv := httptest.NewServer(h)
		defer srv.Close()

		status, body := getRaw(t, srv, "/healthz")
		if status != 200 {
			t.Fatalf("status = %d, want 200: %v", status, body)
		}
		if body.Checks.QueueLagSeconds == nil || *body.Checks.QueueLagSeconds < 25 {
			t.Fatalf("queue_lag_seconds = %v, want >= 25", body.Checks.QueueLagSeconds)
		}
	})

	t.Run("redis down flips to 503", func(t *testing.T) {
		h := New(BuildInfo{Version: "test"}, Deps{DB: sqlDB, Redis: fakeRedis{err: errors.New("connection refused")}})
		srv := httptest.NewServer(h)
		defer srv.Close()

		status, body := getRaw(t, srv, "/healthz")
		if status != 503 {
			t.Fatalf("status = %d, want 503: %v", status, body)
		}
		if body.Status != "error" || body.Checks.Database != "ok" {
			t.Fatalf("unexpected body: %+v", body)
		}
	})

	t.Run("db down flips to 503", func(t *testing.T) {
		dead, err := db.Open(ctx, url)
		if err != nil {
			t.Fatalf("open second connection: %v", err)
		}
		dead.Close()

		h := New(BuildInfo{Version: "test"}, Deps{DB: dead, Redis: fakeRedis{}})
		srv := httptest.NewServer(h)
		defer srv.Close()

		status, body := getRaw(t, srv, "/healthz")
		if status != 503 {
			t.Fatalf("status = %d, want 503: %v", status, body)
		}
		if body.Status != "error" || body.Checks.Redis != "ok" {
			t.Fatalf("unexpected body: %+v", body)
		}
	})
}

func getRaw(t *testing.T, srv *httptest.Server, path string) (int, healthResponse) {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + path)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	var body healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return resp.StatusCode, body
}
