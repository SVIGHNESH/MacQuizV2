package analytics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
)

// This file owns the quiz-level rollup-on-close job (docs/07 section 4:
// "When a quiz closes and grading finishes, a job writes one quiz_stats
// row"). It is an inline sweep step in the worker pass, run right after
// GradeSubmitted / ReleaseDueResults, mirroring quiz.ReleaseDueResults: a
// set-based, idempotent pass that re-derives what is due from the rows, so
// any caller - the close job, the boot re-scan, the periodic backstop - can
// run it at any time (docs/02 section 4.6).
//
// It stays inside the analytics boundary (docs/02 section 3): it only reads
// the transactional tables and writes its own rollup table.
//
// This brick computes the whole quiz_stats row: the score summary
// (distribution, mean, median, participation), the per-question
// item_analysis (p-value, point-biserial, option-pick rates, average time),
// and the integrity summary (violations per student, kicked attempts). It
// also upserts the per-student student_stats rows for the quiz's assigned
// students (see rollupStudents), the one cross-quiz rollup: unlike the
// frozen quiz_stats it is recomputed from the live tables on every close.
//
// Semantic choices, pinned here because the read endpoint and the dashboards
// depend on them:
//   - point-biserial is the item-INCLUDED discrimination: each question's
//     correctness is correlated against the student's whole attempt score,
//     not the score with that item removed. It is the standard simple form;
//     it is null when the correlation is undefined (every responder right or
//     every responder wrong, or no spread in total scores).
//   - p-value's denominator is the responders who ANSWERED the question, not
//     everyone who attempted the quiz: a skipped question does not count
//     against its difficulty.
//   - option-pick rates key on the raw response text (`response #>> '{}'`),
//     so a `multi` question's rates are keyed by the whole selected set, not
//     per option. That is a known first-brick limitation.
// item_analysis is computed over each student's BEST graded attempt, the
// same population the distribution uses. integrity instead spans EVERY
// attempt of the quiz, since a kick or a violation is an event to be counted
// wherever it happened, not only on a student's best-scoring try.

// distributionBuckets is the number of equal-width percentage buckets the
// score-distribution histogram uses: bucket i covers [i*10, (i+1)*10)% of
// the attempt's max score, with a perfect 100% folded into the last bucket.
const distributionBuckets = 10

// RollupDue computes and stores the quiz_stats row for every quiz that has
// closed and finished grading but has no rollup yet, and returns how many
// rows it wrote. "Finished grading" is the same guard ReleaseDueResults
// uses, tightened to exclude every non-graded attempt state (a kicked
// attempt whose grading has not yet landed still reads 'kicked'): a rollup
// is computed once and frozen, so it must never run against a quiz whose
// scores are still moving. A closed quiz takes no new attempts and its
// grading is deterministic, so once every attempt is graded the rollup is
// final - no recompute path is needed.
//
// A closed quiz with zero attempts still gets a row (null mean/median, empty
// distribution, zero participation) so it does not stay perpetually "due"
// and recompute every sweep forever.
func RollupDue(ctx context.Context, db *sql.DB) (int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT z.id FROM quizzes z
		 WHERE z.status IN ('closed', 'archived')
		   AND NOT EXISTS (
		       SELECT 1 FROM attempts a
		       WHERE a.quiz_id = z.id AND a.status <> 'graded')
		   AND NOT EXISTS (
		       SELECT 1 FROM quiz_stats s WHERE s.quiz_id = z.id)`)
	if err != nil {
		return 0, fmt.Errorf("list quizzes due for rollup: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("scan due quiz: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("list quizzes due for rollup: %w", err)
	}

	var written int64
	for _, id := range ids {
		did, err := rollupOne(ctx, db, id)
		if err != nil {
			return written, fmt.Errorf("roll up quiz %s: %w", id, err)
		}
		if did {
			written++
		}
	}
	return written, nil
}

// rollupOne computes one quiz's score summary and inserts its quiz_stats row,
// reporting false without error when a concurrent worker got there first. The
// insert is ON CONFLICT DO NOTHING because River runs workers concurrently:
// two passes can both see the quiz as due and both compute it, and exactly
// one row must land.
func rollupOne(ctx context.Context, db *sql.DB, quizID string) (bool, error) {
	// One score per student: their best graded attempt. max_attempts is
	// usually 1 so this is typically moot, but where a student has several
	// graded attempts the distribution should reflect their best result, not
	// double-count them. max_score is the sum of the pinned snapshot's
	// question points (there is no max_score column; a republish can change
	// it, so it is read per attempt from the version the student saw).
	scoreRows, err := db.QueryContext(ctx,
		`SELECT DISTINCT ON (a.student_id)
		        a.score::float8,
		        (SELECT sum((q->>'points')::float8)
		         FROM quiz_versions v, jsonb_array_elements(v.questions) q
		         WHERE v.quiz_id = a.quiz_id AND v.version = a.quiz_version) AS max_score
		 FROM attempts a
		 WHERE a.quiz_id = $1 AND a.status = 'graded'
		 ORDER BY a.student_id, a.score DESC NULLS LAST`, quizID)
	if err != nil {
		return false, fmt.Errorf("load graded scores: %w", err)
	}
	defer scoreRows.Close()

	var scores []float64
	buckets := make([]int, distributionBuckets)
	for scoreRows.Next() {
		var score, maxScore sql.NullFloat64
		if err := scoreRows.Scan(&score, &maxScore); err != nil {
			return false, fmt.Errorf("scan graded score: %w", err)
		}
		if !score.Valid {
			continue
		}
		scores = append(scores, score.Float64)
		buckets[bucketIndex(score.Float64, maxScore)]++
	}
	if err := scoreRows.Err(); err != nil {
		return false, fmt.Errorf("load graded scores: %w", err)
	}

	var assigned, attempted int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM quiz_assignments WHERE quiz_id = $1`, quizID).Scan(&assigned); err != nil {
		return false, fmt.Errorf("count assignments: %w", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT count(DISTINCT student_id) FROM attempts WHERE quiz_id = $1`, quizID).Scan(&attempted); err != nil {
		return false, fmt.Errorf("count attempted: %w", err)
	}

	mean, median := meanMedian(scores)
	var participation float64
	if assigned > 0 {
		participation = float64(attempted) / float64(assigned)
	}
	distribution, err := json.Marshal(buckets)
	if err != nil {
		return false, fmt.Errorf("marshal distribution: %w", err)
	}

	items, err := itemAnalysis(ctx, db, quizID)
	if err != nil {
		return false, err
	}
	itemJSON, err := json.Marshal(items)
	if err != nil {
		return false, fmt.Errorf("marshal item_analysis: %w", err)
	}
	integrity, err := integritySummary(ctx, db, quizID)
	if err != nil {
		return false, err
	}
	integrityJSON, err := json.Marshal(integrity)
	if err != nil {
		return false, fmt.Errorf("marshal integrity: %w", err)
	}

	// Recompute the per-student rollups BEFORE writing the quiz_stats row, so
	// the quiz_stats INSERT is the commit marker: if this errors, no quiz_stats
	// row lands, the quiz stays "due", and the next sweep safely redoes both
	// rather than stranding student_stats stale forever.
	if err := rollupStudents(ctx, db, quizID); err != nil {
		return false, err
	}

	res, err := db.ExecContext(ctx,
		`INSERT INTO quiz_stats (quiz_id, distribution, mean, median, participation, item_analysis, integrity)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 ON CONFLICT (quiz_id) DO NOTHING`,
		quizID, distribution, mean, median, participation, itemJSON, integrityJSON)
	if err != nil {
		return false, fmt.Errorf("insert quiz_stats: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count quiz_stats insert: %w", err)
	}
	return n == 1, nil
}

// bucketIndex maps one attempt's score to its percentage-of-max distribution
// bucket. A quiz whose snapshot carries no points (max_score null or zero)
// has no meaningful percentage, so every score lands in the lowest bucket.
func bucketIndex(score float64, maxScore sql.NullFloat64) int {
	if !maxScore.Valid || maxScore.Float64 <= 0 {
		return 0
	}
	idx := int(score / maxScore.Float64 * distributionBuckets)
	if idx < 0 {
		idx = 0
	}
	if idx >= distributionBuckets {
		idx = distributionBuckets - 1 // fold a perfect 100% into the top bucket
	}
	return idx
}

// meanMedian returns the mean and median of the per-student scores as
// nullable numerics: both are NULL when no student has a graded attempt, so
// an empty quiz reads as "no data" rather than a misleading zero.
func meanMedian(scores []float64) (mean, median sql.NullFloat64) {
	if len(scores) == 0 {
		return sql.NullFloat64{}, sql.NullFloat64{}
	}
	var sum float64
	for _, s := range scores {
		sum += s
	}
	mean = sql.NullFloat64{Float64: sum / float64(len(scores)), Valid: true}

	sorted := append([]float64(nil), scores...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		median = sql.NullFloat64{Float64: sorted[mid], Valid: true}
	} else {
		median = sql.NullFloat64{Float64: (sorted[mid-1] + sorted[mid]) / 2, Valid: true}
	}
	return mean, median
}

// itemStat is one question's row in the item_analysis array (docs/07 section
// 3). responses is how many counted attempts answered it; p_value is the
// fraction of those answers that were correct (difficulty); point_biserial is
// the item-included discrimination, null when undefined; option_pick_rates
// tallies the raw response text; avg_time_ms is the mean time_spent_ms.
type itemStat struct {
	QuestionID      string         `json:"question_id"`
	Responses       int            `json:"responses"`
	PValue          float64        `json:"p_value"`
	PointBiserial   *float64       `json:"point_biserial"`
	OptionPickRates map[string]int `json:"option_pick_rates"`
	AvgTimeMs       float64        `json:"avg_time_ms"`
}

// itemAnalysis builds the per-question item_analysis over each student's best
// graded attempt - the same population the distribution uses, so difficulty
// and discrimination describe the same set of results the histogram does. It
// returns a possibly-empty (never nil) slice so a quiz with no answers rolls
// up an empty array rather than a null.
func itemAnalysis(ctx context.Context, db *sql.DB, quizID string) ([]itemStat, error) {
	rows, err := db.QueryContext(ctx,
		`WITH best AS (
		     SELECT DISTINCT ON (a.student_id) a.id, a.score::float8 AS total
		     FROM attempts a
		     WHERE a.quiz_id = $1 AND a.status = 'graded'
		     ORDER BY a.student_id, a.score DESC NULLS LAST)
		 SELECT aa.question_id::text, aa.is_correct, aa.time_spent_ms,
		        aa.response #>> '{}', b.total
		 FROM best b
		 JOIN attempt_answers aa ON aa.attempt_id = b.id`, quizID)
	if err != nil {
		return nil, fmt.Errorf("load item answers: %w", err)
	}
	defer rows.Close()

	// Accumulate per question in first-seen-free maps, then emit in a stable
	// question-id order. (Snapshot question order would read better on a
	// dashboard; it is deferred because attempts can pin different versions.)
	type acc struct {
		correct   []float64 // 1/0 per answer that has a graded correctness
		totals    []float64 // the answering attempt's total score, paired with correct
		picks     map[string]int
		timeSum   float64
		responses int
	}
	byQ := map[string]*acc{}
	for rows.Next() {
		var qid, respText sql.NullString
		var isCorrect sql.NullBool
		var timeMs sql.NullInt64
		var total sql.NullFloat64
		if err := rows.Scan(&qid, &isCorrect, &timeMs, &respText, &total); err != nil {
			return nil, fmt.Errorf("scan item answer: %w", err)
		}
		a := byQ[qid.String]
		if a == nil {
			a = &acc{picks: map[string]int{}}
			byQ[qid.String] = a
		}
		a.responses++
		a.timeSum += float64(timeMs.Int64)
		if respText.Valid {
			a.picks[respText.String]++
		}
		if isCorrect.Valid {
			c := 0.0
			if isCorrect.Bool {
				c = 1
			}
			a.correct = append(a.correct, c)
			a.totals = append(a.totals, total.Float64)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load item answers: %w", err)
	}

	qids := make([]string, 0, len(byQ))
	for qid := range byQ {
		qids = append(qids, qid)
	}
	sort.Strings(qids)

	items := make([]itemStat, 0, len(qids))
	for _, qid := range qids {
		a := byQ[qid]
		stat := itemStat{
			QuestionID:      qid,
			Responses:       a.responses,
			OptionPickRates: a.picks,
			PointBiserial:   pearson(a.correct, a.totals),
		}
		if a.responses > 0 {
			stat.AvgTimeMs = a.timeSum / float64(a.responses)
		}
		if n := len(a.correct); n > 0 {
			var sum float64
			for _, c := range a.correct {
				sum += c
			}
			stat.PValue = sum / float64(n)
		}
		items = append(items, stat)
	}
	return items, nil
}

// pearson returns the Pearson correlation of two equal-length series, or nil
// when it is undefined - fewer than two points, or no spread in either series
// (every value identical). For item analysis xs is the 1/0 correctness and ys
// the paired total scores, so a null means the question fails to discriminate
// (all right, all wrong, or no score spread among responders).
func pearson(xs, ys []float64) *float64 {
	n := len(xs)
	if n < 2 {
		return nil
	}
	var mx, my float64
	for i := range xs {
		mx += xs[i]
		my += ys[i]
	}
	mx /= float64(n)
	my /= float64(n)
	var cov, vx, vy float64
	for i := range xs {
		dx := xs[i] - mx
		dy := ys[i] - my
		cov += dx * dy
		vx += dx * dx
		vy += dy * dy
	}
	if vx == 0 || vy == 0 {
		return nil
	}
	r := cov / math.Sqrt(vx*vy)
	return &r
}

// integrityStudent is one student's line in the integrity summary: their
// total violation count across attempts and whether any attempt was kicked.
type integrityStudent struct {
	StudentID  string `json:"student_id"`
	Violations int    `json:"violations"`
	Kicked     bool   `json:"kicked"`
}

// integrity is the quiz_stats integrity summary (docs/07 section 3):
// violations per student and the kicked-attempt count.
type integrity struct {
	KickedAttempts  int                `json:"kicked_attempts"`
	FlaggedStudents int                `json:"flagged_students"`
	TotalViolations int                `json:"total_violations"`
	PerStudent      []integrityStudent `json:"per_student"`
}

// integritySummary tallies violations and kicks across EVERY attempt of the
// quiz. A kicked attempt is one whose submit_kind is 'kicked': grading flips a
// kicked attempt's status to 'graded', so status can no longer identify it -
// only the immutable submit_kind can. per_student lists only students who were
// flagged or kicked, in a stable student-id order.
func integritySummary(ctx context.Context, db *sql.DB, quizID string) (integrity, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT a.student_id::text, a.violation_count, (a.submit_kind = 'kicked')
		 FROM attempts a WHERE a.quiz_id = $1`, quizID)
	if err != nil {
		return integrity{}, fmt.Errorf("load integrity attempts: %w", err)
	}
	defer rows.Close()

	perStudent := map[string]*integrityStudent{}
	out := integrity{PerStudent: []integrityStudent{}}
	for rows.Next() {
		var sid string
		var violations int
		var kicked bool
		if err := rows.Scan(&sid, &violations, &kicked); err != nil {
			return integrity{}, fmt.Errorf("scan integrity attempt: %w", err)
		}
		out.TotalViolations += violations
		if kicked {
			out.KickedAttempts++
		}
		s := perStudent[sid]
		if s == nil {
			s = &integrityStudent{StudentID: sid}
			perStudent[sid] = s
		}
		s.Violations += violations
		s.Kicked = s.Kicked || kicked
	}
	if err := rows.Err(); err != nil {
		return integrity{}, fmt.Errorf("load integrity attempts: %w", err)
	}

	sids := make([]string, 0, len(perStudent))
	for sid, s := range perStudent {
		if s.Violations > 0 || s.Kicked {
			sids = append(sids, sid)
		}
	}
	sort.Strings(sids)
	for _, sid := range sids {
		s := perStudent[sid]
		if s.Violations > 0 {
			out.FlaggedStudents++
		}
		out.PerStudent = append(out.PerStudent, *s)
	}
	return out, nil
}

// rollupStudents recomputes and upserts the student_stats row for every
// student ASSIGNED to the just-closed quiz (docs/07 section 4: closing "upserts
// student_stats"). Unlike quiz_stats - computed once and frozen - a student's
// stats span every quiz they take, so this is a full recompute (ON CONFLICT DO
// UPDATE) re-read from the live tables each close; two concurrent rollups both
// read the same rows and converge to identical data. It is keyed on the
// ASSIGNED set, not the attempters, so a no-show's completion_rate still drops
// when their quiz closes - only the assignment row proves the miss.
//
// One set-based statement, not a per-student Go loop: a 1,000-student quiz
// closing must not fan out into 1,000 synchronous query batches inside the
// sweep (docs/12 load-tests the go-live herd).
//
// Population: each student's BEST graded attempt per terminal (closed or
// archived) quiz - the same graded population quiz_stats uses, so a kick's
// partial score (grading flips kicked -> graded) lands in the trend like any
// other graded result. accuracy_trend is that series of per-quiz accuracies
// (score over the version-pinned max score, null when the snapshot has no
// points) ordered by submission; avg_time_per_question the mean answer time
// over those best attempts; completion_rate the fraction of the student's
// terminal assigned quizzes they have a graded attempt for.
//
// topic_strengths (docs/07 section 3, "strength/weakness by topic tag") is the
// student's accuracy per questions.topic over that same best-attempt
// population, read from the VERSION SNAPSHOT rather than the live questions
// table: the tag frozen into quiz_versions is the one the student sat, so
// retagging or deleting a question after a quiz closes cannot rewrite a past
// result. Two rules it inherits from the sibling metrics:
//   - only ANSWERED questions count, the denominator itemAnalysis' p-value
//     already uses, so skipping a topic's question is not evidence of weakness
//     in that topic;
//   - correctness is the boolean is_correct, unweighted by points, so a topic
//     reads "how often you were right", not "how many marks you earned".
//
// It stays an empty object (never null) for a student none of whose questions
// carried a tag: the taxonomy is optional per question, and pre-topic version
// snapshots have no 'topic' key at all.
func rollupStudents(ctx context.Context, db *sql.DB, quizID string) error {
	if _, err := db.ExecContext(ctx, `
WITH affected AS (
    SELECT DISTINCT student_id FROM quiz_assignments WHERE quiz_id = $1
),
best AS (
    SELECT DISTINCT ON (a.student_id, a.quiz_id)
           a.student_id, a.quiz_id, a.id AS attempt_id, a.quiz_version,
           a.score::float8 AS score, a.submitted_at
    FROM attempts a
    JOIN affected f ON f.student_id = a.student_id
    JOIN quizzes z ON z.id = a.quiz_id
    WHERE a.status = 'graded' AND z.status IN ('closed', 'archived')
    ORDER BY a.student_id, a.quiz_id, a.score DESC NULLS LAST
),
trend AS (
    SELECT b.student_id,
           jsonb_agg(jsonb_build_object(
               'quiz_id', b.quiz_id,
               'accuracy', CASE WHEN ms.max_score > 0 THEN b.score / ms.max_score END,
               'submitted_at', b.submitted_at
           ) ORDER BY b.submitted_at, b.quiz_id) AS trend
    FROM best b
    LEFT JOIN LATERAL (
        SELECT sum((q->>'points')::float8) AS max_score
        FROM quiz_versions v, jsonb_array_elements(v.questions) q
        WHERE v.quiz_id = b.quiz_id AND v.version = b.quiz_version
    ) ms ON true
    GROUP BY b.student_id
),
tm AS (
    SELECT b.student_id, avg(aa.time_spent_ms::float8) AS avg_time
    FROM best b JOIN attempt_answers aa ON aa.attempt_id = b.attempt_id
    GROUP BY b.student_id
),
assigned_terminal AS (
    SELECT DISTINCT qa.student_id, qa.quiz_id
    FROM quiz_assignments qa
    JOIN affected f ON f.student_id = qa.student_id
    JOIN quizzes z ON z.id = qa.quiz_id
    WHERE z.status IN ('closed', 'archived')
),
cp AS (
    SELECT t.student_id,
           count(*) AS assigned_ct,
           count(b.quiz_id) AS done_ct
    FROM assigned_terminal t
    LEFT JOIN best b ON b.student_id = t.student_id AND b.quiz_id = t.quiz_id
    GROUP BY t.student_id
),
topic_answers AS (
    SELECT b.student_id, q->>'topic' AS topic, aa.is_correct
    FROM best b
    JOIN quiz_versions v ON v.quiz_id = b.quiz_id AND v.version = b.quiz_version
    CROSS JOIN LATERAL jsonb_array_elements(v.questions) q
    JOIN attempt_answers aa
      ON aa.attempt_id = b.attempt_id AND aa.question_id = (q->>'id')::uuid
    WHERE q->>'topic' IS NOT NULL AND aa.is_correct IS NOT NULL
),
ts AS (
    SELECT student_id,
           jsonb_object_agg(topic, accuracy) AS topics
    FROM (
        SELECT student_id, topic,
               round(avg(is_correct::int)::numeric, 4) AS accuracy
        FROM topic_answers
        GROUP BY student_id, topic
    ) per_topic
    GROUP BY student_id
)
INSERT INTO student_stats
       (student_id, accuracy_trend, avg_time_per_question, completion_rate, topic_strengths, updated_at)
SELECT f.student_id,
       coalesce(tr.trend, '[]'::jsonb),
       tm.avg_time,
       CASE WHEN cp.assigned_ct > 0 THEN cp.done_ct::numeric / cp.assigned_ct END,
       coalesce(ts.topics, '{}'::jsonb),
       now()
FROM affected f
LEFT JOIN trend tr ON tr.student_id = f.student_id
LEFT JOIN tm ON tm.student_id = f.student_id
LEFT JOIN cp ON cp.student_id = f.student_id
LEFT JOIN ts ON ts.student_id = f.student_id
ON CONFLICT (student_id) DO UPDATE SET
    accuracy_trend        = EXCLUDED.accuracy_trend,
    avg_time_per_question = EXCLUDED.avg_time_per_question,
    completion_rate       = EXCLUDED.completion_rate,
    topic_strengths       = EXCLUDED.topic_strengths,
    updated_at            = now()`, quizID); err != nil {
		return fmt.Errorf("upsert student_stats: %w", err)
	}
	return nil
}
