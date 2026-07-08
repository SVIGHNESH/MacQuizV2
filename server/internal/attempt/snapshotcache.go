package attempt

import "context"

// SnapshotCache is a best-effort read-through cache for one quiz version's
// immutable question/guardrail snapshot (docs/01-requirements.md section
// "Go-live herd": "the question snapshot served from Redis cache (no
// re-serialization per start)"). quiz_versions rows are only ever inserted,
// never updated (a new version is a new row), so a cache entry keyed by
// (quiz_id, version) can never go stale - a miss just costs the one Postgres
// read buildDetail already ran before this cache existed.
//
// realtime.SnapshotCache is the concrete Redis-backed implementation; the
// interface keeps this module from importing go-redis and gives tests a seam.
type SnapshotCache interface {
	// Get returns the cached questions and guardrails JSON for one quiz
	// version, or ok=false on a miss (including "cache unreachable" - callers
	// must fall back to Postgres, never treat a miss as an error).
	Get(ctx context.Context, quizID string, version int) (questions, guardrails []byte, ok bool)
	// Set populates the cache after a miss was resolved from Postgres.
	// Implementations are best-effort: a failed Set is logged and dropped,
	// never surfaced to the caller.
	Set(ctx context.Context, quizID string, version int, questions, guardrails []byte)
}

// noopSnapshotCache is the default: every test and any deploy that has not
// wired Redis simply always misses and falls back to Postgres, exactly as
// buildDetail behaved before this cache existed.
type noopSnapshotCache struct{}

func (noopSnapshotCache) Get(context.Context, string, int) ([]byte, []byte, bool) {
	return nil, nil, false
}

func (noopSnapshotCache) Set(context.Context, string, int, []byte, []byte) {}
