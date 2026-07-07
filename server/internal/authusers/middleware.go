package authusers

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"macquiz/server/internal/httpapi"
)

type contextKey struct{}

// ActorFrom returns the authenticated user placed by RequireAuth. Every
// module handler calls this; ok is false only on routes that forgot the
// middleware, which is a programming error.
func ActorFrom(ctx context.Context) (User, bool) {
	u, ok := ctx.Value(contextKey{}).(User)
	return u, ok
}

// WithActor returns a copy of ctx carrying the authenticated actor - the
// write-side counterpart to ActorFrom. RequireAuth uses it once it has loaded
// the account; it is also the injection point for any authenticator that runs
// outside the standard RequireAuth path (a WebSocket handshake, tests).
func WithActor(ctx context.Context, u User) context.Context {
	return context.WithValue(ctx, contextKey{}, u)
}

// RequireAuth authenticates the request from the access-token cookie (or an
// Authorization: Bearer header for non-browser clients) and loads the account
// fresh from the database, so disabling an account takes effect on the next
// request, not the next token expiry.
func (s *Service) RequireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			if c, err := r.Cookie(accessCookieName); err == nil {
				token = c.Value
			}
		}
		if token == "" {
			httpapi.WriteError(w, http.StatusUnauthorized, httpapi.CodeUnauthenticated, "authentication required")
			return
		}
		userID, _, err := parseAccessToken(s.secret, token)
		if err != nil {
			httpapi.WriteError(w, http.StatusUnauthorized, httpapi.CodeUnauthenticated, "invalid or expired token")
			return
		}
		u, err := s.UserByID(r.Context(), userID)
		if errors.Is(err, sql.ErrNoRows) {
			httpapi.WriteError(w, http.StatusUnauthorized, httpapi.CodeUnauthenticated, "account no longer exists")
			return
		}
		if err != nil {
			s.log.Error("load actor", "err", err)
			httpapi.WriteError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
			return
		}
		if u.Status != "active" {
			httpapi.WriteError(w, http.StatusUnauthorized, httpapi.CodeUnauthenticated, "account disabled")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithActor(r.Context(), u)))
	})
}

// RequirePasswordChanged blocks accounts still on an admin-issued credential
// (docs/08-security.md section 1). Mounted on every module route group except
// the auth endpoints themselves, so the only possible actions are seeing who
// you are, changing the password, and logging out.
func RequirePasswordChanged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, ok := ActorFrom(r.Context()); ok && u.MustChangePassword {
			httpapi.WriteError(w, http.StatusForbidden, httpapi.CodePasswordChangeRequired,
				"password change required before using the API")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if token, ok := strings.CutPrefix(h, "Bearer "); ok {
		return token
	}
	return ""
}
