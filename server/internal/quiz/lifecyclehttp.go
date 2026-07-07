package quiz

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/httpapi"
)

// Duration bounds for a single attempt: below 30 seconds is certainly a
// typo, beyond 24 hours the window itself should carry the time budget.
const (
	minDurationSec = 30
	maxDurationSec = 86400
)

type publishRequest struct {
	StartsAt      *time.Time  `json:"starts_at"`
	EndsAt        *time.Time  `json:"ends_at"`
	DurationSec   *int        `json:"duration_sec"`
	Guardrails    *Guardrails `json:"guardrails"`
	ReleasePolicy *string     `json:"release_policy"`
}

func (h *Handler) handlePublishQuiz(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "no such quiz")
	if !ok {
		return
	}
	var req publishRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpapi.WriteFieldErrors(w, map[string]string{"body": "malformed JSON"})
		return
	}

	// Window rules from docs/04: starts_at < ends_at and in the future,
	// plus a duration. Validated here so precondition failures that need no
	// database read never open a transaction.
	fields := map[string]string{}
	now := time.Now()
	switch {
	case req.StartsAt == nil:
		fields["starts_at"] = "required"
	case !req.StartsAt.After(now):
		fields["starts_at"] = "must be in the future"
	}
	switch {
	case req.EndsAt == nil:
		fields["ends_at"] = "required"
	case req.StartsAt != nil && !req.EndsAt.After(*req.StartsAt):
		fields["ends_at"] = "must be after starts_at"
	}
	switch {
	case req.DurationSec == nil:
		fields["duration_sec"] = "required"
	case *req.DurationSec < minDurationSec || *req.DurationSec > maxDurationSec:
		fields["duration_sec"] = "must be between 30 seconds and 24 hours"
	}
	guardrails := DefaultGuardrails()
	if req.Guardrails != nil {
		guardrails = *req.Guardrails
		for field, msg := range guardrails.Validate() {
			fields[field] = msg
		}
	}
	releasePolicy := "auto"
	if req.ReleasePolicy != nil {
		releasePolicy = *req.ReleasePolicy
		if releasePolicy != "auto" && releasePolicy != "manual" {
			fields["release_policy"] = "must be auto or manual"
		}
	}
	if len(fields) > 0 {
		httpapi.WriteFieldErrors(w, fields)
		return
	}

	q, err := h.svc.Publish(r.Context(), actor, id, PublishInput{
		StartsAt:      req.StartsAt.UTC(),
		EndsAt:        req.EndsAt.UTC(),
		DurationSec:   *req.DurationSec,
		Guardrails:    guardrails,
		ReleasePolicy: releasePolicy,
	})
	if h.writeLifecycleError(w, "publish quiz", err, "no such quiz") {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"quiz": q})
}

type setAssignmentsRequest struct {
	StudentIDs []string `json:"student_ids"`
	GroupIDs   []string `json:"group_ids"`
}

func (h *Handler) handleSetAssignments(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "no such quiz")
	if !ok {
		return
	}
	var req setAssignmentsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpapi.WriteFieldErrors(w, map[string]string{"body": "malformed JSON"})
		return
	}
	for _, sid := range req.StudentIDs {
		if !uuidShape.MatchString(sid) {
			httpapi.WriteFieldErrors(w, map[string]string{"student_ids": "every id must be a uuid"})
			return
		}
	}
	for _, gid := range req.GroupIDs {
		if !uuidShape.MatchString(gid) {
			httpapi.WriteFieldErrors(w, map[string]string{"group_ids": "every id must be a uuid"})
			return
		}
	}
	students, err := h.svc.SetAssignments(r.Context(), actor, id, req.StudentIDs, req.GroupIDs)
	if h.writeLifecycleError(w, "set assignments", err, "no such quiz") {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"students": students})
}

func (h *Handler) handleForceCloseQuiz(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "no such quiz")
	if !ok {
		return
	}
	q, err := h.svc.ForceClose(r.Context(), actor, id)
	if h.writeLifecycleError(w, "force close quiz", err, "no such quiz") {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"quiz": q})
}

type extendRequest struct {
	EndsAt *time.Time `json:"ends_at"`
}

func (h *Handler) handleExtendQuiz(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "no such quiz")
	if !ok {
		return
	}
	var req extendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpapi.WriteFieldErrors(w, map[string]string{"body": "malformed JSON"})
		return
	}
	// "In the future" is checked here so a stale timestamp never opens a
	// transaction; "later than the current ends_at" needs the row and is a
	// precondition the service returns.
	switch {
	case req.EndsAt == nil:
		httpapi.WriteFieldErrors(w, map[string]string{"ends_at": "required"})
		return
	case !req.EndsAt.After(time.Now()):
		httpapi.WriteFieldErrors(w, map[string]string{"ends_at": "must be in the future"})
		return
	}
	q, err := h.svc.Extend(r.Context(), actor, id, req.EndsAt.UTC())
	if h.writeLifecycleError(w, "extend quiz", err, "no such quiz") {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"quiz": q})
}

func (h *Handler) handleArchiveQuiz(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "no such quiz")
	if !ok {
		return
	}
	q, err := h.svc.Archive(r.Context(), actor, id)
	if h.writeLifecycleError(w, "archive quiz", err, "no such quiz") {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"quiz": q})
}

func (h *Handler) handleListAssignments(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "no such quiz")
	if !ok {
		return
	}
	students, err := h.svc.ListAssignments(r.Context(), actor, id)
	if h.writeLifecycleError(w, "list assignments", err, "no such quiz") {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"students": students})
}

func (h *Handler) handleAssignedQuizzes(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	quizzes, err := h.svc.AssignedQuizzes(r.Context(), actor)
	if err != nil {
		h.internalError(w, "list assigned quizzes", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"quizzes": quizzes})
}

// writeLifecycleError extends writeQuizError with the precondition shape
// lifecycle mutations can produce.
func (h *Handler) writeLifecycleError(w http.ResponseWriter, op string, err error, notFoundMsg string) bool {
	var pre *PreconditionError
	if errors.As(err, &pre) {
		httpapi.WriteFieldErrors(w, pre.Fields)
		return true
	}
	return h.writeQuizError(w, op, err, notFoundMsg)
}

// requireStudent gates the student-flow routes. The list itself is scoped by
// the assignment join, so role is the only identity fact to check here.
func requireStudent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, ok := authusers.ActorFrom(r.Context()); !ok || u.Role != "student" {
			httpapi.WriteError(w, http.StatusForbidden, httpapi.CodeForbidden,
				"the assigned quiz list is for students")
			return
		}
		next.ServeHTTP(w, r)
	})
}
