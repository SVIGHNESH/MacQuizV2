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

	"macquiz/server/internal/authusers"
)

// BuildInfo identifies the running binary in health responses and logs.
type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// Deps carries the wired modules into the router. Fields are nil in unit
// tests that only exercise the router shell.
type Deps struct {
	DB   *sql.DB
	Auth *authusers.Handler
}

// New returns the root HTTP handler for the API process.
func New(build BuildInfo, deps Deps) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RealIP)
	r.Use(middleware.RequestID)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
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
		})
	}

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
