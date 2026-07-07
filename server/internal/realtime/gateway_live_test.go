package realtime

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// TestGatewayLiveSmoke is the one test that exercises the real go-redis
// Subscribe wiring end to end: a monitor socket connects, a real Publish to
// quiz:{id}:events is fanned out, and the socket receives the envelope. Like
// TestPublisherLiveSmoke it is gated on a reachable Redis and skips otherwise;
// the deterministic socket/fan-out/detachment behaviour is covered by the
// fake-Subscriber tests, so this only guards the thin NewRedisSubscriber glue.
func TestGatewayLiveSmoke(t *testing.T) {
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

	sub, err := NewRedisSubscriber(url)
	if err != nil {
		t.Fatalf("new subscriber: %v", err)
	}
	defer sub.Close()

	base, baseCancel := context.WithCancel(context.Background())
	defer baseCancel()
	g := NewGateway(base, sub, nil, ownerIs("admin-1"), nil, discardLog())
	_, wsURL := mountMonitor(t, g, admin(), 0)

	dialCtx, c := dial(t, wsURL)
	defer c.CloseNow()

	// The gateway's Subscribe blocks on the SUBSCRIBE round-trip before the
	// handshake completes, so by the time Dial returns the subscription is
	// live and this Publish cannot race ahead of it.
	pub, err := NewPublisher(url, discardLog())
	if err != nil {
		t.Fatalf("new publisher: %v", err)
	}
	defer pub.Close()
	pub.Publish(ctx, testQuizID, "a-live-1", "attempt.graded", map[string]any{"score": 7})

	_, data, err := c.Read(dialCtx)
	if err != nil {
		t.Fatalf("read live event: %v", err)
	}
	if !strings.Contains(string(data), `"attempt.graded"`) || !strings.Contains(string(data), `"a-live-1"`) {
		t.Fatalf("relayed %q, want the published envelope", data)
	}
}
