package authusers

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/go-chi/chi/v5"

	"macquiz/server/internal/httpapi"
)

// UserRoutes returns the admin /api/v1/users route group. Every route is
// authenticated, gated on the forced first-login reset, and checked against
// the central policy (docs/04-api.md section 1).
func (h *Handler) UserRoutes() http.Handler {
	r := chi.NewRouter()
	r.Use(h.svc.RequireAuth, RequirePasswordChanged, requireCan(ActionUsersManage))
	r.Get("/", h.handleListUsers)
	r.Post("/", h.handleCreateUser)
	r.Patch("/{id}", h.handleUpdateUser)
	return r
}

// GroupRoutes returns the admin /api/v1/groups route group.
func (h *Handler) GroupRoutes() http.Handler {
	r := chi.NewRouter()
	r.Use(h.svc.RequireAuth, RequirePasswordChanged, requireCan(ActionGroupsManage))
	r.Get("/", h.handleListGroups)
	r.Post("/", h.handleCreateGroup)
	r.Get("/{id}/members", h.handleGetGroupMembers)
	r.Put("/{id}/members", h.handleSetGroupMembers)
	return r
}

// requireCan gates a route group on a global (resource-less) policy action.
// Per-resource decisions stay in handlers, where the loaded resource is at
// hand and a denial must read as 404.
func requireCan(action Action) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if u, ok := ActorFrom(r.Context()); !ok || !Can(u, action, Resource{}) {
				httpapi.WriteError(w, http.StatusForbidden, httpapi.CodeForbidden,
					"you do not have permission for this action")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

type createUserRequest struct {
	Role     string `json:"role"`
	Email    string `json:"email"`
	FullName string `json:"full_name"`
}

// provisionedUserResponse carries the generated first-login credential. It
// appears exactly once, in this response; it is never retrievable again.
type provisionedUserResponse struct {
	User            User   `json:"user"`
	InitialPassword string `json:"initial_password,omitempty"`
}

var emailShape = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

func (h *Handler) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	actor, _ := ActorFrom(r.Context())
	var req createUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpapi.WriteFieldErrors(w, map[string]string{"body": "malformed JSON"})
		return
	}
	fields := map[string]string{}
	// Admin accounts are created only by the operator bootstrap, never over
	// the API (docs/04-api.md: "provision a teacher or student").
	if req.Role != "teacher" && req.Role != "student" {
		fields["role"] = "must be teacher or student"
	}
	if !emailShape.MatchString(req.Email) {
		fields["email"] = "must be a valid email address"
	}
	if strings.TrimSpace(req.FullName) == "" {
		fields["full_name"] = "required"
	}
	if len(fields) > 0 {
		httpapi.WriteFieldErrors(w, fields)
		return
	}

	u, password, err := h.svc.CreateUser(r.Context(), actor, req.Role, req.Email, strings.TrimSpace(req.FullName))
	if errors.Is(err, ErrEmailTaken) {
		httpapi.WriteFieldErrors(w, map[string]string{"email": "already in use"})
		return
	}
	if err != nil {
		h.internalError(w, "create user", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusCreated, provisionedUserResponse{User: u, InitialPassword: password})
}

type updateUserRequest struct {
	FullName      *string `json:"full_name"`
	Status        *string `json:"status"`
	ResetPassword bool    `json:"reset_password"`
}

func (h *Handler) handleUpdateUser(w http.ResponseWriter, r *http.Request) {
	actor, _ := ActorFrom(r.Context())
	id := chi.URLParam(r, "id")
	if !uuidShape.MatchString(id) {
		httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound, "no such user")
		return
	}
	var req updateUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpapi.WriteFieldErrors(w, map[string]string{"body": "malformed JSON"})
		return
	}
	fields := map[string]string{}
	if req.Status != nil && *req.Status != "active" && *req.Status != "disabled" {
		fields["status"] = "must be active or disabled"
	}
	if req.FullName != nil && strings.TrimSpace(*req.FullName) == "" {
		fields["full_name"] = "must not be empty"
	}
	if len(fields) > 0 {
		httpapi.WriteFieldErrors(w, fields)
		return
	}

	u, password, err := h.svc.UpdateUser(r.Context(), actor, id, UserPatch(req))
	if errors.Is(err, ErrNotFound) {
		httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound, "no such user")
		return
	}
	if errors.Is(err, ErrSelfMutation) {
		httpapi.WriteFieldErrors(w, map[string]string{
			"id": "use the auth endpoints to change your own password or status"})
		return
	}
	if err != nil {
		h.internalError(w, "update user", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, provisionedUserResponse{User: u, InitialPassword: password})
}

func (h *Handler) handleListUsers(w http.ResponseWriter, r *http.Request) {
	role := r.URL.Query().Get("role")
	status := r.URL.Query().Get("status")
	fields := map[string]string{}
	if role != "" && role != "admin" && role != "teacher" && role != "student" {
		fields["role"] = "must be admin, teacher, or student"
	}
	if status != "" && status != "active" && status != "disabled" {
		fields["status"] = "must be active or disabled"
	}
	if len(fields) > 0 {
		httpapi.WriteFieldErrors(w, fields)
		return
	}
	users, err := h.svc.ListUsers(r.Context(), role, status)
	if err != nil {
		h.internalError(w, "list users", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"users": users})
}

type createGroupRequest struct {
	Name string `json:"name"`
}

func (h *Handler) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	actor, _ := ActorFrom(r.Context())
	var req createGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		httpapi.WriteFieldErrors(w, map[string]string{"name": "required"})
		return
	}
	g, err := h.svc.CreateGroup(r.Context(), actor, strings.TrimSpace(req.Name))
	if err != nil {
		h.internalError(w, "create group", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusCreated, map[string]any{"group": g})
}

type setMembersRequest struct {
	StudentIDs []string `json:"student_ids"`
}

func (h *Handler) handleSetGroupMembers(w http.ResponseWriter, r *http.Request) {
	actor, _ := ActorFrom(r.Context())
	id := chi.URLParam(r, "id")
	if !uuidShape.MatchString(id) {
		httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound, "no such group")
		return
	}
	var req setMembersRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.StudentIDs == nil {
		httpapi.WriteFieldErrors(w, map[string]string{"student_ids": "required (may be empty to clear the group)"})
		return
	}
	seen := map[string]bool{}
	for _, sid := range req.StudentIDs {
		if !uuidShape.MatchString(sid) {
			httpapi.WriteFieldErrors(w, map[string]string{"student_ids": "every id must be a uuid"})
			return
		}
		if seen[sid] {
			httpapi.WriteFieldErrors(w, map[string]string{"student_ids": "duplicate id " + sid})
			return
		}
		seen[sid] = true
	}

	g, err := h.svc.SetGroupMembers(r.Context(), actor, id, req.StudentIDs)
	if errors.Is(err, ErrNotFound) {
		httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound, "no such group")
		return
	}
	if errors.Is(err, errNotStudents) {
		httpapi.WriteFieldErrors(w, map[string]string{"student_ids": "every id must be an existing student account"})
		return
	}
	if err != nil {
		h.internalError(w, "set group members", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"group": g})
}

// handleGetGroupMembers serves GET /groups/{id}/members: the cohort's
// current roster, the read side of handleSetGroupMembers.
func (h *Handler) handleGetGroupMembers(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !uuidShape.MatchString(id) {
		httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound, "no such group")
		return
	}
	members, err := h.svc.GroupMembers(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound, "no such group")
		return
	}
	if err != nil {
		h.internalError(w, "list group members", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"students": members})
}

func (h *Handler) handleListGroups(w http.ResponseWriter, r *http.Request) {
	groups, err := h.svc.ListGroups(r.Context())
	if err != nil {
		h.internalError(w, "list groups", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, map[string]any{"groups": groups})
}

// uuidShape pre-screens path and body ids so garbage never reaches a
// Postgres uuid cast (which would surface as a 500 instead of a 404/422).
var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
