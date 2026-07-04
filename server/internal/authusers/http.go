package authusers

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"macquiz/server/internal/httpapi"
)

// Both tokens travel in httpOnly cookies (docs/08-security.md section 1):
// script-inaccessible, so an XSS cannot exfiltrate them. The refresh cookie
// is path-scoped to the auth endpoints, so it rides along only on
// login/refresh/logout, never on ordinary API calls.
const (
	accessCookieName  = "mq_access"
	refreshCookieName = "mq_refresh"
	refreshCookiePath = "/api/v1/auth"
)

// Handler bundles the service with HTTP-only concerns (cookies, rate limits).
type Handler struct {
	svc           *Service
	secureCookies bool
	loginByIP     *rateLimiter
	loginByEmail  *rateLimiter
}

// NewHandler wires the auth routes. secureCookies must be true in production
// (TLS-only cookies) and false in plain-HTTP development.
func NewHandler(svc *Service, secureCookies bool) *Handler {
	return &Handler{
		svc:           svc,
		secureCookies: secureCookies,
		// Login limits per IP and per account (docs/04-api.md section 5).
		loginByIP:    newRateLimiter(20, time.Minute),
		loginByEmail: newRateLimiter(5, time.Minute),
	}
}

// Routes returns the /api/v1/auth route group.
func (h *Handler) Routes() http.Handler {
	r := chi.NewRouter()
	r.Post("/login", h.handleLogin)
	r.Post("/refresh", h.handleRefresh)
	r.Post("/logout", h.handleLogout)
	r.Group(func(r chi.Router) {
		r.Use(h.svc.RequireAuth)
		r.Get("/me", h.handleMe)
		r.Post("/password", h.handleChangePassword)
	})
	return r
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type sessionResponse struct {
	User User `json:"user"`
}

func (h *Handler) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Email == "" || req.Password == "" {
		httpapi.WriteFieldErrors(w, map[string]string{"email": "required", "password": "required"})
		return
	}
	now := time.Now()
	if ok, retry := h.loginByIP.allow(r.RemoteAddr, now); !ok {
		writeRateLimited(w, retry)
		return
	}
	if ok, retry := h.loginByEmail.allow(req.Email, now); !ok {
		writeRateLimited(w, retry)
		return
	}

	u, access, refresh, err := h.svc.Login(r.Context(), req.Email, req.Password)
	if errors.Is(err, ErrInvalidCredentials) {
		httpapi.WriteError(w, http.StatusUnauthorized, httpapi.CodeInvalidCredentials, "email or password is incorrect")
		return
	}
	if err != nil {
		h.internalError(w, "login", err)
		return
	}
	h.setSessionCookies(w, access, refresh)
	httpapi.WriteJSON(w, http.StatusOK, sessionResponse{User: u})
}

func (h *Handler) handleRefresh(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(refreshCookieName)
	if err != nil || c.Value == "" {
		httpapi.WriteError(w, http.StatusUnauthorized, httpapi.CodeUnauthenticated, "no session")
		return
	}
	u, access, refresh, err := h.svc.Refresh(r.Context(), c.Value)
	if errors.Is(err, ErrSessionInvalid) {
		h.clearSessionCookies(w)
		httpapi.WriteError(w, http.StatusUnauthorized, httpapi.CodeUnauthenticated, "session expired; log in again")
		return
	}
	if err != nil {
		h.internalError(w, "refresh", err)
		return
	}
	h.setSessionCookies(w, access, refresh)
	httpapi.WriteJSON(w, http.StatusOK, sessionResponse{User: u})
}

func (h *Handler) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(refreshCookieName); err == nil && c.Value != "" {
		if err := h.svc.Logout(r.Context(), c.Value); err != nil {
			h.internalError(w, "logout", err)
			return
		}
	}
	h.clearSessionCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleMe(w http.ResponseWriter, r *http.Request) {
	u, _ := ActorFrom(r.Context())
	httpapi.WriteJSON(w, http.StatusOK, sessionResponse{User: u})
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (h *Handler) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	u, _ := ActorFrom(r.Context())
	var req changePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpapi.WriteFieldErrors(w, map[string]string{"body": "malformed JSON"})
		return
	}
	if len(req.NewPassword) < 10 {
		httpapi.WriteFieldErrors(w, map[string]string{"new_password": "must be at least 10 characters"})
		return
	}
	err := h.svc.ChangePassword(r.Context(), u.ID, req.CurrentPassword, req.NewPassword)
	if errors.Is(err, ErrInvalidCredentials) {
		httpapi.WriteError(w, http.StatusUnauthorized, httpapi.CodeInvalidCredentials, "current password is incorrect")
		return
	}
	if err != nil {
		h.internalError(w, "change password", err)
		return
	}
	// Every session (this one included) was revoked; the client re-logs-in
	// with the new credential.
	h.clearSessionCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) setSessionCookies(w http.ResponseWriter, access, refresh string) {
	http.SetCookie(w, &http.Cookie{
		Name: accessCookieName, Value: access, Path: "/",
		MaxAge: int(AccessTokenTTL.Seconds()), HttpOnly: true,
		Secure: h.secureCookies, SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name: refreshCookieName, Value: refresh, Path: refreshCookiePath,
		MaxAge: int(RefreshTokenTTL.Seconds()), HttpOnly: true,
		Secure: h.secureCookies, SameSite: http.SameSiteStrictMode,
	})
}

func (h *Handler) clearSessionCookies(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: accessCookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: h.secureCookies, SameSite: http.SameSiteLaxMode,
	})
	http.SetCookie(w, &http.Cookie{
		Name: refreshCookieName, Value: "", Path: refreshCookiePath, MaxAge: -1,
		HttpOnly: true, Secure: h.secureCookies, SameSite: http.SameSiteStrictMode,
	})
}

func (h *Handler) internalError(w http.ResponseWriter, op string, err error) {
	h.svc.log.Error(op, "err", err)
	httpapi.WriteError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
}

func writeRateLimited(w http.ResponseWriter, retry time.Duration) {
	secs := int(retry.Seconds()) + 1
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	httpapi.WriteError(w, http.StatusTooManyRequests, httpapi.CodeRateLimited, "too many attempts; slow down")
}
