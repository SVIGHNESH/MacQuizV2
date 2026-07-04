package db

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestMigrations exercises the full migration lifecycle against a real
// Postgres: up from zero, schema spot checks, the append-only trigger
// invariant, then down to zero and up again to prove reversibility.
//
// Skipped unless MACQUIZ_TEST_DATABASE_URL is set (CI sets it; locally the
// Compose Postgres works: postgres://macquiz:macquiz@localhost:5433/macquiz).
func TestMigrations(t *testing.T) {
	url := os.Getenv("MACQUIZ_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("MACQUIZ_TEST_DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	sqlDB, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sqlDB.Close()

	// Start from a clean slate in case a previous run left state behind.
	if _, err := MigrateDownTo(ctx, sqlDB, 0); err != nil {
		t.Fatalf("initial down to zero: %v", err)
	}

	applied, err := MigrateUp(ctx, sqlDB)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if applied == 0 {
		t.Fatal("up applied no migrations on an empty database")
	}

	for _, table := range []string{
		"users", "groups", "group_members", "quizzes", "questions",
		"quiz_assignments", "attempts", "attempt_answers", "imports",
		"attempt_events", "audit_log", "quiz_stats", "student_stats",
	} {
		var exists bool
		err := sqlDB.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables
			 WHERE table_schema = 'public' AND table_name = $1)`, table,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if !exists {
			t.Errorf("table %s missing after migrate up", table)
		}
	}

	// Up must be a no-op when already at head.
	again, err := MigrateUp(ctx, sqlDB)
	if err != nil {
		t.Fatalf("second up: %v", err)
	}
	if again != 0 {
		t.Errorf("second up applied %d migrations, want 0", again)
	}

	// Data safety invariant 5: audit_log is append-only.
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO audit_log (action, resource_type) VALUES ('test.write', 'test')`,
	); err != nil {
		t.Fatalf("insert audit_log: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`UPDATE audit_log SET action = 'tampered' WHERE action = 'test.write'`,
	); err == nil {
		t.Error("UPDATE on audit_log succeeded, want append-only rejection")
	}
	if _, err := sqlDB.ExecContext(ctx,
		`DELETE FROM audit_log WHERE action = 'test.write'`,
	); err == nil {
		t.Error("DELETE on audit_log succeeded, want append-only rejection")
	}

	// users.email is citext: lookups are case-insensitive.
	var id string
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO users (id, role, email, password_hash, full_name, created_by)
		 VALUES ('00000000-0000-0000-0000-000000000001', 'admin',
		         'Root@Example.com', 'x', 'Bootstrap Admin',
		         '00000000-0000-0000-0000-000000000001')
		 RETURNING id`,
	).Scan(&id); err != nil {
		t.Fatalf("insert bootstrap admin: %v", err)
	}
	var found bool
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE email = 'root@example.com')`,
	).Scan(&found); err != nil {
		t.Fatalf("citext lookup: %v", err)
	}
	if !found {
		t.Error("case-insensitive email lookup failed, citext not in effect")
	}

	// Every Down must reverse its Up, and a re-Up must succeed.
	if _, err := MigrateDownTo(ctx, sqlDB, 0); err != nil {
		t.Fatalf("down to zero: %v", err)
	}
	var tables int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM information_schema.tables
		 WHERE table_schema = 'public' AND table_name <> 'goose_db_version'`,
	).Scan(&tables); err != nil {
		t.Fatalf("count tables after down: %v", err)
	}
	if tables != 0 {
		t.Errorf("%d tables remain after down to zero, want 0", tables)
	}
	if _, err := MigrateUp(ctx, sqlDB); err != nil {
		t.Fatalf("re-up after down: %v", err)
	}
}
