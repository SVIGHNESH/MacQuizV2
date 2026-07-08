// Package ratelimit is a small in-memory sliding-window limiter shared by
// every module that needs to throttle a client action per key (login per
// IP/account, kick/readmit per teacher - docs/08-security.md section 4). The
// state is in-memory, which is correct for the single-API-container topology
// of docs/09-deployment.md; a second replica would need this moved to Redis.
package ratelimit

import (
	"sync"
	"time"
)

// Limiter is a fixed-capacity sliding-window counter keyed by string.
type Limiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
}

// New builds a Limiter that allows up to limit hits per key within window.
func New(limit int, window time.Duration) *Limiter {
	return &Limiter{limit: limit, window: window, hits: make(map[string][]time.Time)}
}

// Allow records an event for key and reports whether it is within the limit.
// When refused, retryAfter says how long until the oldest hit leaves the window.
func (rl *Limiter) Allow(key string, now time.Time) (ok bool, retryAfter time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	cutoff := now.Add(-rl.window)
	kept := rl.hits[key][:0]
	for _, t := range rl.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= rl.limit {
		rl.hits[key] = kept
		return false, kept[0].Sub(cutoff)
	}
	rl.hits[key] = append(kept, now)

	// Opportunistic cleanup so abandoned keys cannot grow the map forever.
	if len(rl.hits) > 10_000 {
		for k, ts := range rl.hits {
			if len(ts) == 0 || !ts[len(ts)-1].After(cutoff) {
				delete(rl.hits, k)
			}
		}
	}
	return true, 0
}
