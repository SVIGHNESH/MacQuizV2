-- +goose Up
-- Negative marking: a quiz carries a global marking scheme (marks a question
-- earns, marks a wrong answer costs) and a question may override either.
-- questions.points/penalty are NULL when the question inherits the quiz
-- default; publish resolves the effective values into the version snapshot,
-- so grading and old attempts never see the indirection.
ALTER TABLE quizzes
    ADD COLUMN default_points  numeric NOT NULL DEFAULT 1
        CHECK (default_points > 0 AND default_points <= 1000),
    ADD COLUMN default_penalty numeric NOT NULL DEFAULT 0
        CHECK (default_penalty >= 0 AND default_penalty <= 1000);

ALTER TABLE questions
    ALTER COLUMN points DROP NOT NULL,
    ALTER COLUMN points DROP DEFAULT,
    ADD CONSTRAINT questions_points_range
        CHECK (points IS NULL OR (points > 0 AND points <= 1000)),
    ADD COLUMN penalty numeric
        CHECK (penalty IS NULL OR (penalty >= 0 AND penalty <= 1000));

-- +goose Down
UPDATE questions SET points = 1 WHERE points IS NULL;
ALTER TABLE questions
    DROP COLUMN penalty,
    DROP CONSTRAINT questions_points_range,
    ALTER COLUMN points SET DEFAULT 1,
    ALTER COLUMN points SET NOT NULL;
ALTER TABLE quizzes
    DROP COLUMN default_penalty,
    DROP COLUMN default_points;
