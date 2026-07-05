-- +goose Up
-- Milestone 3 publish (docs/03-data-model.md section 4): publishing copies
-- the question set and the guardrail config into an immutable version, so a
-- mid-window edit or republish can never change what a student who already
-- started actually sees. Attempts pin quiz_version, and grading is a pure
-- function of (snapshot, answers).
CREATE TABLE quiz_versions (
    quiz_id    uuid NOT NULL REFERENCES quizzes (id) ON DELETE CASCADE,
    version    int  NOT NULL,
    -- Full question rows in position order, INCLUDING the answer key. This
    -- column is server-internal: it feeds attempt delivery (stripped via the
    -- serializer boundary) and the grader; it is never serialized whole.
    questions  jsonb NOT NULL,
    guardrails jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (quiz_id, version)
);

-- Snapshots are write-once: a republish inserts version n+1 and leaves every
-- earlier version untouched. Reuses the append-only enforcement function
-- from the initial schema.
CREATE TRIGGER quiz_versions_append_only
    BEFORE UPDATE OR DELETE ON quiz_versions
    FOR EACH ROW EXECUTE FUNCTION forbid_update_delete();

-- +goose Down
DROP TRIGGER quiz_versions_append_only ON quiz_versions;
DROP TABLE quiz_versions;
