package quiz

import (
	"net/http"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/httpapi"
)

// handleLiveRoster serves GET /quizzes/{id}/live (owner or admin): the roster
// snapshot the teacher dashboard fetches on connect before applying streamed
// deltas (docs/05 section 4). server_time is the database clock the row
// timestamps were read against, so every client-side countdown shares one
// origin.
func (h *Handler) handleLiveRoster(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "no such quiz")
	if !ok {
		return
	}
	q, roster, serverTime, err := h.svc.LiveRoster(r.Context(), actor, id)
	if h.writeQuizError(w, "live roster", err, "no such quiz") {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"quiz":        q,
		"roster":      roster,
		"server_time": serverTime,
	})
}
