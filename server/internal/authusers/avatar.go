package authusers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"macquiz/server/internal/audit"
	"macquiz/server/internal/blobstore"
)

// The users.avatar column encodes the choice as one self-describing string
// (migration 00011): "preset:<slug>" names a sticker from AvatarPresets,
// "upload:<hash>" points at a re-encoded photo in the avatar blob store.
// The single string travels cheaply through every people-list payload, and
// the upload hash doubles as the serving endpoint's ETag.
const (
	avatarPresetPrefix = "preset:"
	avatarUploadPrefix = "upload:"
)

// ErrAvatarStorageUnavailable reports a Service without an avatar blob
// store; only possible on a miswired process, never in serve (main always
// calls SetAvatarStore - the local-disk fallback needs no configuration).
var ErrAvatarStorageUnavailable = errors.New("avatar storage unavailable")

// AvatarPresets is the built-in sticker allowlist. The SPA bundles the
// matching art under the same slugs (web/src/components/avatarPresets.ts);
// the two lists must stay identical, and the e2e profile suite pins that.
var AvatarPresets = map[string]bool{
	"robot":      true,
	"alien":      true,
	"ghost":      true,
	"cool-cat":   true,
	"skull-jam":  true,
	"dino":       true,
	"astro-duck": true,
	"wizard":     true,
	"coffee":     true,
	"noodles":    true,
	"boba":       true,
	"controller": true,
	"pizza":      true,
	"cassette":   true,
	"bolt":       true,
	"rocket":     true,
}

// SetAvatarStore wires the blob store uploaded avatar photos live in.
func (s *Service) SetAvatarStore(store blobstore.Store) {
	s.avatars = store
}

// avatarBlobRef maps an "upload:<hash>" avatar value onto its blob ref;
// ok is false for presets and any other value.
func avatarBlobRef(value string) (string, bool) {
	hash, ok := strings.CutPrefix(value, avatarUploadPrefix)
	if !ok {
		return "", false
	}
	return hash + ".jpg", true
}

// UploadAvatar re-encodes raw (any decodable image) into the canonical
// 256x256 JPEG, stores it, and points the account's avatar at it. The blob
// is written before the row so a failed transaction strands only an
// unreferenced blob - which the same user's retry overwrites, because the
// ref is derived from (user, content) and both are unchanged.
func (s *Service) UploadAvatar(ctx context.Context, userID string, raw []byte) (User, error) {
	if s.avatars == nil {
		return User{}, ErrAvatarStorageUnavailable
	}
	encoded, err := processAvatarImage(raw)
	if err != nil {
		return User{}, err
	}
	// The hash is scoped per user: the same photo on two accounts must not
	// share one blob, or one account replacing theirs would best-effort
	// delete the blob out from under the other.
	sum := sha256.Sum256(append([]byte(userID+":"), encoded...))
	hash := hex.EncodeToString(sum[:16])
	if err := s.avatars.Put(ctx, hash+".jpg", bytes.NewReader(encoded)); err != nil {
		return User{}, fmt.Errorf("store avatar blob: %w", err)
	}
	value := avatarUploadPrefix + hash
	return s.setAvatar(ctx, userID, &value)
}

// SelectAvatarPreset points the account's avatar at a built-in sticker.
// The slug is validated against AvatarPresets by the handler.
func (s *Service) SelectAvatarPreset(ctx context.Context, userID, slug string) (User, error) {
	value := avatarPresetPrefix + slug
	return s.setAvatar(ctx, userID, &value)
}

// ClearAvatar reverts the account to the initials chip.
func (s *Service) ClearAvatar(ctx context.Context, userID string) (User, error) {
	return s.setAvatar(ctx, userID, nil)
}

// setAvatar is the single write path every avatar mutation funnels through:
// row locked, diff audited as profile.updated in the same transaction
// (docs/08 section 7), and a replaced upload's blob deleted best-effort
// only after commit, so a rollback can never orphan the still-referenced one.
func (s *Service) setAvatar(ctx context.Context, userID string, next *string) (User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, fmt.Errorf("begin avatar tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	u, _, err := scanUser(tx.QueryRowContext(ctx,
		`SELECT `+userColumns+`, password_hash FROM users WHERE id = $1 FOR UPDATE`, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("load user: %w", err)
	}

	prev := u.Avatar
	if equalAvatar(prev, next) {
		return u, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE users SET avatar = $1 WHERE id = $2`, next, userID); err != nil {
		return User{}, fmt.Errorf("update avatar: %w", err)
	}
	u.Avatar = next

	changes := map[string]audit.Change{"avatar": {From: deref(prev), To: deref(next)}}
	if err := writeAudit(ctx, tx, userID, "profile.updated", "user", userID,
		map[string]any{"changes": changes}); err != nil {
		return User{}, err
	}
	if err := tx.Commit(); err != nil {
		return User{}, fmt.Errorf("commit avatar: %w", err)
	}

	s.deleteAvatarBlob(ctx, prev)
	return u, nil
}

// deleteAvatarBlob removes a no-longer-referenced upload's blob,
// best-effort: blob storage must never fail a committed mutation, so a
// failure is only logged and the orphan sits harmlessly in the store.
func (s *Service) deleteAvatarBlob(ctx context.Context, value *string) {
	if value == nil || s.avatars == nil {
		return
	}
	ref, ok := avatarBlobRef(*value)
	if !ok {
		return
	}
	if err := s.avatars.Delete(ctx, ref); err != nil {
		s.log.Warn("delete replaced avatar blob", "ref", ref, "err", err)
	}
}

// AvatarContent opens the stored JPEG for an account whose avatar is an
// upload, returning the content hash the handler serves as the ETag.
// ErrNotFound covers a missing account, a preset or empty avatar, and a
// missing blob alike - the serving endpoint answers 404 to each.
func (s *Service) AvatarContent(ctx context.Context, userID string) (io.ReadCloser, string, error) {
	if s.avatars == nil {
		return nil, "", ErrAvatarStorageUnavailable
	}
	u, err := s.UserByID(ctx, userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", fmt.Errorf("load user: %w", err)
	}
	if u.Avatar == nil {
		return nil, "", ErrNotFound
	}
	ref, ok := avatarBlobRef(*u.Avatar)
	if !ok {
		return nil, "", ErrNotFound
	}
	rc, err := s.avatars.Open(ctx, ref)
	if err != nil {
		s.log.Warn("open avatar blob", "ref", ref, "err", err)
		return nil, "", ErrNotFound
	}
	return rc, strings.TrimPrefix(*u.Avatar, avatarUploadPrefix), nil
}

func equalAvatar(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// deref renders a nullable avatar value for an audit diff: the string
// itself, or nil - never the pointer, which would serialize as an address.
func deref(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}
