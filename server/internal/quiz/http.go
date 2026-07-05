package quiz

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/httpapi"
)

// Handler exposes the quiz authoring routes. Authentication and the
// forced-reset gate come from authusers middleware; ownership checks live in
// the service against the central policy.
type Handler struct {
	svc  *Service
	auth *authusers.Service
}

// NewHandler wires the quiz routes.
func NewHandler(svc *Service, auth *authusers.Service) *Handler {
	return &Handler{svc: svc, auth: auth}
}

// QuizRoutes returns the /api/v1/quizzes route group. The whole authoring
// surface is teacher-only (docs/08-security.md: admins cannot author);
// per-quiz ownership then decides 404 in the service.
func (h *Handler) QuizRoutes() http.Handler {
	r := chi.NewRouter()
	r.Use(h.auth.RequireAuth, authusers.RequirePasswordChanged, requireTeacher)
	r.Get("/", h.handleListQuizzes)
	r.Post("/", h.handleCreateQuiz)
	r.Get("/{id}", h.handleGetQuiz)
	r.Patch("/{id}", h.handleUpdateQuiz)
	r.Delete("/{id}", h.handleDeleteQuiz)
	r.Post("/{id}/questions", h.handleAddQuestion)
	r.Put("/{id}/questions/order", h.handleReorderQuestions)
	return r
}

// QuestionRoutes returns the /api/v1/questions route group
// (docs/04-api.md: PATCH /questions/:id, DELETE /questions/:id).
func (h *Handler) QuestionRoutes() http.Handler {
	r := chi.NewRouter()
	r.Use(h.auth.RequireAuth, authusers.RequirePasswordChanged, requireTeacher)
	r.Patch("/{id}", h.handleUpdateQuestion)
	r.Delete("/{id}", h.handleDeleteQuestion)
	return r
}

// requireTeacher gates the authoring surface on the create capability, the
// role-shaped row of the permission matrix. Resource-specific denials stay
// in the service, where they answer 404.
func requireTeacher(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, ok := authusers.ActorFrom(r.Context()); !ok ||
			!authusers.Can(u, authusers.ActionQuizCreate, authusers.Resource{}) {
			httpapi.WriteError(w, http.StatusForbidden, httpapi.CodeForbidden,
				"quiz authoring is for teachers")
			return
		}
		next.ServeHTTP(w, r)
	})
}

type createQuizRequest struct {
	Title string `json:"title"`
}

func (h *Handler) handleCreateQuiz(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	var req createQuizRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Title) == "" {
		httpapi.WriteFieldErrors(w, map[string]string{"title": "required"})
		return
	}
	q, err := h.svc.CreateQuiz(r.Context(), actor, strings.TrimSpace(req.Title))
	if err != nil {
		h.internalError(w, "create quiz", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusCreated, map[string]any{"quiz": q})
}

func (h *Handler) handleListQuizzes(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	quizzes, err := h.svc.ListQuizzes(r.Context(), actor)
	if err != nil {
		h.internalError(w, "list quizzes", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"quizzes": quizzes})
}

func (h *Handler) handleGetQuiz(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "no such quiz")
	if !ok {
		return
	}
	q, questions, err := h.svc.GetQuiz(r.Context(), actor, id)
	if h.writeQuizError(w, "get quiz", err, "no such quiz") {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{
		"quiz":      q,
		"questions": TeacherViews(questions),
	})
}

type updateQuizRequest struct {
	Title            *string `json:"title"`
	MaxAttempts      *int    `json:"max_attempts"`
	ShuffleQuestions *bool   `json:"shuffle_questions"`
}

func (h *Handler) handleUpdateQuiz(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "no such quiz")
	if !ok {
		return
	}
	var req updateQuizRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpapi.WriteFieldErrors(w, map[string]string{"body": "malformed JSON"})
		return
	}
	fields := map[string]string{}
	if req.Title != nil && strings.TrimSpace(*req.Title) == "" {
		fields["title"] = "must not be empty"
	}
	if req.MaxAttempts != nil && (*req.MaxAttempts < 1 || *req.MaxAttempts > 10) {
		fields["max_attempts"] = "must be between 1 and 10"
	}
	if len(fields) > 0 {
		httpapi.WriteFieldErrors(w, fields)
		return
	}
	if req.Title != nil {
		trimmed := strings.TrimSpace(*req.Title)
		req.Title = &trimmed
	}

	q, err := h.svc.UpdateQuiz(r.Context(), actor, id, QuizPatch{
		Title: req.Title, MaxAttempts: req.MaxAttempts, ShuffleQuestions: req.ShuffleQuestions})
	if h.writeQuizError(w, "update quiz", err, "no such quiz") {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"quiz": q})
}

func (h *Handler) handleDeleteQuiz(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "no such quiz")
	if !ok {
		return
	}
	err := h.svc.DeleteQuiz(r.Context(), actor, id)
	if h.writeQuizError(w, "delete quiz", err, "no such quiz") {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleAddQuestion(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "no such quiz")
	if !ok {
		return
	}
	in, ok := decodeQuestionInput(w, r)
	if !ok {
		return
	}
	q, err := h.svc.AddQuestion(r.Context(), actor, id, in)
	if h.writeQuizError(w, "add question", err, "no such quiz") {
		return
	}
	httpapi.WriteJSON(w, http.StatusCreated, map[string]any{"question": TeacherView(q)})
}

func (h *Handler) handleUpdateQuestion(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "no such question")
	if !ok {
		return
	}
	in, ok := decodeQuestionInput(w, r)
	if !ok {
		return
	}
	q, err := h.svc.UpdateQuestion(r.Context(), actor, id, in)
	if h.writeQuizError(w, "update question", err, "no such question") {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"question": TeacherView(q)})
}

func (h *Handler) handleDeleteQuestion(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "no such question")
	if !ok {
		return
	}
	err := h.svc.DeleteQuestion(r.Context(), actor, id)
	if h.writeQuizError(w, "delete question", err, "no such question") {
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type reorderRequest struct {
	QuestionIDs []string `json:"question_ids"`
}

func (h *Handler) handleReorderQuestions(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id, ok := pathUUID(w, r, "no such quiz")
	if !ok {
		return
	}
	var req reorderRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.QuestionIDs == nil {
		httpapi.WriteFieldErrors(w, map[string]string{"question_ids": "required"})
		return
	}
	for _, qid := range req.QuestionIDs {
		if !uuidShape.MatchString(qid) {
			httpapi.WriteFieldErrors(w, map[string]string{"question_ids": "every id must be a uuid"})
			return
		}
	}
	questions, err := h.svc.ReorderQuestions(r.Context(), actor, id, req.QuestionIDs)
	if errors.Is(err, ErrBadOrder) {
		httpapi.WriteFieldErrors(w, map[string]string{
			"question_ids": "must list every question of the quiz exactly once"})
		return
	}
	if h.writeQuizError(w, "reorder questions", err, "no such quiz") {
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"questions": TeacherViews(questions)})
}

// decodeQuestionInput parses and validates a question body, writing the 422
// itself when invalid.
func decodeQuestionInput(w http.ResponseWriter, r *http.Request) (QuestionInput, bool) {
	var in QuestionInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpapi.WriteFieldErrors(w, map[string]string{"body": "malformed JSON"})
		return QuestionInput{}, false
	}
	if fields := in.Validate(); len(fields) > 0 {
		httpapi.WriteFieldErrors(w, fields)
		return QuestionInput{}, false
	}
	return in, true
}

// writeQuizError maps service errors onto the wire vocabulary; it reports
// whether a response was written.
func (h *Handler) writeQuizError(w http.ResponseWriter, op string, err error, notFoundMsg string) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrNotFound):
		httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound, notFoundMsg)
	case errors.Is(err, ErrNotEditable):
		httpapi.WriteError(w, http.StatusConflict, httpapi.CodeQuizNotEditable,
			"this quiz has been published and can no longer be edited")
	default:
		h.internalError(w, op, err)
	}
	return true
}

// pathUUID pre-screens the {id} path segment so garbage never reaches a
// Postgres uuid cast; a non-uuid reads as 404, same as an unknown id.
func pathUUID(w http.ResponseWriter, r *http.Request, notFoundMsg string) (string, bool) {
	id := chi.URLParam(r, "id")
	if !uuidShape.MatchString(id) {
		httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound, notFoundMsg)
		return "", false
	}
	return id, true
}

func (h *Handler) internalError(w http.ResponseWriter, op string, err error) {
	h.svc.log.Error(op, "err", err)
	httpapi.WriteError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
}

var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
