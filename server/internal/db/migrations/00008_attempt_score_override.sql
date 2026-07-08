-- +goose Up
-- docs/06-attempt-lifecycle.md line 80: "results are flagged kicked wherever
-- scores appear, and the teacher can override the score to zero per
-- attempt." score_overridden_at/_by are moderation metadata of the same
-- class as kicked_by/kick_reason/readmitted_at (set on the attempt row after
-- it leaves in_progress); the marker itself is the source of truth that a
-- score of 0 is a deliberate override rather than a naturally-earned zero.
ALTER TABLE attempts
    ADD COLUMN score_overridden_at timestamptz,
    ADD COLUMN score_overridden_by uuid REFERENCES users (id);

-- +goose Down
ALTER TABLE attempts
    DROP COLUMN score_overridden_by,
    DROP COLUMN score_overridden_at;
