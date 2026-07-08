package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/db"
	"macquiz/server/internal/itest"
)

// TestDeployCheck pins docs/10-operations.md section 4's deploy policy
// ("deploys are refused while any quiz is live") against a real Postgres:
// no quizzes -> safe, a quiz whose row status is already 'live' -> unsafe,
// and a quiz still stored as 'scheduled' but whose starts_at has already
// passed -> unsafe too (the lazy-status window docs/06 describes), because
// the sweep job that would flip the row may not have run yet.
func TestDeployCheck(t *testing.T) {
	url := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	sqlDB := itest.FreshDatabase(t, ctx, url, "macquiz_deploycheck_test")
	if _, err := db.MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	authSvc := authusers.NewService(sqlDB, "test-secret", log)
	if err := authSvc.EnsureBootstrapAdmin(ctx, "admin@school.test", "admin-password-1", "Root Admin"); err != nil {
		t.Fatalf("bootstrap admin: %v", err)
	}
	var ownerID string
	if err := sqlDB.QueryRowContext(ctx, `SELECT id FROM users WHERE email = 'admin@school.test'`).Scan(&ownerID); err != nil {
		t.Fatalf("read owner id: %v", err)
	}

	h := New(BuildInfo{Version: "test"}, Deps{DB: sqlDB})
	srv := httptest.NewServer(h)
	defer srv.Close()

	t.Run("no quizzes is safe", func(t *testing.T) {
		status, body := getDeployCheck(t, srv)
		if status != 200 {
			t.Fatalf("status = %d, want 200: %+v", status, body)
		}
		if !body.SafeToDeploy || body.LiveQuizCount != 0 {
			t.Fatalf("unexpected body: %+v", body)
		}
	})

	t.Run("a draft or closed quiz is safe", func(t *testing.T) {
		insertQuiz(t, ctx, sqlDB, ownerID, "draft", nil, nil)
		insertQuiz(t, ctx, sqlDB, ownerID, "closed", past(t, -2*time.Hour), past(t, -time.Hour))
		status, body := getDeployCheck(t, srv)
		if status != 200 {
			t.Fatalf("status = %d, want 200: %+v", status, body)
		}
		if !body.SafeToDeploy || body.LiveQuizCount != 0 {
			t.Fatalf("unexpected body: %+v", body)
		}
	})

	t.Run("a live quiz is unsafe", func(t *testing.T) {
		id := insertQuiz(t, ctx, sqlDB, ownerID, "live", past(t, -time.Hour), past(t, time.Hour))
		status, body := getDeployCheck(t, srv)
		if status != 409 {
			t.Fatalf("status = %d, want 409: %+v", status, body)
		}
		if body.SafeToDeploy || body.LiveQuizCount != 1 {
			t.Fatalf("unexpected body: %+v", body)
		}
		if _, err := sqlDB.ExecContext(ctx, `DELETE FROM quizzes WHERE id = $1`, id); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	})

	t.Run("a scheduled quiz whose window already started is unsafe", func(t *testing.T) {
		id := insertQuiz(t, ctx, sqlDB, ownerID, "scheduled", past(t, -time.Minute), past(t, time.Hour))
		status, body := getDeployCheck(t, srv)
		if status != 409 {
			t.Fatalf("status = %d, want 409: %+v", status, body)
		}
		if body.SafeToDeploy || body.LiveQuizCount != 1 {
			t.Fatalf("unexpected body: %+v", body)
		}
		if _, err := sqlDB.ExecContext(ctx, `DELETE FROM quizzes WHERE id = $1`, id); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
	})

	t.Run("a scheduled quiz whose window has not started is safe", func(t *testing.T) {
		insertQuiz(t, ctx, sqlDB, ownerID, "scheduled", past(t, time.Hour), past(t, 2*time.Hour))
		status, body := getDeployCheck(t, srv)
		if status != 200 {
			t.Fatalf("status = %d, want 200: %+v", status, body)
		}
		if !body.SafeToDeploy || body.LiveQuizCount != 0 {
			t.Fatalf("unexpected body: %+v", body)
		}
	})

	t.Run("no database is unsafe", func(t *testing.T) {
		h := New(BuildInfo{Version: "test"}, Deps{})
		srv := httptest.NewServer(h)
		defer srv.Close()
		status, body := getDeployCheck(t, srv)
		if status != 503 {
			t.Fatalf("status = %d, want 503: %+v", status, body)
		}
		if body.SafeToDeploy {
			t.Fatalf("unexpected body: %+v", body)
		}
	})
}

func insertQuiz(t *testing.T, ctx context.Context, sqlDB *sql.DB, ownerID, status string, startsAt, endsAt *time.Time) string {
	t.Helper()
	var id string
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO quizzes (owner_id, title, status, starts_at, ends_at) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		ownerID, "Deploy Check Quiz", status, startsAt, endsAt).Scan(&id); err != nil {
		t.Fatalf("insert quiz (status=%s): %v", status, err)
	}
	return id
}

func getDeployCheck(t *testing.T, srv *httptest.Server) (int, deployCheckResponse) {
	t.Helper()
	resp, err := srv.Client().Get(srv.URL + "/deploy-check")
	if err != nil {
		t.Fatalf("GET /deploy-check: %v", err)
	}
	defer resp.Body.Close()
	var body deployCheckResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return resp.StatusCode, body
}

func past(t *testing.T, d time.Duration) *time.Time {
	t.Helper()
	tm := time.Now().Add(d)
	return &tm
}
