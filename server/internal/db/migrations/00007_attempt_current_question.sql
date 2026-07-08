-- +goose Up
-- docs/05-realtime-events.md section 4: current_question was always null in
-- the live roster snapshot and the attempt.progress delta because no server
-- column tracked the student's cursor. current_question stores the 1-based
-- ordinal position (within the pinned quiz_version's questions array) of the
-- last question the student saved an answer for - the same signal SaveAnswer
-- already validates the answer against (docs/06 section 4's snapshot
-- membership check), so no navigation-only client write is required.
ALTER TABLE attempts
    ADD COLUMN current_question integer;

-- +goose Down
ALTER TABLE attempts
    DROP COLUMN current_question;
