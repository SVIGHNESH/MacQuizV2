-- +goose Up
-- docs/07-authoring-imports-analytics.md section 3 lists "strength/weakness by
-- topic tag" among the per-student metrics, but the schema carried no per-
-- question taxonomy to strengthen against, so student_stats.topic_strengths
-- was pinned to an empty object. topic is that taxonomy in its smallest
-- honest form: a free-text tag the author writes on the question, nullable
-- because an untagged question simply contributes to no topic. It is copied
-- into the quiz_versions snapshot on publish, so a student's topic strengths
-- are computed against the tags frozen in the version they actually sat -
-- retagging a question after a quiz closes cannot rewrite history.
ALTER TABLE questions
    ADD COLUMN topic text CHECK (topic IS NULL OR (length(topic) BETWEEN 1 AND 60));

-- +goose Down
ALTER TABLE questions
    DROP COLUMN topic;
