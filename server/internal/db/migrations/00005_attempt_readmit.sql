-- +goose Up
-- Milestone 6 re-admission (docs/06 section 4:81): "Re-admission is a new
-- attempt, not a resurrection ... grants one extra attempt slot (audited)".
-- readmitted_at marks a kicked attempt whose student has been granted one fresh
-- slot. It is moderation metadata of the same class as kicked_by/kick_reason
-- (set on the attempt row after it leaves in_progress); it never rewrites the
-- student's answers or the kick reason, so the docs/06:84 immutability invariant
-- holds. Start counts these markers - count(readmitted_at) - and adds them to
-- max_attempts, so each readmit grants exactly one extra attempt and the grant
-- record is the marker itself (no separate counter to drift).
ALTER TABLE attempts
    ADD COLUMN readmitted_at timestamptz;

-- +goose Down
ALTER TABLE attempts
    DROP COLUMN readmitted_at;
