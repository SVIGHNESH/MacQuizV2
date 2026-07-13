-- +goose Up
-- User-chosen avatar (profile feature): NULL renders as the initials chip,
-- "preset:<slug>" names a built-in sticker bundled with the SPA, and
-- "upload:<hash>" points at a re-encoded photo in the avatar blob store,
-- where <hash> is the content hash the serving endpoint also uses as its
-- ETag. The CHECK pins the encoding so a bad write can never leak an
-- arbitrary string into every people-list payload that carries the column.
ALTER TABLE users
    ADD COLUMN avatar text
        CHECK (avatar IS NULL OR avatar ~ '^(preset:[a-z0-9-]+|upload:[a-f0-9]{16,64})$');

-- +goose Down
ALTER TABLE users DROP COLUMN avatar;
