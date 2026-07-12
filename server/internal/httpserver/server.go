// Package httpserver wires the chi router, cross-cutting middleware, and
// module route mounting for the MacQuiz API.
//
// Modules (authusers, quiz, attempt, analytics, realtime) each expose a
// Routes() http.Handler that gets mounted here; httpserver itself contains
// no business logic.
package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"macquiz/server/internal/analytics"
	"macquiz/server/internal/attempt"
	"macquiz/server/internal/authusers"
	"macquiz/server/internal/quiz"
	"macquiz/server/internal/realtime"
)

// BuildInfo identifies the running binary in health responses and logs.
type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// RedisPinger checks Redis reachability for the /healthz dependency check.
// realtime.Publisher satisfies this without httpserver importing go-redis
// directly.
type RedisPinger interface {
	Ping(ctx context.Context) error
}

// Deps carries the wired modules into the router. Fields are nil in unit
// tests that only exercise the router shell.
type Deps struct {
	DB        *sql.DB
	Redis     RedisPinger
	Auth      *authusers.Handler
	Quiz      *quiz.Handler
	Attempt   *attempt.Handler
	Analytics *analytics.Handler
	Realtime  *realtime.Gateway
	// QueueLagMaxSec is the /healthz queue-depth ceiling in seconds
	// (config.HealthQueueLagMaxSec). 0 - the zero value, so router-shell
	// tests keep their old behaviour - leaves queue lag informational.
	QueueLagMaxSec int
}

// New returns the root HTTP handler for the API process.
func New(build BuildInfo, deps Deps) http.Handler {
	r := chi.NewRouter()

	// Cross-cutting middleware every surface shares. Timeout is deliberately
	// NOT here: it belongs only on the REST surface (see the group below). A
	// request-scoped deadline is wrong for a long-lived WebSocket - chi's
	// Timeout also writes a 504 once the handler returns, which on a hijacked
	// socket is a "WriteHeader on hijacked connection" error on every close.
	r.Use(clientIP)
	r.Use(middleware.RequestID)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// The realtime gateway mounts at /ws, outside /api/v1 and outside the REST
	// Timeout: it is a long-lived WebSocket surface, not a REST resource. Its
	// handler detaches the socket lifetime from the request context, and the
	// gateway's base context closes every socket on shutdown (docs/05 section 3).
	if deps.Realtime != nil {
		r.Mount("/ws", deps.Realtime.Routes())
	}

	// The REST surface: everything below gets the 30 s request timeout.
	r.Group(func(r chi.Router) {
		r.Use(middleware.Timeout(30 * time.Second))

		r.Get("/healthz", handleHealth(build, deps.DB, deps.Redis, deps.QueueLagMaxSec))
		r.Get("/readyz", handleReady(deps.DB))
		r.Get("/deploy-check", handleDeployCheck(deps.DB))

		// Module routes are mounted under /api/v1 as milestones land:
		//   authusers -> /api/v1/auth, /api/v1/users, /api/v1/groups
		//   quiz      -> /api/v1/quizzes, /api/v1/imports
		//   attempt   -> /api/v1/attempts
		//   analytics -> /api/v1/analytics
		//   realtime  -> /ws
		if deps.Auth != nil {
			r.Route("/api/v1", func(r chi.Router) {
				r.Mount("/auth", deps.Auth.Routes())
				r.Mount("/users", deps.Auth.UserRoutes())
				r.Mount("/groups", deps.Auth.GroupRoutes())
				r.Mount("/directory", deps.Auth.DirectoryRoutes())
				r.Mount("/audit", deps.Auth.AuditRoutes())
				if deps.Quiz != nil {
					// POST /quizzes/{id}/attempts belongs to the quiz mount's
					// subtree; the handler itself stays in the attempt module.
					if deps.Attempt != nil {
						deps.Quiz.AttachAttemptStart(deps.Attempt.HandleStart)
					}
					r.Mount("/quizzes", deps.Quiz.QuizRoutes())
					r.Mount("/questions", deps.Quiz.QuestionRoutes())
					r.Mount("/imports", deps.Quiz.ImportRoutes())
				}
				if deps.Attempt != nil {
					r.Mount("/attempts", deps.Attempt.Routes())
				}
				if deps.Analytics != nil {
					r.Mount("/analytics", deps.Analytics.Routes())
				}
			})
		}
	})

	return r
}

// clientIP overwrites r.RemoteAddr with the caller's real IP (port
// stripped), so anything keying on RemoteAddr - today, the per-IP login
// rate limiter (docs/08-security.md section 4) - gets one stable value per
// client instead of a fresh key per TCP connection.
//
// chi's own middleware.RealIP is deprecated: it trusts the leftmost
// X-Forwarded-For entry, which the client sets and can freely rewrite on
// every request, letting anyone bypass the login rate limit at will
// (GHSA-3fxj-6jh8-hvhx). Production sits behind Cloudflare
// (docs/09-deployment.md section 4), which always overwrites
// CF-Connecting-IP on every request it proxies - a client cannot forge it
// once traffic reaches Cloudflare's edge. Local/dev has no proxy in front
// (docker-compose hits the app directly), so this falls back to the bare
// TCP peer address in that case.
func clientIP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := r.Header.Get("CF-Connecting-IP")
		if ip == "" {
			if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
				ip = host
			} else {
				ip = r.RemoteAddr
			}
		}
		if addr, err := netip.ParseAddr(ip); err == nil {
			r.RemoteAddr = addr.String()
		}
		next.ServeHTTP(w, r)
	})
}

type healthResponse struct {
	Status  string       `json:"status"`
	Version string       `json:"version"`
	Commit  string       `json:"commit"`
	Time    time.Time    `json:"time"`
	Checks  healthChecks `json:"checks"`
}

type healthChecks struct {
	Database        string   `json:"database,omitempty"`
	Redis           string   `json:"redis,omitempty"`
	QueueLagSeconds *float64 `json:"queue_lag_seconds,omitempty"`
}

// healthCheckTimeout bounds each dependency probe below so a partitioned
// Postgres or Redis fails the check fast instead of hanging the request until
// the REST group's 30 s timeout middleware fires.
const healthCheckTimeout = 2 * time.Second

// handleHealth is the dependency check docs/10-operations.md section 2
// requires: "/healthz checks DB connectivity, Redis connectivity, and queue
// depth" - UptimeRobot pings this from outside every 5 min. A dependency that
// is not wired (nil, as in router-shell-only tests) is skipped rather than
// treated as down. Any wired-but-failing dependency flips the response to 503
// so external monitoring alerts on it.
//
// queueLagMaxSec is the queue-depth half of that check: a backlog older than
// it means the worker is not draining, which puts deadline timers and
// auto-submits at risk, so it fails the endpoint the same way an unreachable
// dependency does. 0 disables the gate (queue lag stays informational).
func handleHealth(build BuildInfo, db *sql.DB, redis RedisPinger, queueLagMaxSec int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
		defer cancel()

		healthy := true
		checks := healthChecks{}

		if db != nil {
			if err := db.PingContext(ctx); err != nil {
				checks.Database = "error: " + err.Error()
				healthy = false
			} else {
				checks.Database = "ok"
				// The queue-depth check. A query failure here is not itself a
				// liveness condition - it just omits the field - but a real
				// backlog past the configured ceiling is: the queue carries
				// the deadline timers and auto-submits a live quiz depends on.
				if lag, err := QueueLagSeconds(ctx, db); err == nil {
					checks.QueueLagSeconds = &lag
					if queueLagMaxSec > 0 && lag > float64(queueLagMaxSec) {
						healthy = false
					}
				}
			}
		}

		if redis != nil {
			if err := redis.Ping(ctx); err != nil {
				checks.Redis = "error: " + err.Error()
				healthy = false
			} else {
				checks.Redis = "ok"
			}
		}

		status := "ok"
		if !healthy {
			status = "error"
			w.WriteHeader(http.StatusServiceUnavailable)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(healthResponse{
			Status:  status,
			Version: build.Version,
			Commit:  build.Commit,
			Time:    time.Now().UTC(),
			Checks:  checks,
		})
	}
}

// QueueLagSeconds reports how overdue the oldest due-but-unfired River job
// is (docs/10-operations.md's "queue lag (delayed jobs overdue)" alert
// signal): the age of the oldest job still available/scheduled at or before
// now, or 0 if the queue has no backlog.
func QueueLagSeconds(ctx context.Context, db *sql.DB) (float64, error) {
	var lag float64
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(EXTRACT(EPOCH FROM (NOW() - MIN(scheduled_at))), 0)
		FROM river_job
		WHERE state IN ('available', 'scheduled') AND scheduled_at <= NOW()
	`).Scan(&lag)
	return lag, err
}

// handleReady reports readiness: the process can serve real traffic, which
// means Postgres answers. 503 tells the orchestrator to keep traffic away.
func handleReady(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if db == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "no database"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "database unreachable"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
	}
}

type deployCheckResponse struct {
	SafeToDeploy  bool   `json:"safe_to_deploy"`
	LiveQuizCount int    `json:"live_quiz_count"`
	Reason        string `json:"reason,omitempty"`
}

// handleDeployCheck backs docs/10-operations.md section 4's deploy policy:
// "Deploys are refused while any quiz is live (pre-deploy check)". The
// GitHub Actions deploy workflow curls this before rolling out a new image
// and aborts if safe_to_deploy is false - the cheapest possible prevention
// of shipping a rollout in the middle of a live exam.
//
// "Live" here means effectively live, matching the lazy status derivation
// the rest of the app uses (docs/06): a quiz still marked 'scheduled' in the
// row but whose starts_at has already passed counts as live even before the
// scheduler job flips the column, so a deploy landing in that narrow window
// is refused too.
func handleDeployCheck(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if db == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(deployCheckResponse{Reason: "no database"})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), healthCheckTimeout)
		defer cancel()

		var count int
		err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM quizzes
			WHERE (status = 'live' AND (ends_at IS NULL OR ends_at > now()))
			   OR (status = 'scheduled' AND starts_at <= now() AND (ends_at IS NULL OR ends_at > now()))
		`).Scan(&count)
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(deployCheckResponse{Reason: "database unreachable"})
			return
		}

		resp := deployCheckResponse{SafeToDeploy: count == 0, LiveQuizCount: count}
		if count > 0 {
			resp.Reason = "quizzes are live"
			w.WriteHeader(http.StatusConflict)
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}
