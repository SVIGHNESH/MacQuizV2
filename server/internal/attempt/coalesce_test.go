package attempt

import (
	"context"
	"sync"
	"testing"
	"time"
)

// recordedPublish is one call the fake inner publisher saw, reduced to the
// fields the coalescer test asserts on.
type recordedPublish struct {
	quizID    string
	attemptID string
	eventType string
}

// recordingPublisher is the coalescer's downstream: it records every call that
// survives the throttle so the test can count what was relayed. It is
// concurrency-safe because one subtest relays from many goroutines.
type recordingPublisher struct {
	mu   sync.Mutex
	seen []recordedPublish
}

func (r *recordingPublisher) Publish(_ context.Context, quizID, attemptID, eventType string, _ any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, recordedPublish{quizID, attemptID, eventType})
}

func (r *recordingPublisher) count(attemptID, eventType string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.seen {
		if s.attemptID == attemptID && s.eventType == eventType {
			n++
		}
	}
	return n
}

// newTestCoalescer wraps a recorder with a controllable clock so the test drives
// the window without sleeping.
func newTestCoalescer() (*ProgressCoalescer, *recordingPublisher, *time.Time) {
	rec := &recordingPublisher{}
	c := NewProgressCoalescer(rec)
	clock := time.Unix(0, 0).UTC()
	c.now = func() time.Time { return clock }
	return c, rec, &clock
}

func TestProgressCoalescer(t *testing.T) {
	ctx := context.Background()

	t.Run("rapid progress in one window relays once", func(t *testing.T) {
		c, rec, _ := newTestCoalescer()
		for i := 0; i < 3; i++ {
			c.Publish(ctx, "quiz-1", "att-1", eventProgress, progressPayload{AnsweredCount: i + 1})
		}
		if got := rec.count("att-1", eventProgress); got != 1 {
			t.Fatalf("progress relayed = %d, want 1 (leading-edge, rest dropped)", got)
		}
	})

	t.Run("progress relays again once the window elapses", func(t *testing.T) {
		c, rec, clock := newTestCoalescer()
		c.Publish(ctx, "quiz-1", "att-1", eventProgress, progressPayload{AnsweredCount: 1})

		*clock = clock.Add(progressWindow - time.Nanosecond)
		c.Publish(ctx, "quiz-1", "att-1", eventProgress, progressPayload{AnsweredCount: 2})
		if got := rec.count("att-1", eventProgress); got != 1 {
			t.Fatalf("progress just inside window relayed = %d, want 1", got)
		}

		*clock = clock.Add(time.Nanosecond) // now exactly one window on
		c.Publish(ctx, "quiz-1", "att-1", eventProgress, progressPayload{AnsweredCount: 3})
		if got := rec.count("att-1", eventProgress); got != 2 {
			t.Fatalf("progress at window boundary relayed total = %d, want 2", got)
		}
	})

	t.Run("throttle is per attempt", func(t *testing.T) {
		c, rec, _ := newTestCoalescer()
		c.Publish(ctx, "quiz-1", "att-1", eventProgress, progressPayload{AnsweredCount: 1})
		c.Publish(ctx, "quiz-1", "att-1", eventProgress, progressPayload{AnsweredCount: 2})
		c.Publish(ctx, "quiz-1", "att-2", eventProgress, progressPayload{AnsweredCount: 1})
		if got := rec.count("att-1", eventProgress); got != 1 {
			t.Fatalf("att-1 progress relayed = %d, want 1", got)
		}
		if got := rec.count("att-2", eventProgress); got != 1 {
			t.Fatalf("att-2 progress relayed = %d, want 1 (independent window)", got)
		}
	})

	t.Run("non-progress events always pass through unthrottled", func(t *testing.T) {
		c, rec, _ := newTestCoalescer()
		// Within a single window, other event types are never coalesced.
		c.Publish(ctx, "quiz-1", "att-1", eventStarted, startedPayload{})
		c.Publish(ctx, "quiz-1", "att-1", eventProgress, progressPayload{AnsweredCount: 1})
		c.Publish(ctx, "quiz-1", "att-1", eventProgress, progressPayload{AnsweredCount: 2})
		c.Publish(ctx, "quiz-1", "att-1", eventSubmitted, submittedPayload{})
		c.Publish(ctx, "quiz-1", "att-1", eventGraded, gradedPayload{})
		if got := rec.count("att-1", eventStarted); got != 1 {
			t.Fatalf("started relayed = %d, want 1", got)
		}
		if got := rec.count("att-1", eventProgress); got != 1 {
			t.Fatalf("progress relayed = %d, want 1 (only progress is throttled)", got)
		}
		if got := rec.count("att-1", eventSubmitted); got != 1 {
			t.Fatalf("submitted relayed = %d, want 1", got)
		}
		if got := rec.count("att-1", eventGraded); got != 1 {
			t.Fatalf("graded relayed = %d, want 1", got)
		}
	})

	t.Run("sweep drops elapsed attempts and keeps the map bounded", func(t *testing.T) {
		c, _, clock := newTestCoalescer()
		// One relay per distinct attempt fills the map to the sweep threshold.
		for i := 0; i < progressSweepInterval-1; i++ {
			c.Publish(ctx, "quiz-1", attemptKey(i), eventProgress, progressPayload{})
		}
		c.mu.Lock()
		filled := len(c.last)
		c.mu.Unlock()
		if filled != progressSweepInterval-1 {
			t.Fatalf("map size before sweep = %d, want %d", filled, progressSweepInterval-1)
		}

		// Advance past the window so every recorded entry is now sweepable, then
		// the next admitted relay (the threshold-th) triggers the sweep and
		// leaves only its own fresh entry behind.
		*clock = clock.Add(progressWindow)
		c.Publish(ctx, "quiz-1", "att-fresh", eventProgress, progressPayload{})
		c.mu.Lock()
		swept := len(c.last)
		c.mu.Unlock()
		if swept != 1 {
			t.Fatalf("map size after sweep = %d, want 1 (only the fresh entry)", swept)
		}
	})
}

// attemptKey builds distinct attempt ids for the sweep test without importing a
// formatter into the hot path.
func attemptKey(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "att-0"
	}
	buf := []byte("att-")
	var rev []byte
	for i > 0 {
		rev = append(rev, digits[i%10])
		i /= 10
	}
	for j := len(rev) - 1; j >= 0; j-- {
		buf = append(buf, rev[j])
	}
	return string(buf)
}
