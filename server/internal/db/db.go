// Package db owns database access and schema migrations.
//
// Migrations are plain goose SQL files embedded into the binary, so the
// deployed image migrates itself (`macquiz migrate`) with no extra tooling
// on the host. docs/03-data-model.md is the authoritative schema reference;
// every migration must keep the two in sync.
package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver used by goose
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
	"github.com/riverqueue/river/riverdriver/riverdatabasesql"
	"github.com/riverqueue/river/rivermigrate"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Open connects to Postgres via pgx's database/sql adapter and verifies the
// connection with a ping. maxOpenConns bounds the pool (0 leaves
// database/sql's default of unlimited, fine for a one-shot command that
// opens a handful of connections and exits); the long-running serve/worker
// processes must pass a real bound (docs/01 "go-live herd": found by the
// go-live-herd load test - with no cap, a concurrent request spike opens one
// Postgres connection per in-flight request and can blow straight through
// Postgres's own max_connections, failing every request mid-storm with
// "sorry, too many clients already" instead of queuing briefly on a small
// reused pool).
func Open(ctx context.Context, url string, maxOpenConns int) (*sql.DB, error) {
	sqlDB, err := sql.Open("pgx", url)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if maxOpenConns > 0 {
		sqlDB.SetMaxOpenConns(maxOpenConns)
		sqlDB.SetMaxIdleConns(maxOpenConns)
		sqlDB.SetConnMaxLifetime(30 * time.Minute)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return sqlDB, nil
}

// newProvider builds a goose provider over the embedded migrations. A
// Postgres session (advisory) lock serializes concurrent migrators, so two
// instances racing at deploy time cannot interleave DDL.
func newProvider(sqlDB *sql.DB) (*goose.Provider, error) {
	fsys, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("sub migrations fs: %w", err)
	}
	locker, err := lock.NewPostgresSessionLocker()
	if err != nil {
		return nil, fmt.Errorf("new session locker: %w", err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, fsys,
		goose.WithSessionLocker(locker))
	if err != nil {
		return nil, fmt.Errorf("new goose provider: %w", err)
	}
	return provider, nil
}

// MigrateUp applies all pending migrations and returns how many were
// applied. River's queue tables (river_job and friends) are versioned by
// River itself, not by goose, so its migrator runs alongside - the one
// `macquiz migrate` entrypoint keeps every schema at head.
func MigrateUp(ctx context.Context, sqlDB *sql.DB) (int, error) {
	provider, err := newProvider(sqlDB)
	if err != nil {
		return 0, err
	}
	results, err := provider.Up(ctx)
	if err != nil {
		return 0, fmt.Errorf("migrate up: %w", err)
	}

	riverMigrator, err := rivermigrate.New(riverdatabasesql.New(sqlDB), nil)
	if err != nil {
		return 0, fmt.Errorf("new river migrator: %w", err)
	}
	riverResults, err := riverMigrator.Migrate(ctx, rivermigrate.DirectionUp, &rivermigrate.MigrateOpts{})
	if err != nil {
		return 0, fmt.Errorf("migrate river up: %w", err)
	}
	return len(results) + len(riverResults.Versions), nil
}

// MigrateDownTo rolls back migrations down to (and not including) version.
// Used by tests to prove every Down section actually reverses its Up.
func MigrateDownTo(ctx context.Context, sqlDB *sql.DB, version int64) (int, error) {
	provider, err := newProvider(sqlDB)
	if err != nil {
		return 0, err
	}
	results, err := provider.DownTo(ctx, version)
	if err != nil {
		return 0, fmt.Errorf("migrate down to %d: %w", version, err)
	}
	return len(results), nil
}
