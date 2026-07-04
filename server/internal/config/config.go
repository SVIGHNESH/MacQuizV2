// Package config loads process configuration from the environment.
//
// Every knob has a development default so `go run ./cmd/macquiz serve`
// works against the Compose dev stack with no setup.
package config

import (
	"os"
	"time"
)

// Config holds the runtime configuration shared by the API server and
// the worker process (both live in the same binary; see cmd/macquiz).
type Config struct {
	// Addr is the listen address of the HTTP API, e.g. ":8080".
	Addr string
	// DatabaseURL is the PostgreSQL connection string.
	DatabaseURL string
	// RedisURL is the Redis connection string.
	RedisURL string
	// ShutdownGrace is how long in-flight requests get to finish on SIGTERM.
	ShutdownGrace time.Duration
	// Env is "development" or "production"; controls log format and defaults.
	Env string
	// AuthSecret signs JWT access tokens (HS256). The development default
	// works with the Compose stack; production must set its own.
	AuthSecret string
	// Bootstrap* seed the first admin account via `macquiz bootstrap`.
	BootstrapAdminEmail    string
	BootstrapAdminPassword string
	BootstrapAdminName     string
}

// Load reads configuration from the environment, applying development defaults.
func Load() Config {
	return Config{
		Addr: getenv("MACQUIZ_ADDR", ":8080"),
		// Dev defaults match the host-port mappings in docker-compose.yml.
		DatabaseURL:   getenv("MACQUIZ_DATABASE_URL", "postgres://macquiz:macquiz@localhost:5433/macquiz?sslmode=disable"),
		RedisURL:      getenv("MACQUIZ_REDIS_URL", "redis://localhost:6380/0"),
		ShutdownGrace: 10 * time.Second,
		Env:           getenv("MACQUIZ_ENV", "development"),
		AuthSecret:    getenv("MACQUIZ_AUTH_SECRET", "dev-only-insecure-secret"),

		BootstrapAdminEmail:    os.Getenv("MACQUIZ_BOOTSTRAP_ADMIN_EMAIL"),
		BootstrapAdminPassword: os.Getenv("MACQUIZ_BOOTSTRAP_ADMIN_PASSWORD"),
		BootstrapAdminName:     getenv("MACQUIZ_BOOTSTRAP_ADMIN_NAME", "Administrator"),
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
