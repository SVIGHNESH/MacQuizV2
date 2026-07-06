package attempt

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// This file owns grading (docs/04 section 4, docs/12 Milestone 4): a
// deterministic, idempotent pass that scores a submitted attempt against the
// answer key of the exact snapshot version the student saw.
//
// Grading is a pure function of (snapshot, answers) - docs/03 invariant 4 -
// so rerunning it is always safe. The terminal write is guarded by
// status = 'submitted', the same single-guard pattern as the submit funnel,
// so a job firing twice, late, or concurrently with the backstop sweep can
// never double-grade.
//
// Trigger paths: the manual submit leg enqueues GradeArgs inside its own
// transaction; the batch legs (auto, forced) need no job at all, because the
// worker pass that sweeps them runs GradeSubmitted right after - and the boot
// re-scan plus the periodic backstop heal any grading that was missed.

// GradeArgs is the per-attempt grading job enqueued inside the submit
// transaction, so an attempt can never be submitted without its grading and
// a failed submit leaves no orphan job.
type GradeArgs struct {
	AttemptID string `json:"attempt_id"`
}

// Kind names the job type in the queue.
func (GradeArgs) Kind() string { return "grade_attempt" }

// enqueueGradeJob inserts the attempt's grading job inside the submitting
// transaction (docs/04 section 4: "submission enqueues a grading job").
func (s *Service) enqueueGradeJob(ctx context.Context, tx *sql.Tx, attemptID string) error {
	if _, err := s.jobs.InsertTx(ctx, tx, GradeArgs{AttemptID: attemptID}, nil); err != nil {
		return fmt.Errorf("enqueue grade job: %w", err)
	}
	return nil
}

// GradeSubmitted grades every attempt sitting in status = 'submitted', each
// in its own transaction, and returns how many were graded. Like the due
// sweeps, it re-derives what needs work from the rows themselves, so any
// caller - the grade job, the close pass, the boot re-scan, the periodic
// backstop - can run it at any time.
func GradeSubmitted(ctx context.Context, db *sql.DB) (int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT id FROM attempts WHERE status = 'submitted' ORDER BY submitted_at, id`)
	if err != nil {
		return 0, fmt.Errorf("list submitted attempts: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return 0, fmt.Errorf("scan submitted attempt: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("list submitted attempts: %w", err)
	}

	var graded int64
	for _, id := range ids {
		did, err := gradeOne(ctx, db, id)
		if err != nil {
			return graded, fmt.Errorf("grade attempt %s: %w", id, err)
		}
		if did {
			graded++
		}
	}
	return graded, nil
}

// gradeOne scores a single attempt. It reports false without error when the
// attempt is no longer in status = 'submitted' - a concurrent grader got
// there first, which is exactly the idempotence the funnel promises.
func gradeOne(ctx context.Context, db *sql.DB, attemptID string) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin grade tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var quizID string
	var version int
	err = tx.QueryRowContext(ctx,
		`SELECT quiz_id, quiz_version FROM attempts
		 WHERE id = $1 AND status = 'submitted' FOR UPDATE`, attemptID).Scan(&quizID, &version)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("lock attempt: %w", err)
	}

	// The answer key comes from the snapshot the attempt pinned, never the
	// live quiz row: a republish after this student submitted cannot change
	// what their work is graded against.
	var questionsJSON []byte
	if err := tx.QueryRowContext(ctx,
		`SELECT questions FROM quiz_versions WHERE quiz_id = $1 AND version = $2`,
		quizID, version).Scan(&questionsJSON); err != nil {
		return false, fmt.Errorf("load snapshot: %w", err)
	}
	questions, err := decodeSnapshot(questionsJSON)
	if err != nil {
		return false, err
	}

	responses := map[string]json.RawMessage{}
	rows, err := tx.QueryContext(ctx,
		`SELECT question_id, response FROM attempt_answers WHERE attempt_id = $1`, attemptID)
	if err != nil {
		return false, fmt.Errorf("load answers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var qid string
		var response []byte
		if err := rows.Scan(&qid, &response); err != nil {
			return false, fmt.Errorf("scan answer: %w", err)
		}
		responses[qid] = response
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("load answers: %w", err)
	}

	var score float64
	for _, q := range questions {
		response, answered := responses[q.ID]
		if !answered {
			// An unanswered question simply contributes nothing; there is no
			// row to mark, and the snapshot documents what was skipped.
			continue
		}
		correct, awarded := gradeQuestion(q, response)
		score += awarded
		if _, err := tx.ExecContext(ctx,
			`UPDATE attempt_answers SET is_correct = $1, points_awarded = $2
			 WHERE attempt_id = $3 AND question_id = $4`,
			correct, awarded, attemptID, q.ID); err != nil {
			return false, fmt.Errorf("mark answer: %w", err)
		}
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE attempts SET score = $1, status = 'graded'
		 WHERE id = $2 AND status = 'submitted'`, score, attemptID)
	if err != nil {
		return false, fmt.Errorf("mark graded: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count graded: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit grade: %w", err)
	}
	return n == 1, nil
}

// gradeQuestion scores one response against one snapshot question. A response
// that does not decode into the shape its type expects is simply wrong - the
// autosave endpoint accepts opaque JSON, so the grader is where malformed
// input lands, and it must never error an entire attempt over one bad answer.
func gradeQuestion(q Question, response json.RawMessage) (correct bool, awarded float64) {
	switch q.Type {
	case "single":
		var got, want string
		correct = json.Unmarshal(response, &got) == nil &&
			json.Unmarshal(q.Correct, &want) == nil && got == want
	case "multi":
		// All-or-nothing set equality: docs/04 defines no partial credit, and
		// awarding it silently would overstate scores.
		var got, want []string
		correct = json.Unmarshal(response, &got) == nil &&
			json.Unmarshal(q.Correct, &want) == nil && sameKeySet(got, want)
	case "truefalse":
		var got, want bool
		correct = json.Unmarshal(response, &got) == nil &&
			json.Unmarshal(q.Correct, &want) == nil && got == want
	case "short":
		var sc shortAnswerKey
		if json.Unmarshal(q.Correct, &sc) == nil {
			correct = matchesShortAnswer(response, sc.Accepted)
		}
	}
	if correct {
		awarded = q.Points
	}
	return correct, awarded
}

// shortAnswerKey mirrors quiz.shortCorrect: the accepted answers a short
// response is matched against.
type shortAnswerKey struct {
	Accepted []string `json:"accepted"`
}

// sameKeySet reports whether two key lists select the same set. The stored
// key is validated distinct at authoring time; duplicates in the response are
// collapsed, so ["a","a"] neither cheats nor double-counts.
func sameKeySet(got, want []string) bool {
	if len(want) == 0 {
		return false
	}
	gotSet := map[string]bool{}
	for _, k := range got {
		gotSet[k] = true
	}
	if len(gotSet) != len(want) {
		return false
	}
	for _, k := range want {
		if !gotSet[k] {
			return false
		}
	}
	return true
}

// matchesShortAnswer implements the docs/04 short-answer rule: "normalized
// exact/numeric matching". The response may arrive as a JSON string or a bare
// JSON number (a numeric input widget saves 5, a text one saves "5"); both
// normalize into the same comparison.
func matchesShortAnswer(response json.RawMessage, accepted []string) bool {
	var text string
	if json.Unmarshal(response, &text) != nil {
		var num float64
		if json.Unmarshal(response, &num) != nil {
			return false
		}
		text = strconv.FormatFloat(num, 'f', -1, 64)
	}
	got := normalizeShort(text)
	gotNum, gotErr := strconv.ParseFloat(got, 64)
	for _, a := range accepted {
		want := normalizeShort(a)
		if got == want {
			return true
		}
		// Numeric equivalence: "5.0" matches an accepted "5".
		if gotErr == nil {
			if wantNum, err := strconv.ParseFloat(want, 64); err == nil && gotNum == wantNum {
				return true
			}
		}
	}
	return false
}

// normalizeShort is the normalization side of "normalized exact" matching:
// case-insensitive, trimmed, inner whitespace runs collapsed to one space.
func normalizeShort(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}
