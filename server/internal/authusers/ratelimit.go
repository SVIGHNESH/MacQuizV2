package authusers

import (
	"sync"
	"time"
)

// rateLimiter is a fixed-capacity sliding-window counter keyed by string.
// Login is limited per IP and per account (docs/04-api.md section 5). The
// state is in-memory, which is correct for the single-API-container topology
// of docs/09-deployment.md; a second replica would need this moved to Redis.
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   map[string][]time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window, hits: make(map[string][]time.Time)}
}

// allow records an event for key and reports whether it is within the limit.
// When refused, retryAfter says how long until the oldest hit leaves the window.
func (rl *rateLimiter) allow(key string, now time.Time) (ok bool, retryAfter time.Duration) {
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
