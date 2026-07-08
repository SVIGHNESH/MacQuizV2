-- +goose Up
-- Exam-day backup belt (docs/10-operations.md section 1): "a pre-quiz-window
-- dump is triggered automatically by the scheduler when any quiz enters
-- `scheduled` for the same day". quiz.Service.Publish upserts a row here
-- (trigger_date = starts_at's UTC date) when a quiz is published to start
-- today; the backup container's tighter cron (scripts/backup/check-trigger.sh)
-- polls this table and runs an extra pg_dump/upload pass ahead of the nightly
-- 02:00 UTC job, then stamps fulfilled_at so later polls that day are no-ops.
-- One row per calendar day regardless of how many quizzes start that day.
CREATE TABLE backup_triggers (
    trigger_date date PRIMARY KEY,
    requested_at timestamptz NOT NULL DEFAULT now(),
    fulfilled_at timestamptz
);

-- +goose Down
DROP TABLE backup_triggers;
