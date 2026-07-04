-- +goose Up
-- Initial schema from docs/03-data-model.md (SDD-001 v2.0 section 5).
-- All timestamps are timestamptz in UTC; rendering is a client concern.

CREATE EXTENSION IF NOT EXISTS citext;

CREATE TYPE user_role AS ENUM ('admin', 'teacher', 'student');
CREATE TYPE user_status AS ENUM ('active', 'disabled');
CREATE TYPE quiz_status AS ENUM ('draft', 'scheduled', 'live', 'closed', 'archived');
CREATE TYPE question_type AS ENUM ('single', 'multi', 'truefalse', 'short');
CREATE TYPE question_source AS ENUM ('manual', 'import');
CREATE TYPE submit_kind AS ENUM ('manual', 'auto', 'forced', 'kicked');
CREATE TYPE attempt_status AS ENUM ('in_progress', 'submitted', 'graded', 'kicked');
CREATE TYPE import_status AS ENUM ('validating', 'failed', 'ready', 'committed');

-- Every account is admin-created; the bootstrap admin references itself.
CREATE TABLE users (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    role          user_role NOT NULL,
    email         citext NOT NULL UNIQUE,
    password_hash text NOT NULL,
    full_name     text NOT NULL,
    status        user_status NOT NULL DEFAULT 'active',
    created_by    uuid NOT NULL REFERENCES users (id),
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE groups (
    id         uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name       text NOT NULL,
    created_by uuid NOT NULL REFERENCES users (id),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE group_members (
    group_id   uuid NOT NULL REFERENCES groups (id) ON DELETE CASCADE,
    student_id uuid NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    PRIMARY KEY (group_id, student_id)
);

CREATE TABLE quizzes (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_id          uuid NOT NULL REFERENCES users (id),
    title             text NOT NULL,
    status            quiz_status NOT NULL DEFAULT 'draft',
    starts_at         timestamptz,
    ends_at           timestamptz,
    duration_sec      int,
    max_attempts      int NOT NULL DEFAULT 1,
    shuffle_questions boolean NOT NULL DEFAULT false,
    -- Snapshotted at publish: {fullscreen, focus_tracking, block_clipboard,
    -- max_violations, violation_action}.
    guardrails        jsonb,
    published_at      timestamptz,
    version           int NOT NULL DEFAULT 0,
    created_at        timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE imports (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    quiz_id      uuid NOT NULL REFERENCES quizzes (id) ON DELETE CASCADE,
    uploaded_by  uuid NOT NULL REFERENCES users (id),
    file_ref     text NOT NULL,
    status       import_status NOT NULL DEFAULT 'validating',
    row_count    int,
    error_report jsonb,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE questions (
    id        uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    quiz_id   uuid NOT NULL REFERENCES quizzes (id) ON DELETE CASCADE,
    position  int NOT NULL,
    type      question_type NOT NULL,
    body      jsonb NOT NULL,
    options   jsonb,
    correct   jsonb NOT NULL, -- NEVER serialized to student clients
    points    numeric NOT NULL DEFAULT 1,
    source    question_source NOT NULL DEFAULT 'manual',
    import_id uuid REFERENCES imports (id)
);

CREATE INDEX questions_quiz_id_position_idx ON questions (quiz_id, position);

-- Group assignment is expanded to individual rows at assignment time, so
-- removing a student from a group never silently revokes an assigned quiz.
CREATE TABLE quiz_assignments (
    quiz_id     uuid NOT NULL REFERENCES quizzes (id) ON DELETE CASCADE,
    student_id  uuid NOT NULL REFERENCES users (id),
    assigned_by uuid NOT NULL REFERENCES users (id),
    assigned_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (quiz_id, student_id)
);

CREATE INDEX quiz_assignments_student_id_idx ON quiz_assignments (student_id);

CREATE TABLE attempts (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    quiz_id         uuid NOT NULL REFERENCES quizzes (id),
    student_id      uuid NOT NULL REFERENCES users (id),
    attempt_no      int NOT NULL,
    quiz_version    int NOT NULL, -- pins the snapshot the student saw
    started_at      timestamptz NOT NULL DEFAULT now(),
    -- min(started_at + duration, quiz.ends_at); precomputed so every autosave
    -- and submit validates against this one column.
    deadline_at     timestamptz NOT NULL,
    submitted_at    timestamptz,
    submit_kind     submit_kind,
    score           numeric,
    status          attempt_status NOT NULL DEFAULT 'in_progress',
    violation_count int NOT NULL DEFAULT 0,
    kicked_by       uuid REFERENCES users (id),
    kick_reason     text, -- required when kicked; enforced in the kick transaction
    UNIQUE (quiz_id, student_id, attempt_no)
);

CREATE INDEX attempts_quiz_id_status_idx ON attempts (quiz_id, status);
CREATE INDEX attempts_student_id_idx ON attempts (student_id);

CREATE TABLE attempt_answers (
    attempt_id     uuid NOT NULL REFERENCES attempts (id) ON DELETE CASCADE,
    question_id    uuid NOT NULL REFERENCES questions (id),
    response       jsonb,
    is_correct     boolean,       -- filled by grader
    points_awarded numeric,       -- filled by grader
    time_spent_ms  int NOT NULL DEFAULT 0,
    saved_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (attempt_id, question_id)
);

CREATE TABLE attempt_events (
    id         bigserial PRIMARY KEY,
    attempt_id uuid NOT NULL REFERENCES attempts (id) ON DELETE CASCADE,
    type       text NOT NULL,
    payload    jsonb,
    at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX attempt_events_attempt_id_id_idx ON attempt_events (attempt_id, id);

CREATE TABLE audit_log (
    id            bigserial PRIMARY KEY,
    actor_id      uuid,
    action        text NOT NULL,
    resource_type text NOT NULL,
    resource_id   uuid,
    detail        jsonb, -- includes diff for mutations
    at            timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_log_actor_id_at_idx ON audit_log (actor_id, at);
CREATE INDEX audit_log_resource_idx ON audit_log (resource_type, resource_id);

CREATE TABLE quiz_stats (
    quiz_id       uuid PRIMARY KEY REFERENCES quizzes (id) ON DELETE CASCADE,
    distribution  jsonb,
    mean          numeric,
    median        numeric,
    participation numeric,
    item_analysis jsonb,
    integrity     jsonb,
    computed_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE student_stats (
    student_id            uuid PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    accuracy_trend        jsonb,
    avg_time_per_question numeric,
    completion_rate       numeric,
    topic_strengths       jsonb,
    updated_at            timestamptz NOT NULL DEFAULT now()
);

-- Data safety invariant 5 (docs/03-data-model.md section 5): audit_log and
-- attempt_events are append-only. Enforced with triggers rather than grants
-- so the rule holds even for the table owner.
-- +goose StatementBegin
CREATE FUNCTION forbid_update_delete() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION '% is append-only', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER audit_log_append_only
    BEFORE UPDATE OR DELETE ON audit_log
    FOR EACH ROW EXECUTE FUNCTION forbid_update_delete();

CREATE TRIGGER attempt_events_append_only
    BEFORE UPDATE OR DELETE ON attempt_events
    FOR EACH ROW EXECUTE FUNCTION forbid_update_delete();

-- +goose Down
DROP TRIGGER attempt_events_append_only ON attempt_events;
DROP TRIGGER audit_log_append_only ON audit_log;
DROP FUNCTION forbid_update_delete();

DROP TABLE student_stats;
DROP TABLE quiz_stats;
DROP TABLE audit_log;
DROP TABLE attempt_events;
DROP TABLE attempt_answers;
DROP TABLE attempts;
DROP TABLE quiz_assignments;
DROP TABLE questions;
DROP TABLE imports;
DROP TABLE quizzes;
DROP TABLE group_members;
DROP TABLE groups;
DROP TABLE users;

DROP TYPE import_status;
DROP TYPE attempt_status;
DROP TYPE submit_kind;
DROP TYPE question_source;
DROP TYPE question_type;
DROP TYPE quiz_status;
DROP TYPE user_status;
DROP TYPE user_role;
