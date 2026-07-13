// Package config loads process configuration from the environment.
//
// Every knob has a development default so `go run ./cmd/macquiz serve`
// works against the Compose dev stack with no setup.
package config

import (
	"os"
	"strconv"
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
	// DatabaseMaxConns bounds the pgx/database/sql pool serve and worker
	// each open (docs/01 "go-live herd"): with no cap a request spike opens
	// one Postgres connection per in-flight request and can exceed
	// Postgres's own max_connections, failing every request mid-spike
	// instead of queuing briefly on a small reused pool. The default (20)
	// leaves headroom under a stock max_connections=100 for both processes
	// plus migrate/bootstrap one-shots and a few admin sessions at once.
	DatabaseMaxConns int
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
	// HealthQueueLagMaxSec is the queue-lag ceiling /healthz gates on
	// (docs/10 section 2: "/healthz checks DB connectivity, Redis
	// connectivity, and queue depth"). A backlog older than this flips the
	// endpoint to 503 so external monitoring alerts on a wedged worker -
	// deadline timers and auto-submits ride on that queue. The default (60 s)
	// is docs/10 section 3's page threshold for queue lag, high enough that a
	// routine worker restart does not flap the check. 0 disables the gate and
	// keeps queue_lag_seconds purely informational.
	HealthQueueLagMaxSec int
	// AuthSecret signs JWT access tokens (HS256). The development default
	// works with the Compose stack; production must set its own.
	AuthSecret string
	// Bootstrap* seed the first admin account via `macquiz bootstrap`.
	BootstrapAdminEmail    string
	BootstrapAdminPassword string
	BootstrapAdminName     string
	// ImportDir is the local-disk directory bulk-upload files are written to
	// by the register-import endpoint and read back from by the import
	// validation worker (docs/07 section 2), used only when ImportR2Bucket
	// is unset. serve and worker run as separate containers, so this
	// directory must be a shared volume (docker-compose.yml) in that mode.
	ImportDir string
	// ImportR2Bucket, ImportR2Endpoint, ImportR2AccessKeyID, and
	// ImportR2SecretAccessKey configure the production object-storage
	// backend for bulk-import files (docs/02 section 3.5, docs/09 section
	// 4): a Cloudflare R2 bucket, addressed via its S3-compatible API.
	// ImportR2Bucket empty (the dev/test default) falls back to the
	// local-disk blob store against ImportDir instead - never a boot failure.
	ImportR2Bucket          string
	ImportR2Endpoint        string
	ImportR2AccessKeyID     string
	ImportR2SecretAccessKey string
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
		DatabaseMaxConns: getenvInt("MACQUIZ_DATABASE_MAX_CONNS", 20),
		RedisURL:         getenv("MACQUIZ_REDIS_URL", "redis://localhost:6380/0"),
		WSAllowedOrigins: splitCSV(os.Getenv("MACQUIZ_WS_ALLOWED_ORIGINS")),
		ShutdownGrace:    10 * time.Second,
		Env:              getenv("MACQUIZ_ENV", "development"),
		AuthSecret:       getenv("MACQUIZ_AUTH_SECRET", "dev-only-insecure-secret"),

		HealthQueueLagMaxSec: getenvInt("MACQUIZ_HEALTH_QUEUE_LAG_MAX_SEC", 60),

		BootstrapAdminEmail:    os.Getenv("MACQUIZ_BOOTSTRAP_ADMIN_EMAIL"),
		BootstrapAdminPassword: os.Getenv("MACQUIZ_BOOTSTRAP_ADMIN_PASSWORD"),
		BootstrapAdminName:     getenv("MACQUIZ_BOOTSTRAP_ADMIN_NAME", "Administrator"),

		ImportDir: getenv("MACQUIZ_IMPORT_DIR", "/tmp/macquiz-imports"),

		ImportR2Bucket:          os.Getenv("MACQUIZ_IMPORT_R2_BUCKET"),
		ImportR2Endpoint:        os.Getenv("MACQUIZ_IMPORT_R2_ENDPOINT"),
		ImportR2AccessKeyID:     os.Getenv("MACQUIZ_IMPORT_R2_ACCESS_KEY_ID"),
		ImportR2SecretAccessKey: os.Getenv("MACQUIZ_IMPORT_R2_SECRET_ACCESS_KEY"),

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

// getenvInt reads an integer env var, falling back (silently, same as
// getenv) on either an unset value or one that fails to parse.
func getenvInt(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
