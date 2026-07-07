-- +goose Up
-- Milestone 4 results release (docs/01 open question 1, resolved to its
-- documented default: a per-quiz teacher toggle, defaulting to automatic
-- release at close). release_policy is chosen at publish; results_released_at
-- is the single fact every results read gates on - null means scores and the
-- answer key stay server-internal (docs/08 section 3).
ALTER TABLE quizzes
    ADD COLUMN release_policy text NOT NULL DEFAULT 'auto'
        CHECK (release_policy IN ('auto', 'manual')),
    ADD COLUMN results_released_at timestamptz;

-- +goose Down
ALTER TABLE quizzes
    DROP COLUMN results_released_at,
    DROP COLUMN release_policy;
