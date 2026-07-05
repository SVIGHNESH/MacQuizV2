package authusers

import (
	"context"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"macquiz/server/internal/httpapi"
)

// DirectoryStudent is the audience-picker view of a student account: just
// enough to choose and recognize a person. Status, role, and credential
// facts stay on the admin surface.
type DirectoryStudent struct {
	ID       string `json:"id"`
	FullName string `json:"full_name"`
	Email    string `json:"email"`
}

// Directory is what a teacher sees when assigning a quiz: every active
// student and every cohort, name-sorted for a picker.
type Directory struct {
	Students []DirectoryStudent `json:"students"`
	Groups   []Group            `json:"groups"`
}

// ListDirectory returns the assignable audience. Only active students
// appear: a disabled account cannot take a quiz, so offering it in the
// picker would only manufacture confusing assignments.
func (s *Service) ListDirectory(ctx context.Context) (Directory, error) {
	dir := Directory{Students: []DirectoryStudent{}}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, full_name, email FROM users
		 WHERE role = 'student' AND status = 'active'
		 ORDER BY full_name, id`)
	if err != nil {
		return Directory{}, fmt.Errorf("list directory students: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var st DirectoryStudent
		if err := rows.Scan(&st.ID, &st.FullName, &st.Email); err != nil {
			return Directory{}, fmt.Errorf("scan directory student: %w", err)
		}
		dir.Students = append(dir.Students, st)
	}
	if err := rows.Err(); err != nil {
		return Directory{}, err
	}

	groups, err := s.ListGroups(ctx)
	if err != nil {
		return Directory{}, err
	}
	dir.Groups = groups
	return dir, nil
}

// DirectoryRoutes returns the /api/v1/directory route group: a read-only
// surface for teachers (and admins) building a quiz audience.
func (h *Handler) DirectoryRoutes() http.Handler {
	r := chi.NewRouter()
	r.Use(h.svc.RequireAuth, RequirePasswordChanged, requireCan(ActionDirectoryRead))
	r.Get("/", h.handleDirectory)
	return r
}

func (h *Handler) handleDirectory(w http.ResponseWriter, r *http.Request) {
	dir, err := h.svc.ListDirectory(r.Context())
	if err != nil {
		h.internalError(w, "list directory", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, dir)
}
