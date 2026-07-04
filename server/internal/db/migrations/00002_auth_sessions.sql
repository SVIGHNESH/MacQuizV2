-- +goose Up
-- Milestone 1 auth (docs/08-security.md section 1): rotating refresh tokens
-- and the forced first-login password reset.

-- Admin-issued credentials must be rotated on first login; the flag clears
-- when the user sets their own password. The bootstrap admin chose their own
-- password, so bootstrap inserts with false.
ALTER TABLE users
    ADD COLUMN must_change_password boolean NOT NULL DEFAULT true;

-- One row per refresh token. Rotation inserts a successor row in the same
-- family and stamps used_at on the presented token; presenting a token that
-- is already used or revoked is reuse (a stolen token), which revokes the
-- whole family. Only the SHA-256 of the token is stored, so a database leak
-- exposes no usable credentials.
CREATE TABLE user_sessions (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    family_id  uuid NOT NULL,
    token_hash bytea NOT NULL UNIQUE,
    created_at timestamptz NOT NULL DEFAULT now(),
    expires_at timestamptz NOT NULL,
    used_at    timestamptz,
    revoked_at timestamptz
);

CREATE INDEX user_sessions_user_id_idx ON user_sessions (user_id);
CREATE INDEX user_sessions_family_id_idx ON user_sessions (family_id);

-- +goose Down
DROP TABLE user_sessions;
ALTER TABLE users DROP COLUMN must_change_password;
