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
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

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

// Deps carries the wired modules into the router. Fields are nil in unit
// tests that only exercise the router shell.
type Deps struct {
	DB       *sql.DB
	Auth     *authusers.Handler
	Quiz     *quiz.Handler
	Attempt  *attempt.Handler
	Realtime *realtime.Gateway
}

// New returns the root HTTP handler for the API process.
func New(build BuildInfo, deps Deps) http.Handler {
	r := chi.NewRouter()

	// Cross-cutting middleware every surface shares. Timeout is deliberately
	// NOT here: it belongs only on the REST surface (see the group below). A
	// request-scoped deadline is wrong for a long-lived WebSocket - chi's
	// Timeout also writes a 504 once the handler returns, which on a hijacked
	// socket is a "WriteHeader on hijacked connection" error on every close.
	r.Use(middleware.RealIP)
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

		r.Get("/healthz", handleHealth(build))
		r.Get("/readyz", handleReady(deps.DB))

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
				}
				if deps.Attempt != nil {
					r.Mount("/attempts", deps.Attempt.Routes())
				}
			})
		}
	})

	return r
}

type healthResponse struct {
	Status  string    `json:"status"`
	Version string    `json:"version"`
	Commit  string    `json:"commit"`
	Time    time.Time `json:"time"`
}

// handleHealth reports liveness. It deliberately checks no dependencies:
// Compose and the load balancer use it to decide whether the process is up,
// not whether Postgres is.
func handleHealth(build BuildInfo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(healthResponse{
			Status:  "ok",
			Version: build.Version,
			Commit:  build.Commit,
			Time:    time.Now().UTC(),
		})
	}
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
