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

// The docs/05 section 2 event vocabulary. Most are sourced from a REST/worker
// write; attempt.disconnected/attempt.reconnected are the exception - they
// originate in the realtime gateway's heartbeat tracking on the attempt:{id}
// socket (docs/05 section 5), not a request transaction, so they are logged
// through LogAttemptDisconnected/LogAttemptReconnected instead of appendEvent
// being called inline from a handler.
const (
	eventStarted            = "attempt.started"
	eventProgress           = "attempt.progress"
	eventSubmitted          = "attempt.submitted"
	eventGraded             = "attempt.graded"
	eventKicked             = "attempt.kicked"
	eventViolation          = "attempt.violation"
	eventSessionInvalidated = "attempt.session_invalidated"
	eventDisconnected       = "attempt.disconnected"
	eventReconnected        = "attempt.reconnected"
)

// eventViolationAlert is the violation ladder's notify action (docs/06 section
// 3), and the one event in this module that rides the user:{id}:notify channel
// rather than quiz:{id}:events: it is addressed to the quiz owner personally,
// so it reaches a teacher who is anywhere in their workspace rather than only
// one watching this quiz's live monitor. It is a notification, not a state
// delta - no attempt_events row backs it, because the attempt.violation row
// that triggered it already is the durable evidence.
const eventViolationAlert = "attempt.violation_alert"

// startedPayload is the attempt.started delta: the dashboard moves the row to
// "in progress" and starts its countdown from deadline_at (docs/05 section 2).
type startedPayload struct {
	StudentID  string    `json:"student_id"`
	AttemptID  string    `json:"attempt_id"`
	DeadlineAt time.Time `json:"deadline_at"`
}

// progressPayload is the attempt.progress delta. CurrentQuestion is the
// 1-based ordinal position (within the pinned quiz_version's questions array)
// of the last question SaveAnswer resolved - the closest proxy REST autosave
// has to a navigation cursor, since there is no separate "viewing question N"
// signal.
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

// kickedPayload is the attempt.kicked delta (docs/05 section 2): the monitor
// row moves to "kicked" and the student's own attempt socket renders the
// lockout screen with the reason. KickedBy is the teacher or admin who ordered
// it; Reason is the required free-text/canned justification (docs/06 section 4).
type kickedPayload struct {
	KickedBy string `json:"kicked_by"`
	Reason   string `json:"reason"`
}

// violationPayload is the attempt.violation delta (docs/05 section 2): the
// monitor row's amber badge shows the running ViolationCount, with Type on
// hover. ViolationCount is the counted tally the ladder reads against - it
// advances only for a guardrail whose snapshotted policy is "count"; a
// warn-only or clipboard-logged report carries the (unchanged) count so the
// teacher still sees the evidence type without it feeding the ladder (docs/06
// section 3). DurationMs is optional (focus-loss duration); omitted otherwise.
type violationPayload struct {
	Type           string `json:"type"`
	DurationMs     *int   `json:"duration_ms"`
	ViolationCount int    `json:"violation_count"`
}

// violationNotifyPayload is the attempt.violation_alert notification (docs/06
// section 3's notify action). It carries who did what, where, and how many
// times, so the teacher's banner can name the student and the quiz without a
// follow-up fetch - the notify socket is open across the whole workspace, so
// the recipient generally is not looking at either.
type violationNotifyPayload struct {
	QuizID         string `json:"quiz_id"`
	QuizTitle      string `json:"quiz_title"`
	AttemptID      string `json:"attempt_id"`
	StudentID      string `json:"student_id"`
	StudentName    string `json:"student_name"`
	ViolationType  string `json:"violation_type"`
	ViolationCount int    `json:"violation_count"`
}

// sessionInvalidatedPayload is the attempt.session_invalidated delta (docs/08
// section 1, docs/06 section 3 "Single active session"): recorded when a
// second device's attempt:{id} socket connects and the gateway force-closes
// the first. It carries no payload - the row's existence and timestamp are
// the whole record the teacher can see.
type sessionInvalidatedPayload struct{}

// disconnectedPayload is the attempt.disconnected delta (docs/05 section 2):
// the monitor row flags amber "disconnected" (the deadline clock keeps
// running). LastSeenAt is the last heartbeat the gateway received before the
// heartbeatTimeout elapsed.
type disconnectedPayload struct {
	LastSeenAt time.Time `json:"last_seen_at"`
}

// reconnectedPayload is the attempt.reconnected delta (docs/05 section 2):
// the monitor row's disconnected flag clears. It carries no payload, same as
// sessionInvalidatedPayload.
type reconnectedPayload struct{}

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
//
// PublishNotify is the same relay onto one user's own user:{id}:notify channel
// (docs/05 section 3), for the events addressed to a person rather than to
// everyone watching a quiz - today, the violation ladder's notify action. It
// has the exact shape quiz.EventPublisher declares, so the one wired
// realtime.Publisher satisfies both modules' interfaces.
type EventPublisher interface {
	Publish(ctx context.Context, quizID, attemptID, eventType string, payload any)
	PublishNotify(ctx context.Context, userID, eventType string, payload any)
}

// noopPublisher is the default relay: every test that does not exercise the
// socket, and any deploy that has not wired Redis, gets a publisher that
// drops every event. Callers publish unconditionally against it.
type noopPublisher struct{}

func (noopPublisher) Publish(context.Context, string, string, string, any) {}
func (noopPublisher) PublishNotify(context.Context, string, string, any)   {}

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
