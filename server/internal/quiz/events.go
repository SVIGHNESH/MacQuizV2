package quiz

import (
	"context"
	"time"
)

// This file owns two realtime relays. quiz.extended/quiz.closed
// (docs/05 section 2) have no per-attempt attempt_events row to persist
// first - the window change's source of truth is already the audit_log row
// Extend/ForceClose write in the same transaction (docs/08 section 7) - so
// they publish, after commit, onto the same quiz:{quiz_id}:events channel
// the attempt module's deltas ride; the realtime gateway's attempt:{id}
// channel already relays anything with a "quiz." prefix through to the
// student (docs/05 section 3), and the teacher monitor channel fans out
// every event on the quiz channel regardless of prefix, so no gateway
// change was needed to carry these.
//
// quiz.assigned/quiz.unassigned are the "Notifications on assignment
// changes" gap: SetAssignments's audit_log row (quizzes.assignments_set) is
// their source of truth, and they publish, after commit, to the affected
// student's own user:{id}:notify channel rather than the quiz channel -
// unlike a window change, an assignment notification is meaningful even
// before the student has ever opened the quiz, so it cannot ride a channel
// keyed by an attempt or a monitor subscription that does not exist yet.
const (
	eventExtended = "quiz.extended"
	eventClosed   = "quiz.closed"

	// eventAssigned/eventUnassigned are the "Notifications on assignment
	// changes" gap docs/12's implementation plan names and docs/05 section 3
	// scopes to the user:{id}:notify channel ("Assignment notifications").
	// SetAssignments (lifecycle.go) publishes one of these, after commit, to
	// every student whose membership in the quiz's audience changed.
	eventAssigned   = "quiz.assigned"
	eventUnassigned = "quiz.unassigned"
)

// windowPayload is the quiz.extended/quiz.closed delta: the new end of the
// live window (docs/05 section 2: "new ends_at").
type windowPayload struct {
	EndsAt time.Time `json:"ends_at"`
}

// assignmentPayload is the quiz.assigned/quiz.unassigned notification: enough
// for the student's client to name the quiz without a follow-up fetch.
type assignmentPayload struct {
	QuizID string `json:"quiz_id"`
	Title  string `json:"title"`
}

// EventPublisher relays a committed quiz-wide event to subscribers, and a
// committed per-user notification to that user's own channel. It has the
// same Publish shape as attempt.EventPublisher (and realtime.Publisher
// satisfies both, plus PublishNotify) so a single wired Publisher instance
// can relay for both modules without either importing the other or go-redis
// directly.
type EventPublisher interface {
	Publish(ctx context.Context, quizID, attemptID, eventType string, payload any)
	PublishNotify(ctx context.Context, userID, eventType string, payload any)
}

// noopPublisher is the default relay: every test that does not wire a
// publisher, and any deploy that has not wired Redis, gets one that drops
// every event. Callers publish unconditionally against it.
type noopPublisher struct{}

func (noopPublisher) Publish(context.Context, string, string, string, any) {}
func (noopPublisher) PublishNotify(context.Context, string, string, any)   {}

// resolvePublisher picks the wired relay or the no-op fallback, so Extend and
// ForceClose can call Publish without a nil guard. The variadic keeps every
// existing NewService call site - none of which relay - compiling unchanged.
func resolvePublisher(publishers []EventPublisher) EventPublisher {
	if len(publishers) > 0 && publishers[0] != nil {
		return publishers[0]
	}
	return noopPublisher{}
}
