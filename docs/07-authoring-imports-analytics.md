# MacQuiz v2 - Authoring, Bulk Import, Analytics

Source: SDD-001 v2.0 sections 7, 12.
Status: implementation baseline.

## 1. One-by-one authoring

Straightforward CRUD against `POST /quizzes/:id/questions`.
Validation on save: a correct answer must exist among the options, points > 0, choice types need 2-8 options.
Questions are reorderable; `position` is a dense integer the API rewrites on reorder.
The editor autosaves so a teacher never loses a half-written question.

## 2. Bulk upload pipeline (asynchronous, transactional)

The file is fully validated before a single question row is written; the commit is all-or-nothing.

1. Template: teacher downloads a CSV/XLSX template with columns `type, question, option_a..option_f, correct, points`; one row per question.
2. Upload: file goes to object storage via a pre-signed URL; the API creates an `imports` row in state `validating` and enqueues a job.
   Limits: 10 MB, 500 rows.
3. Validate: the import worker parses every row and collects errors per row and column (unknown type, missing correct answer, correct answer not among options, duplicate question text within the file, malformed points).
   Nothing is written to `questions` yet.
4. Review: the teacher sees a preview (n rows parsed, m errors, failing rows inline).
   On errors the state is `failed` with a downloadable error report; the teacher fixes and re-uploads.
5. Commit: on a clean file the state becomes `ready`; the teacher confirms and the worker inserts all rows in one transaction, tagged `source='import'` with `import_id` for provenance.
   Imported questions are ordinary questions afterwards, editable like any other.

Why async: parsing 500 rows with images is too slow for a request cycle, and a synchronous import that dies halfway leaves a corrupted quiz.
The job-queue pipeline gives retries, progress reporting, and a clean failure mode.

## 3. Analytics

Three altitudes: student, quiz, teacher.
Student and quiz analytics are visible to the owning teacher and the admin; teacher analytics are admin-only (teachers see their own).

| Level | Metrics | Visible to |
|-------|---------|-----------|
| Per student | Score and percentile per quiz, accuracy trend over time, average time per question, completion rate (assigned vs attempted), strength/weakness by topic tag | Owning teacher, admin, the student (self) |
| Per quiz | Score distribution histogram, mean/median, participation rate, per-question item analysis: difficulty (p-value), discrimination (point-biserial), option-pick rates, average time on question, integrity summary (violations per student, kicked attempts) | Owning teacher, admin |
| Per teacher | Quizzes created/conducted, total student attempts, average participation, average class score, publish-to-results latency | Admin (teacher sees own) |
| Org-wide | Active users, quizzes per week, platform participation, cohort comparisons across groups | Admin |

Score and percentile per quiz is served by `GET /attempts/:id/result` (`Result.percentile`, `server/internal/attempt/results.go`): a percentile-rank derived from the quiz's already-computed `quiz_stats.distribution` histogram (below-bucket count plus half the attempt's own bucket, over the total), not a fresh full-population query - consistent with section 4's "no separate analytics store" rule. It is bucket-granular (10 buckets), not an exact rank, and is `null` until the quiz's `quiz_stats` rollup lands or when the quiz has no points. Strength/weakness by topic tag is not implemented: the schema carries no per-question topic taxonomy to strengthen against (`student_stats.topic_strengths` is always an empty object), so this sub-metric is out of scope for v1 pending a topic-tagging data model, not merely deferred.

## 4. How analytics are computed

No separate analytics store in v1; two mechanisms cover everything:

1. Rollup on close.
   When a quiz closes and grading finishes, a job writes one `quiz_stats` row (distribution buckets, mean, item-analysis JSON) and upserts `student_stats`.
   Dashboards read these precomputed rows, so heavy queries run once, not per page view.
2. Read replica for exploration.
   Ad-hoc admin queries and CSV exports run against a Postgres read replica when one exists, keeping reporting load off the transactional path.
   (On the single-VM deployment, run exports through the worker at low priority instead.)

Item analysis justifies the precomputation: point-biserial correlation per question requires joining every answer with every attempt score.
At close time for a 500-student quiz it is milliseconds; live per dashboard load it would be the slowest query in the system.
