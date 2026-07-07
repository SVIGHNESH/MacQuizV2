package attempt

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// This file owns the attempt_events source-of-truth layer (docs/05 sections 1
// and 2). Every lifecycle write appends its event row in the same transaction
// as the state change it records: "persist first, publish second - the event
// row is the source of truth; the publish is best-effort delivery". The Redis
// publish and the WebSocket fan-out are separate bricks that layer on top; the
// live-roster snapshot (quiz.LiveRoster) already promises to be reconciled
// against this stream, so the rows must exist before the socket can replay
// them. attempt_events is append-only (schema trigger forbid_update_delete),
// so this module only ever INSERTs.

// The docs/05 section 2 event vocabulary. Only the events the server can
// source from a REST/worker write are emitted here; disconnected, reconnected,
// violation, and kicked arrive with the heartbeat, guardrail, and kick bricks
// that do not exist yet.
const (
	eventStarted   = "attempt.started"
	eventProgress  = "attempt.progress"
	eventSubmitted = "attempt.submitted"
	eventGraded    = "attempt.graded"
)

// startedPayload is the attempt.started delta: the dashboard moves the row to
// "in progress" and starts its countdown from deadline_at (docs/05 section 2).
type startedPayload struct {
	StudentID  string    `json:"student_id"`
	AttemptID  string    `json:"attempt_id"`
	DeadlineAt time.Time `json:"deadline_at"`
}

// progressPayload is the attempt.progress delta. CurrentQuestion is always
// null from a REST autosave: no server column tracks the student's cursor (the
// same honest degradation quiz.LiveRow documents), so the dashboard advances
// only the answered count until the attempt socket can carry a real position.
type progressPayload struct {
	CurrentQuestion *int `json:"current_question"`
	AnsweredCount   int  `json:"answered_count"`
}

// submittedPayload is the attempt.submitted delta: the row moves to
// "submitted" and the summary counters update. SubmitKind distinguishes the
// student's manual submit from the auto/forced terminations the sweep applies.
type submittedPayload struct {
	SubmitKind    string `json:"submit_kind"`
	AnsweredCount int    `json:"answered_count"`
}

// gradedPayload is the attempt.graded delta. The score is recorded whenever it
// is computed; whether a client may see it is a read-side concern the results
// release policy owns (docs/04 section 4), not the event log's.
type gradedPayload struct {
	Score float64 `json:"score"`
}

// execer abstracts *sql.Tx (and, for the sweep's per-row inserts, anything
// that can exec) so appendEvent can run inside whatever transaction owns the
// state change.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// EventPublisher relays a committed attempt event to subscribers (docs/05
// section 1: "persist first, publish second"). The attempt module owns the
// persist - appendEvent writes the source-of-truth row inside the state
// change's transaction - and hands the same delta to a publisher only after
// that transaction commits. realtime.Publisher is the concrete relay onto
// Redis pub/sub; the interface keeps this module from importing go-redis and
// gives tests a capture seam. Publish is best-effort and returns nothing: the
// row is already durable and the live snapshot reconciles any lost delta, so a
// publish failure must never affect the request that triggered it.
type EventPublisher interface {
	Publish(ctx context.Context, quizID, attemptID, eventType string, payload any)
}

// noopPublisher is the default relay: every test that does not exercise the
// socket, and any deploy that has not wired Redis, gets a publisher that
// drops every event. Callers publish unconditionally against it.
type noopPublisher struct{}

func (noopPublisher) Publish(context.Context, string, string, string, any) {}

// resolvePublisher picks the wired relay or the no-op fallback, so every emit
// site can call Publish without a nil guard. The variadic keeps the many
// existing NewService/SweepDueAttempts/GradeSubmitted callers - none of which
// relay - compiling unchanged.
func resolvePublisher(publishers []EventPublisher) EventPublisher {
	if len(publishers) > 0 && publishers[0] != nil {
		return publishers[0]
	}
	return noopPublisher{}
}

// appendEvent writes one attempt_events row inside the caller's transaction.
// It marshals the typed payload rather than accepting raw JSON so the event
// shape is checked at compile time, and it never updates or deletes: the
// append-only trigger would reject that, and the log's whole value is that it
// is immutable.
func appendEvent(ctx context.Context, tx execer, attemptID, eventType string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal %s payload: %w", eventType, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO attempt_events (attempt_id, type, payload) VALUES ($1, $2, $3)`,
		attemptID, eventType, raw); err != nil {
		return fmt.Errorf("append %s event: %w", eventType, err)
	}
	return nil
}

// countAnswered is the answered_count carried by the progress and submitted
// events, defined exactly as quiz.LiveRoster defines it - responses that are
// non-null - so the streamed delta and the snapshot cell can never disagree.
func countAnswered(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}, attemptID string) (int, error) {
	var n int
	if err := q.QueryRowContext(ctx,
		`SELECT count(*) FROM attempt_answers
		 WHERE attempt_id = $1 AND response IS NOT NULL`, attemptID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count answered: %w", err)
	}
	return n, nil
}
