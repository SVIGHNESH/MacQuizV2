package realtime

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// TestSnapshotCacheLiveSmoke proves the actual Redis round trip end to end: a
// Set followed by a Get on a fresh SnapshotCache returns the same bytes, and
// an unknown version misses. Gated on a reachable Redis like
// TestPublisherLiveSmoke, for the same reason.
func TestSnapshotCacheLiveSmoke(t *testing.T) {
	url := os.Getenv("MACQUIZ_TEST_REDIS_URL")
	if url == "" {
		url = "redis://localhost:6380/0"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	probe := redis.NewClient(opt)
	defer probe.Close()
	if err := probe.Ping(ctx).Err(); err != nil {
		t.Skipf("redis not reachable at %s: %v", url, err)
	}

	cache, err := NewSnapshotCache(url, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new snapshot cache: %v", err)
	}
	defer cache.Close()

	const quizID = "quiz-snapcache-smoke-1"
	defer probe.Del(ctx, snapshotKey(quizID, 1))

	if _, _, ok := cache.Get(ctx, quizID, 1); ok {
		t.Fatalf("Get on an unpopulated key returned ok=true, want a miss")
	}

	wantQuestions := []byte(`[{"id":"q1","position":1}]`)
	wantGuardrails := []byte(`{"max_violations":3}`)
	cache.Set(ctx, quizID, 1, wantQuestions, wantGuardrails)

	gotQuestions, gotGuardrails, ok := cache.Get(ctx, quizID, 1)
	if !ok {
		t.Fatalf("Get after Set returned ok=false, want a hit")
	}
	if string(gotQuestions) != string(wantQuestions) {
		t.Errorf("questions = %s, want %s", gotQuestions, wantQuestions)
	}
	if string(gotGuardrails) != string(wantGuardrails) {
		t.Errorf("guardrails = %s, want %s", gotGuardrails, wantGuardrails)
	}

	// A different version of the same quiz is a distinct key.
	if _, _, ok := cache.Get(ctx, quizID, 2); ok {
		t.Fatalf("Get on a different version returned ok=true, want a miss")
	}
}
