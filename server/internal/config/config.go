// Package config loads process configuration from the environment.
//
// Every knob has a development default so `go run ./cmd/macquiz serve`
// works against the Compose dev stack with no setup.
package config

import (
	"os"
	"strings"
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
	// WSAllowedOrigins lists the browser Origin patterns permitted on the
	// realtime WebSocket handshake (coder/websocket rejects cross-origin by
	// default). Empty means same-origin only; a dashboard dev server served
	// from a different port is added here in development.
	WSAllowedOrigins []string
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
	// ImportDir is the local-disk directory bulk-upload files are written to
	// by the register-import endpoint and read back from by the import
	// validation worker (docs/07 section 2). It stands in for object storage
	// until the pre-signed-upload brick lands; a production deployment will
	// replace it with an R2-backed config. serve and worker run as separate
	// containers, so this directory must be a shared volume (docker-compose.yml).
	ImportDir string
	// OTelExporterEndpoint is the OTLP/HTTP endpoint (host:port, no scheme)
	// metrics are exported to (docs/10-operations.md section 2's Grafana
	// Cloud free tier). Empty (the dev/test default) disables telemetry
	// entirely - every instrument becomes a no-op rather than dialing out.
	OTelExporterEndpoint string
	// OTelExporterHeaders carries request headers the OTLP exporter sends on
	// every export, formatted as "key=value,key2=value2" (Grafana Cloud's
	// OTLP gateway takes an "Authorization=Basic <token>" pair here).
	OTelExporterHeaders string
	// EmailAPIKey authenticates against Resend (docs/09-deployment.md
	// section 3) for the email leg of assignment-change notifications. Empty
	// (the dev/test default) disables email delivery entirely - it degrades
	// to a no-op, never a boot failure, since the in-app user:{id}:notify
	// channel already delivers the same event.
	EmailAPIKey string
	// EmailFrom/EmailFromName set the From header on outgoing mail.
	EmailFrom     string
	EmailFromName string
}

// Load reads configuration from the environment, applying development defaults.
func Load() Config {
	return Config{
		Addr: getenv("MACQUIZ_ADDR", ":8080"),
		// Dev defaults match the host-port mappings in docker-compose.yml.
		DatabaseURL:      getenv("MACQUIZ_DATABASE_URL", "postgres://macquiz:macquiz@localhost:5433/macquiz?sslmode=disable"),
		RedisURL:         getenv("MACQUIZ_REDIS_URL", "redis://localhost:6380/0"),
		WSAllowedOrigins: splitCSV(os.Getenv("MACQUIZ_WS_ALLOWED_ORIGINS")),
		ShutdownGrace:    10 * time.Second,
		Env:              getenv("MACQUIZ_ENV", "development"),
		AuthSecret:       getenv("MACQUIZ_AUTH_SECRET", "dev-only-insecure-secret"),

		BootstrapAdminEmail:    os.Getenv("MACQUIZ_BOOTSTRAP_ADMIN_EMAIL"),
		BootstrapAdminPassword: os.Getenv("MACQUIZ_BOOTSTRAP_ADMIN_PASSWORD"),
		BootstrapAdminName:     getenv("MACQUIZ_BOOTSTRAP_ADMIN_NAME", "Administrator"),

		ImportDir: getenv("MACQUIZ_IMPORT_DIR", "/tmp/macquiz-imports"),

		OTelExporterEndpoint: os.Getenv("MACQUIZ_OTEL_EXPORTER_ENDPOINT"),
		OTelExporterHeaders:  os.Getenv("MACQUIZ_OTEL_EXPORTER_HEADERS"),

		EmailAPIKey:   os.Getenv("MACQUIZ_EMAIL_API_KEY"),
		EmailFrom:     getenv("MACQUIZ_EMAIL_FROM", "notify@macquiz.example.edu"),
		EmailFromName: getenv("MACQUIZ_EMAIL_FROM_NAME", "MacQuiz"),
	}
}

// splitCSV parses a comma-separated env value into a trimmed, non-empty list.
// An unset or blank value yields nil (same-origin only).
func splitCSV(v string) []string {
	if strings.TrimSpace(v) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
