package analytics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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
// This brick computes the quiz_stats score summary - distribution, mean,
// median, participation. The per-question item_analysis (p-value,
// point-biserial, option-pick rates) and the integrity summary are a
// follow-up brick and are left NULL for now; student_stats likewise.

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

	res, err := db.ExecContext(ctx,
		`INSERT INTO quiz_stats (quiz_id, distribution, mean, median, participation)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (quiz_id) DO NOTHING`,
		quizID, distribution, mean, median, participation)
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
