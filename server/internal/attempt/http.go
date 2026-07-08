package attempt

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/httpapi"
	"macquiz/server/internal/ratelimit"
	"macquiz/server/internal/telemetry"
)

// Handler exposes the attempt player routes. Authentication and the
// forced-reset gate come from authusers middleware; ownership checks live in
// the service and read as 404.
type Handler struct {
	svc     *Service
	auth    *authusers.Service
	metrics *telemetry.Metrics
	// Kick and readmit are rate-limited per teacher, keyed by actor ID, to
	// prevent kick storms (docs/04-api.md section 5, docs/08 section 4).
	kickByTeacher    *ratelimit.Limiter
	readmitByTeacher *ratelimit.Limiter
}

// NewHandler wires the attempt routes.
func NewHandler(svc *Service, auth *authusers.Service) *Handler {
	return &Handler{
		svc:              svc,
		auth:             auth,
		kickByTeacher:    ratelimit.New(20, time.Minute),
		readmitByTeacher: ratelimit.New(20, time.Minute),
	}
}

// SetMetrics wires the docs/10-operations.md section 2 key-series metrics
// (autosave latency, violation/kick rates). Optional: a Handler with no
// metrics set records nothing, which is what every existing test gets.
func (h *Handler) SetMetrics(m *telemetry.Metrics) {
	h.metrics = m
}

// Routes returns the /api/v1/attempts route group: resume, autosave, and
// manual submit (docs/04-api.md student flow). Attempt start lives under
// /quizzes/{id}/attempts and is attached to the quiz mount via HandleStart.
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(h.auth.RequireAuth, authusers.RequirePasswordChanged)
	// The player surface is students only; per-attempt ownership stays in the
	// service, where denials answer 404.
	r.Group(func(r chi.Router) {
		r.Use(requireStudent)
		r.Get("/{id}", h.handleGet)
		r.Put("/{id}/answers/{questionID}", h.handleSaveAnswer)
		r.Post("/{id}/submit", h.handleSubmit)
		r.Post("/{id}/events", h.handleReportViolation)
		r.Get("/{id}/result", h.handleResult)
	})
	// Kick is a live-moderation power for teachers and admins (docs/06 section
	// 4); the owner-vs-admin resource decision stays in the service, where a
	// non-owning teacher answers 404.
	r.Group(func(r chi.Router) {
		r.Use(requireStaff)
		r.Post("/{id}/kick", h.handleKick)
		r.Post("/{id}/readmit", h.handleReadmit)
	})
	return r
}

// requireStudent gates the player surface on role; per-attempt ownership
// stays in the service, where denials answer 404.
func requireStudent(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, ok := authusers.ActorFrom(r.Context()); !ok || u.Role != "student" {
			httpapi.WriteError(w, http.StatusForbidden, httpapi.CodeForbidden,
				"the attempt player is for students")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireStaff gates the kick surface on the role-shaped fact that only
// teachers and admins may moderate a live attempt (docs/06 section 4). The
// owner-vs-admin resource decision stays in the service, where a non-owning
// teacher answers 404. It mirrors quiz.requireStaff (the two surfaces live in
// different packages, so the small gate is duplicated rather than shared).
func requireStaff(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, ok := authusers.ActorFrom(r.Context()); !ok ||
			(u.Role != "teacher" && u.Role != "admin") {
			httpapi.WriteError(w, http.StatusForbidden, httpapi.CodeForbidden,
				"live moderation is for teachers and admins")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// HandleStart serves POST /quizzes/{id}/attempts. The quiz module mounts it
// inside its student route group (which already enforces authentication and
// the student role), keeping the attempt lifecycle logic in this package.
func (h *Handler) HandleStart(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	quizID, ok := pathUUID(w, r, "id", "no such quiz")
	if !ok {
		return
	}
	detail, resumed, err := h.svc.Start(r.Context(), actor, quizID)
	if h.writeAttemptError(w, "start attempt", err, "no such quiz") {
		return
	}
	status := http.StatusCreated
	if resumed {
		status = http.StatusOK
	}
	httpapi.WriteJSON(w, status, detail)
}

func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "id", "no such attempt")
	if !ok {
		return
	}
	detail, err := h.svc.Get(r.Context(), actor, id)
	if h.writeAttemptError(w, "resume attempt", err, "no such attempt") {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, detail)
}

// maxResponseBytes bounds one autosaved response body. Real answers are an
// option key, a key list, a boolean, or a short text - a payload anywhere
// near this size is not a quiz answer.
const maxResponseBytes = 16 * 1024

type saveAnswerRequest struct {
	Response    json.RawMessage `json:"response"`
	TimeSpentMs *int            `json:"time_spent_ms"`
}

func (h *Handler) handleSaveAnswer(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "id", "no such attempt")
	if !ok {
		return
	}
	questionID, ok := pathUUID(w, r, "questionID", "no such question in this attempt")
	if !ok {
		return
	}
	var req saveAnswerRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxResponseBytes)).Decode(&req); err != nil {
		httpapi.WriteFieldErrors(w, map[string]string{"body": "malformed JSON"})
		return
	}
	if len(req.Response) == 0 {
		httpapi.WriteFieldErrors(w, map[string]string{"response": "required"})
		return
	}
	timeSpent := 0
	if req.TimeSpentMs != nil {
		if *req.TimeSpentMs < 0 {
			httpapi.WriteFieldErrors(w, map[string]string{"time_spent_ms": "must not be negative"})
			return
		}
		timeSpent = *req.TimeSpentMs
	}

	start := time.Now()
	answer, deadline, err := h.svc.SaveAnswer(r.Context(), actor, id, questionID, req.Response, timeSpent)
	h.metrics.RecordAutosave(r.Context(), time.Since(start))
	if h.writeAttemptError(w, "save answer", err, "no such question in this attempt") {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"answer":      answer,
		"deadline_at": deadline,
		"now":         time.Now().UTC(),
	})
}

func (h *Handler) handleSubmit(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "id", "no such attempt")
	if !ok {
		return
	}
	attempt, err := h.svc.SubmitManual(r.Context(), actor, id)
	if h.writeAttemptError(w, "submit attempt", err, "no such attempt") {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"attempt": attempt})
}

// maxEventBytes bounds a violation report body. A report is a short type tag
// plus an optional duration; anything larger is not a guardrail event.
const maxEventBytes = 1024

type reportViolationRequest struct {
	Type       string `json:"type"`
	DurationMs *int   `json:"duration_ms"`
}

// handleReportViolation serves POST /attempts/{id}/events: the student's client
// reports one guardrail violation (docs/04:72, the REST fallback for the attempt
// socket). The type is required and must be a known guardrail; duration_ms is
// optional. The response carries the attempt (with its updated violation_count)
// and whether this report counted toward the ladder.
func (h *Handler) handleReportViolation(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "id", "no such attempt")
	if !ok {
		return
	}
	var req reportViolationRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxEventBytes)).Decode(&req); err != nil {
		httpapi.WriteFieldErrors(w, map[string]string{"body": "malformed JSON"})
		return
	}
	req.Type = strings.TrimSpace(req.Type)
	if !violationTypes[req.Type] {
		httpapi.WriteFieldErrors(w, map[string]string{"type": "must be fullscreen, focus, or clipboard"})
		return
	}
	if req.DurationMs != nil && *req.DurationMs < 0 {
		httpapi.WriteFieldErrors(w, map[string]string{"duration_ms": "must not be negative"})
		return
	}
	attempt, counted, err := h.svc.ReportViolation(r.Context(), actor, id, req.Type, req.DurationMs)
	if h.writeAttemptError(w, "report violation", err, "no such attempt") {
		return
	}
	h.metrics.RecordViolation(r.Context(), req.Type)
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"attempt": attempt, "counted": counted})
}

// maxReasonBytes bounds the kick reason. A real justification is a canned
// phrase plus optional free text; anything near this size is not a reason.
const maxReasonBytes = 2 * 1024

type kickRequest struct {
	Reason string `json:"reason"`
}

// handleKick serves POST /attempts/{id}/kick: the teacher/admin removes a
// student from a live attempt (docs/06 section 4). The reason is required.
func (h *Handler) handleKick(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "id", "no such attempt")
	if !ok {
		return
	}
	if ok, retry := h.kickByTeacher.Allow(actor.ID, time.Now()); !ok {
		httpapi.WriteRateLimited(w, retry)
		return
	}
	var req kickRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxReasonBytes)).Decode(&req); err != nil {
		httpapi.WriteFieldErrors(w, map[string]string{"body": "malformed JSON"})
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" {
		httpapi.WriteFieldErrors(w, map[string]string{"reason": "required"})
		return
	}
	attempt, err := h.svc.Kick(r.Context(), actor, id, req.Reason)
	if h.writeAttemptError(w, "kick attempt", err, "no such attempt") {
		return
	}
	h.metrics.RecordKick(r.Context())
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"attempt": attempt})
}

// handleReadmit serves POST /attempts/{id}/readmit: the teacher/admin grants a
// kicked student one fresh attempt slot (docs/06 section 4). The reason is
// required and audited; the target must be a kicked attempt. It mirrors
// handleKick's body handling.
func (h *Handler) handleReadmit(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "id", "no such attempt")
	if !ok {
		return
	}
	if ok, retry := h.readmitByTeacher.Allow(actor.ID, time.Now()); !ok {
		httpapi.WriteRateLimited(w, retry)
		return
	}
	var req kickRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxReasonBytes)).Decode(&req); err != nil {
		httpapi.WriteFieldErrors(w, map[string]string{"body": "malformed JSON"})
		return
	}
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" {
		httpapi.WriteFieldErrors(w, map[string]string{"reason": "required"})
		return
	}
	attempt, err := h.svc.Readmit(r.Context(), actor, id, req.Reason)
	if h.writeAttemptError(w, "readmit attempt", err, "no such attempt") {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"attempt": attempt})
}

// handleResult serves GET /attempts/{id}/result: the released review with
// the score, the answer key, and the per-question grading.
func (h *Handler) handleResult(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "id", "no such attempt")
	if !ok {
		return
	}
	result, err := h.svc.Result(r.Context(), actor, id)
	if err != nil {
		if errors.Is(err, ErrResultsNotReleased) {
			httpapi.WriteError(w, http.StatusConflict, httpapi.CodeResultsNotReleased,
				"results for this quiz have not been released yet")
			return
		}
		h.writeAttemptError(w, "read result", err, "no such attempt")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, result)
}

// writeAttemptError maps service errors onto the docs/04 wire vocabulary; it
// reports whether a response was written.
func (h *Handler) writeAttemptError(w http.ResponseWriter, op string, err error, notFoundMsg string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrNotFound):
		httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound, notFoundMsg)
	case errors.Is(err, ErrQuizNotLive):
		httpapi.WriteError(w, http.StatusConflict, httpapi.CodeQuizNotLive,
			"this quiz is not open right now")
	case errors.Is(err, ErrAttemptLimit):
		httpapi.WriteError(w, http.StatusConflict, httpapi.CodeAttemptLimitReached,
			"every allowed attempt for this quiz has been used")
	case errors.Is(err, ErrDeadlinePassed):
		httpapi.WriteError(w, http.StatusConflict, httpapi.CodeAttemptDeadlinePassed,
			"the attempt deadline has passed")
	case errors.Is(err, ErrKicked):
		httpapi.WriteError(w, http.StatusConflict, httpapi.CodeAttemptKicked,
			"you were removed from this quiz")
	case errors.Is(err, ErrAlreadySubmitted):
		httpapi.WriteError(w, http.StatusConflict, httpapi.CodeAttemptAlreadySubmitted,
			"this attempt has already been submitted")
	case errors.Is(err, ErrNotKicked):
		httpapi.WriteError(w, http.StatusConflict, httpapi.CodeAttemptNotKicked,
			"this attempt was not kicked, so there is nothing to readmit")
	case errors.Is(err, ErrGuardrailOff):
		httpapi.WriteError(w, http.StatusConflict, httpapi.CodeGuardrailOff,
			"this guardrail is not enabled for this attempt")
	default:
		h.svc.log.Error(op, "err", err)
		httpapi.WriteError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
	}
	return true
}

// pathUUID pre-screens a path segment so garbage never reaches a Postgres
// uuid cast; a non-uuid reads as 404, same as an unknown id.
func pathUUID(w http.ResponseWriter, r *http.Request, param, notFoundMsg string) (string, bool) {
	id := chi.URLParam(r, param)
	if !uuidShape.MatchString(id) {
		httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound, notFoundMsg)
		return "", false
	}
	return id, true
}

var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
