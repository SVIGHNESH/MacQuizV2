// Package httpserver wires the chi router, cross-cutting middleware, and
// module route mounting for the MacQuiz API.
//
// Modules (authusers, quiz, attempt, analytics, realtime) each expose a
// Routes() http.Handler that gets mounted here; httpserver itself contains
// no business logic.
package httpserver

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// BuildInfo identifies the running binary in health responses and logs.
type BuildInfo struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// New returns the root HTTP handler for the API process.
func New(build BuildInfo) http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RealIP)
	r.Use(middleware.RequestID)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(30 * time.Second))

	r.Get("/healthz", handleHealth(build))

	// Module routes are mounted under /api/v1 as milestones land:
	//   authusers -> /api/v1/auth, /api/v1/users, /api/v1/groups
	//   quiz      -> /api/v1/quizzes, /api/v1/imports
	//   attempt   -> /api/v1/attempts
	//   analytics -> /api/v1/analytics
	//   realtime  -> /ws

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
// not whether Postgres is. Readiness (dependency checks) comes with the
// database wiring in Milestone 1.
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
