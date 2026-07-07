package realtime

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// TestPublisherLiveSmoke proves the actual Redis relay end to end: a real
// subscriber on quiz:{id}:events receives exactly the envelope Publish sends.
// It is the one test that exercises go-redis, so it is gated on a reachable
// Redis (MACQUIZ_TEST_REDIS_URL, else the dev default) and skips otherwise -
// the deterministic behaviour is covered by attempt's capturePublisher test.
func TestPublisherLiveSmoke(t *testing.T) {
	url := os.Getenv("MACQUIZ_TEST_REDIS_URL")
	if url == "" {
		url = "redis://localhost:6380/0"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Skip unless this Redis is actually reachable, so the suite stays green in
	// environments without the Compose stack.
	opt, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	probe := redis.NewClient(opt)
	defer probe.Close()
	if err := probe.Ping(ctx).Err(); err != nil {
		t.Skipf("redis not reachable at %s: %v", url, err)
	}

	const quizID = "quiz-smoke-1"
	sub := probe.Subscribe(ctx, eventsChannel(quizID))
	defer sub.Close()
	// Wait for the subscription to be established before publishing, so the
	// message cannot race ahead of the subscriber.
	if _, err := sub.Receive(ctx); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	pub, err := NewPublisher(url, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("new publisher: %v", err)
	}
	defer pub.Close()

	pub.Publish(ctx, quizID, "attempt-smoke-1", "attempt.started",
		map[string]any{"answered_count": 2})

	msg, err := sub.ReceiveMessage(ctx)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if msg.Channel != eventsChannel(quizID) {
		t.Fatalf("channel = %q, want %q", msg.Channel, eventsChannel(quizID))
	}
	var env Event
	if err := json.Unmarshal([]byte(msg.Payload), &env); err != nil {
		t.Fatalf("decode envelope: %v (%q)", err, msg.Payload)
	}
	if env.Type != "attempt.started" || env.AttemptID != "attempt-smoke-1" {
		t.Fatalf("envelope = %+v, want type/attempt set", env)
	}
	var payload map[string]any
	if err := json.Unmarshal(env.Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload["answered_count"] != float64(2) {
		t.Fatalf("payload answered_count = %v, want 2", payload["answered_count"])
	}
}
