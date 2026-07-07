package authusers

import (
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"macquiz/server/internal/audit"
	"macquiz/server/internal/httpapi"
)

// AuditRoutes returns the admin /api/v1/audit route group: a single filterable,
// keyset-paginated read of the append-only audit_log (docs/04-api.md section 2,
// admin-only per the permission matrix). It reuses the same middleware chain as
// the other admin surfaces and the ActionAuditRead policy action, which Can
// grants to admins alone.
func (h *Handler) AuditRoutes() http.Handler {
	r := chi.NewRouter()
	r.Use(h.svc.RequireAuth, RequirePasswordChanged, requireCan(ActionAuditRead))
	r.Get("/", h.handleListAudit)
	return r
}

// handleListAudit parses the audit filters, screening each malformed value to a
// 422 rather than letting garbage reach a Postgres uuid/timestamp cast (which
// would surface as a 500) or silently defaulting a bad limit.
func (h *Handler) handleListAudit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	fields := map[string]string{}

	f := audit.Filter{
		Action:       q.Get("action"),
		ResourceType: q.Get("resource_type"),
	}

	if v := q.Get("actor_id"); v != "" {
		if !uuidShape.MatchString(v) {
			fields["actor_id"] = "must be a uuid"
		} else {
			f.ActorID = v
		}
	}
	if v := q.Get("resource_id"); v != "" {
		if !uuidShape.MatchString(v) {
			fields["resource_id"] = "must be a uuid"
		} else {
			f.ResourceID = v
		}
	}
	if v := q.Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err != nil {
			fields["from"] = "must be an RFC 3339 timestamp"
		} else {
			f.From = &t
		}
	}
	if v := q.Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err != nil {
			fields["to"] = "must be an RFC 3339 timestamp"
		} else {
			f.To = &t
		}
	}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > audit.MaxPageSize {
			fields["limit"] = "must be an integer between 1 and 200"
		} else {
			f.Limit = n
		}
	}
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			fields["before"] = "must be a positive audit entry id"
		} else {
			f.Before = n
		}
	}

	if len(fields) > 0 {
		httpapi.WriteFieldErrors(w, fields)
		return
	}

	page, err := audit.List(r.Context(), h.svc.db, f)
	if err != nil {
		h.internalError(w, "list audit", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, page)
}
