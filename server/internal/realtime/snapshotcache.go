package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// snapshotTimeout bounds every cache round trip the same way publishTimeout
// bounds Publish: a cache op sits on the request goroutine (buildDetail calls
// it inline, not from a background worker), so a slow or partitioned Redis
// must fail fast and fall back to the Postgres read it was trying to save,
// never stall the attempt read path behind it.
const snapshotTimeout = 250 * time.Millisecond

// snapshotTTL bounds how long a cached snapshot can outlive any real use for
// it. quiz_versions rows never change once inserted, so this is not
// invalidation - it is a memory safety valve so years of quizzes do not
// accumulate unbounded keys in Redis.
const snapshotTTL = 7 * 24 * time.Hour

// SnapshotCache is the Redis-backed docs/01 "Go-live herd" cache: it caches
// one quiz version's immutable questions/guardrails JSON so a start storm
// (1,000 students starting within ~60s) re-serializes that payload out of
// Postgres once, not once per start. It satisfies attempt.SnapshotCache, so
// the attempt module depends only on the small Get/Set methods, never on
// go-redis - same decoupling as Publisher and attempt.EventPublisher.
type SnapshotCache struct {
	rdb *redis.Client
	log *slog.Logger
}

// NewSnapshotCache builds a SnapshotCache from a redis:// URL. Like
// NewPublisher, it validates the URL but does not dial: go-redis connects
// lazily and reconnects on its own, and a cache is an optimization only - a
// transiently unreachable Redis must never keep the API from booting, and
// every Get/Set below degrades to "treat it as a miss" on any error.
func NewSnapshotCache(redisURL string, log *slog.Logger) (*SnapshotCache, error) {
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	opt.DialTimeout = snapshotTimeout
	opt.ReadTimeout = time.Second
	opt.WriteTimeout = time.Second
	opt.MaxRetries = -1
	return &SnapshotCache{rdb: redis.NewClient(opt), log: log}, nil
}

// Close releases the Redis connection pool.
func (c *SnapshotCache) Close() error { return c.rdb.Close() }

func snapshotKey(quizID string, version int) string {
	return fmt.Sprintf("quizsnap:%s:%d", quizID, version)
}

// snapshotEntry is the cached value: both fields are already-serialized JSON
// (jsonb columns read back as bytes), so this wrapper just concatenates them
// into one Redis value instead of a second key per version.
type snapshotEntry struct {
	Questions  json.RawMessage `json:"q"`
	Guardrails json.RawMessage `json:"g"`
}

// Get returns the cached snapshot, or ok=false on any miss - including a
// cache error, which is logged and treated as a miss rather than returned:
// the caller's Postgres fallback is always correct, just slower.
func (c *SnapshotCache) Get(ctx context.Context, quizID string, version int) (questions, guardrails []byte, ok bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotTimeout)
	defer cancel()

	val, err := c.rdb.Get(ctx, snapshotKey(quizID, version)).Bytes()
	if err != nil {
		if err != redis.Nil {
			c.log.Warn("snapshot cache get", "quiz_id", quizID, "version", version, "err", err)
		}
		return nil, nil, false
	}
	var entry snapshotEntry
	if err := json.Unmarshal(val, &entry); err != nil {
		c.log.Warn("snapshot cache decode", "quiz_id", quizID, "version", version, "err", err)
		return nil, nil, false
	}
	return entry.Questions, entry.Guardrails, true
}

// Set populates the cache after a miss was resolved from Postgres. Best-
// effort: a failed write is logged and dropped, never surfaced - the next
// reader just misses again and re-populates it.
func (c *SnapshotCache) Set(ctx context.Context, quizID string, version int, questions, guardrails []byte) {
	val, err := json.Marshal(snapshotEntry{Questions: questions, Guardrails: guardrails})
	if err != nil {
		c.log.Error("marshal snapshot cache entry", "quiz_id", quizID, "version", version, "err", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), snapshotTimeout)
	defer cancel()
	if err := c.rdb.Set(ctx, snapshotKey(quizID, version), val, snapshotTTL).Err(); err != nil {
		c.log.Warn("snapshot cache set", "quiz_id", quizID, "version", version, "err", err)
	}
}
