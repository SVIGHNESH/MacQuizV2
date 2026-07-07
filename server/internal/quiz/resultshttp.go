package quiz

import (
	"errors"
	"net/http"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/httpapi"
)

// handleReleaseResults serves POST /quizzes/{id}/release-results (owner).
// Idempotent: a second release returns the same released quiz.
func (h *Handler) handleReleaseResults(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "no such quiz")
	if !ok {
		return
	}
	q, err := h.svc.ReleaseResults(r.Context(), actor, id)
	if err != nil {
		if errors.Is(err, ErrQuizNotClosed) {
			httpapi.WriteError(w, http.StatusConflict, httpapi.CodeQuizNotClosed,
				"results can be released once the quiz has closed")
			return
		}
		h.writeQuizError(w, "release results", err, "no such quiz")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"quiz": q})
}

// handleResults serves GET /quizzes/{id}/results (owner): the per-student
// attempt/score table the teacher reads before deciding to release.
func (h *Handler) handleResults(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "no such quiz")
	if !ok {
		return
	}
	q, results, err := h.svc.Results(r.Context(), actor, id)
	if h.writeQuizError(w, "list results", err, "no such quiz") {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"quiz":    q,
		"results": results,
	})
}
