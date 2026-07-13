package authusers

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"macquiz/server/internal/httpapi"
)

// handleUploadAvatar serves PUT /auth/me/avatar. There is no multipart
// envelope to parse or defend against, so the request body IS the image
// file, same raw-body convention as the bulk imports; the pipeline in
// avatarimage.go re-encodes it before anything is stored.
func (h *Handler) handleUploadAvatar(w http.ResponseWriter, r *http.Request) {
	u, _ := ActorFrom(r.Context())
	if ok, retry := h.avatarUploads.Allow(u.ID, time.Now()); !ok {
		httpapi.WriteRateLimited(w, retry)
		return
	}

	body := http.MaxBytesReader(w, r.Body, MaxAvatarBytes)
	raw, err := io.ReadAll(body)
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		httpapi.WriteFieldErrors(w, map[string]string{"file": "must be 2 MB or smaller"})
		return
	}
	if err != nil {
		h.internalError(w, "read avatar upload", err)
		return
	}

	updated, err := h.svc.UploadAvatar(r.Context(), u.ID, raw)
	if errors.Is(err, ErrBadAvatarImage) {
		httpapi.WriteFieldErrors(w, map[string]string{"file": "must be a PNG, JPEG, WebP, or GIF image"})
		return
	}
	if err != nil {
		h.internalError(w, "upload avatar", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, sessionResponse{User: updated})
}

type selectAvatarPresetRequest struct {
	Preset string `json:"preset"`
}

// handleSelectAvatarPreset serves POST /auth/me/avatar/preset.
func (h *Handler) handleSelectAvatarPreset(w http.ResponseWriter, r *http.Request) {
	u, _ := ActorFrom(r.Context())
	var req selectAvatarPresetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Preset == "" {
		httpapi.WriteFieldErrors(w, map[string]string{"preset": "required"})
		return
	}
	if !AvatarPresets[req.Preset] {
		httpapi.WriteFieldErrors(w, map[string]string{"preset": "unknown avatar preset"})
		return
	}

	updated, err := h.svc.SelectAvatarPreset(r.Context(), u.ID, req.Preset)
	if err != nil {
		h.internalError(w, "select avatar preset", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, sessionResponse{User: updated})
}

// handleDeleteAvatar serves DELETE /auth/me/avatar.
func (h *Handler) handleDeleteAvatar(w http.ResponseWriter, r *http.Request) {
	u, _ := ActorFrom(r.Context())
	updated, err := h.svc.ClearAvatar(r.Context(), u.ID)
	if err != nil {
		h.internalError(w, "clear avatar", err)
		return
	}
	httpapi.WriteJSON(w, http.StatusOK, sessionResponse{User: updated})
}

// handleGetUserAvatar serves GET /users/{id}/avatar: the stored JPEG for an
// account whose avatar is an upload. Any active account may fetch any
// avatar (ActionAvatarRead) - it is an identification aid on the same
// surfaces that already show names - so unlike other per-resource reads
// there is no assignment check to fail, and 404 means only "no photo".
func (h *Handler) handleGetUserAvatar(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !uuidShape.MatchString(id) {
		httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound, "no avatar")
		return
	}

	rc, hash, err := h.svc.AvatarContent(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound, "no avatar")
		return
	}
	if err != nil {
		h.internalError(w, "get avatar", err)
		return
	}
	defer rc.Close()

	// The hash is content-derived, so it is a valid strong ETag: a replaced
	// photo changes the hash, and the SPA busts its cache by ?v=<hash>.
	etag := `"` + hash + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	if _, err := io.Copy(w, rc); err != nil {
		// Headers are gone; nothing to write but the log.
		h.svc.log.Warn("stream avatar", "user_id", id, "err", err)
	}
}
