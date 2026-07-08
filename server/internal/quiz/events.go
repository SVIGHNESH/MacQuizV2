package quiz

import (
	"context"
	"time"
)

// This file owns the quiz-wide realtime relay docs/05 section 2 names but
// nothing in the codebase emitted until now: "quiz.extended" / "quiz.closed"
// | new ends_at | Banner to teacher and all in-progress students". Unlike
// attempt.EventPublisher's deltas, these events have no per-attempt
// attempt_events row to persist first - the window change's source of truth
// is already the audit_log row Extend/ForceClose write in the same
// transaction (docs/08 section 7) - so this module only ever publishes,
// after commit, onto the same quiz:{quiz_id}:events channel the attempt
// module's deltas ride. The realtime gateway's attempt:{id} channel already
// relays anything with a "quiz." prefix through to the student (docs/05
// section 3); the teacher monitor channel fans out every event on the quiz
// channel regardless of prefix, so no gateway change is needed to carry
// these.
const (
	eventExtended = "quiz.extended"
	eventClosed   = "quiz.closed"
)

// windowPayload is the quiz.extended/quiz.closed delta: the new end of the
// live window (docs/05 section 2: "new ends_at").
type windowPayload struct {
	EndsAt time.Time `json:"ends_at"`
}

// EventPublisher relays a committed quiz-wide event to subscribers. It has
// the same shape as attempt.EventPublisher (and realtime.Publisher satisfies
// both) so a single wired Publisher instance can relay for both modules
// without either importing the other or go-redis directly.
type EventPublisher interface {
	Publish(ctx context.Context, quizID, attemptID, eventType string, payload any)
}

// noopPublisher is the default relay: every test that does not wire a
// publisher, and any deploy that has not wired Redis, gets one that drops
// every event. Callers publish unconditionally against it.
type noopPublisher struct{}

func (noopPublisher) Publish(context.Context, string, string, string, any) {}

// resolvePublisher picks the wired relay or the no-op fallback, so Extend and
// ForceClose can call Publish without a nil guard. The variadic keeps every
// existing NewService call site - none of which relay - compiling unchanged.
func resolvePublisher(publishers []EventPublisher) EventPublisher {
	if len(publishers) > 0 && publishers[0] != nil {
		return publishers[0]
	}
	return noopPublisher{}
}
