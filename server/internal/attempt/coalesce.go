package attempt

import (
	"context"
	"sync"
	"time"
)

// progressWindow is the docs/05 section 5 coalescing cap: attempt.progress is
// relayed at most once per this interval per attempt. A 500-student quiz then
// peaks near 250 events/s (one per student per 2 s), the budget the doc sizes
// Redis pub/sub and a single gateway node against.
const progressWindow = 2 * time.Second

// progressSweepInterval bounds the coalescer's memory. Every this many admitted
// relays it discards attempts whose window has fully elapsed, so the last-seen
// map tracks only recently-active attempts rather than every attempt the
// process has ever relayed for. Stale entries never affect correctness (an
// elapsed window is admitted whether or not the entry is present), so this is a
// pure memory cap, amortized onto the write path instead of a janitor goroutine
// that would need shutdown plumbing.
const progressSweepInterval = 1024

// ProgressCoalescer wraps an EventPublisher and enforces docs/05 section 5's
// throttle: attempt.progress is relayed at most once per progressWindow per
// attempt; every other event type passes straight through. Coalescing is a
// delivery-only concern - SaveAnswer still appends an attempt_events row on
// every autosave, so the append-only log and the reconciling live-roster
// snapshot (quiz.LiveRoster) stay complete. Only the best-effort Redis relay is
// thinned, which is exactly what the section 5 events/s budget assumes.
//
// Leading-edge by design: the first progress in a window relays immediately and
// the rest are dropped. A burst-then-idle student can leave the streamed
// answered_count briefly stale, but the terminal count rides attempt.submitted
// and a reconnecting dashboard re-fetches the snapshot, so no dashboard drifts.
// Keep-latest (trailing-edge) coalescing is a deliberate non-goal until the
// live dashboard that would benefit from it exists - adding a per-attempt timer
// and its lifecycle now would optimize a UX nothing can yet observe.
//
// The decorator lives at the composition root (main.go wraps the serve-side
// realtime.Publisher); the attempt Service still takes a raw EventPublisher, so
// tests that count publishes inject their own fake unthrottled. The worker
// never emits progress, so only the serve-side publisher is wrapped.
type ProgressCoalescer struct {
	inner  EventPublisher
	window time.Duration
	now    func() time.Time

	mu    sync.Mutex
	last  map[string]time.Time
	fires int
}

// NewProgressCoalescer wraps inner with the section 5 progress throttle. inner
// receives every non-progress event and the admitted progress events; a nil
// inner is invalid (wrap the resolved publisher, which is never nil).
func NewProgressCoalescer(inner EventPublisher) *ProgressCoalescer {
	return &ProgressCoalescer{
		inner:  inner,
		window: progressWindow,
		now:    time.Now,
		last:   make(map[string]time.Time),
	}
}

// Publish relays every non-progress event unchanged and admits at most one
// attempt.progress per window per attempt, satisfying EventPublisher.
func (c *ProgressCoalescer) Publish(ctx context.Context, quizID, attemptID, eventType string, payload any) {
	if eventType != eventProgress {
		c.inner.Publish(ctx, quizID, attemptID, eventType, payload)
		return
	}
	if !c.admit(attemptID) {
		return
	}
	// Relay outside the lock: the throttle decision is a fast map lookup, but
	// inner.Publish can block on a slow or partitioned Redis for up to
	// realtime.publishTimeout. Holding the mutex across it would serialize every
	// student's progress behind one stuck network call - the exact head-of-line
	// stall that publishTimeout and the detached publish context exist to avoid.
	c.inner.Publish(ctx, quizID, attemptID, eventType, payload)
}

// admit returns whether a progress relay for attemptID is allowed now, recording
// the time when it is. A relay is allowed when the attempt has no recorded relay
// or its window has fully elapsed. It also amortizes the memory sweep onto the
// admitted path (see progressSweepInterval).
func (c *ProgressCoalescer) admit(attemptID string) bool {
	now := c.now()
	c.mu.Lock()
	defer c.mu.Unlock()

	if last, ok := c.last[attemptID]; ok && now.Sub(last) < c.window {
		return false
	}
	c.last[attemptID] = now

	c.fires++
	if c.fires >= progressSweepInterval {
		c.fires = 0
		for id, t := range c.last {
			if now.Sub(t) >= c.window {
				delete(c.last, id)
			}
		}
	}
	return true
}
