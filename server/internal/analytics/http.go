package analytics

import (
	"errors"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/httpapi"
)

// Handler exposes the analytics read routes (docs/04 section 2, "Monitoring
// and analytics"). Authentication and the forced-reset gate come from
// authusers middleware; the owner-vs-admin resource decision stays in the
// service, where a non-owning teacher answers 404.
type Handler struct {
	svc  *Service
	auth *authusers.Service
}

// NewHandler wires the analytics routes.
func NewHandler(svc *Service, auth *authusers.Service) *Handler {
	return &Handler{svc: svc, auth: auth}
}

// Routes returns the /api/v1/analytics route group. Quiz analytics is
// staff-only (docs/04 permission matrix: teacher/admin, never a student), so it
// carries the same require-staff role gate live monitoring does; the service
// decides owner-vs-admin and 404s a teacher who is not the owner. Student
// analytics is NOT staff-gated - a student may read their own profile - so it
// gates on authentication alone and pushes the whole audience decision (self /
// assigned / admin) into the service, where a caller who may not see the
// subject answers 404, never 403.
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(h.auth.RequireAuth, authusers.RequirePasswordChanged)
	r.With(requireStaff).Get("/quizzes/{id}", h.handleQuizStats)
	r.Get("/students/{id}", h.handleStudentStats)
	r.With(requireStaff).Get("/teachers/{id}", h.handleTeacherStats)
	r.With(requireAdmin).Get("/org", h.handleOrgStats)
	return r
}

// requireAdmin gates the org-wide dashboard on the role-shaped fact that it
// is admin-only (docs/07 section 4, "Org-wide ... | Admin"): no owner or
// subject resource ever makes it visible to a teacher or student.
func requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, ok := authusers.ActorFrom(r.Context()); !ok || u.Role != "admin" {
			httpapi.WriteError(w, http.StatusForbidden, httpapi.CodeForbidden,
				"org analytics is admin-only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireStaff gates the analytics surface on the role-shaped fact that only
// teachers and admins may read quiz analytics (docs/04 permission matrix).
func requireStaff(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, ok := authusers.ActorFrom(r.Context()); !ok ||
			(u.Role != "teacher" && u.Role != "admin") {
			httpapi.WriteError(w, http.StatusForbidden, httpapi.CodeForbidden,
				"analytics is for teachers and admins")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleQuizStats serves GET /analytics/quizzes/{id}: the rolled-up quiz_stats
// row for the owning teacher or an admin. Every "you cannot see this" outcome
// reads as 404, so existence never leaks to a non-owner.
func (h *Handler) handleQuizStats(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id := chi.URLParam(r, "id")
	if !uuidShape.MatchString(id) {
		httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound, "no such quiz")
		return
	}
	stats, err := h.svc.QuizStats(r.Context(), actor, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound,
				"no analytics for this quiz")
			return
		}
		h.svc.log.Error("quiz analytics", "err", err)
		httpapi.WriteError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, stats)
}

// handleStudentStats serves GET /analytics/students/{id}: the student's
// cross-quiz profile for the student themselves, a teacher who has them
// assigned, or an admin. A caller who may not see the subject - or a subject
// with no rollup yet - reads 404, so one student's existence never leaks.
func (h *Handler) handleStudentStats(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id := chi.URLParam(r, "id")
	if !uuidShape.MatchString(id) {
		httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound, "no such student")
		return
	}
	stats, err := h.svc.StudentStats(r.Context(), actor, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound,
				"no analytics for this student")
			return
		}
		h.svc.log.Error("student analytics", "err", err)
		httpapi.WriteError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, stats)
}

// handleTeacherStats serves GET /analytics/teachers/{id}: the teacher's
// activity-and-outcomes summary for an admin or that teacher themselves. It is
// staff-gated like the quiz route (a student answers 403 at the gate); the
// service 404s a teacher aiming at another teacher and an admin aiming at a
// non-teacher id, so one teacher's existence never leaks.
func (h *Handler) handleTeacherStats(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	id := chi.URLParam(r, "id")
	if !uuidShape.MatchString(id) {
		httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound, "no such teacher")
		return
	}
	stats, err := h.svc.TeacherStats(r.Context(), actor, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound,
				"no analytics for this teacher")
			return
		}
		h.svc.log.Error("teacher analytics", "err", err)
		httpapi.WriteError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, stats)
}

// handleOrgStats serves GET /analytics/org: the admin-only org-wide
// dashboard (active users, quizzes-per-week trend, platform participation,
// cohort comparisons). It takes no path parameter, so there is no
// existence-leak concern to hide behind a 404 - the requireAdmin gate above
// already turned "not an admin" into 403.
func (h *Handler) handleOrgStats(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	stats, err := h.svc.OrgStats(r.Context(), actor)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			httpapi.WriteError(w, http.StatusForbidden, httpapi.CodeForbidden,
				"org analytics is admin-only")
			return
		}
		h.svc.log.Error("org analytics", "err", err)
		httpapi.WriteError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, stats)
}

// uuidShape pre-screens the {id} path segment so garbage never reaches a
// Postgres uuid cast; a non-uuid reads as 404, same as an unknown id.
var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
