package realtime

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// TestRedisSubscriptionForwarderExitsOnCancel is the regression guard for the
// forwarding-goroutine leak: if the pump stops reading (client disconnect) and
// the 16-slot buffer fills, the goroutine parks on the send and would never
// observe raw closing. Canceling the subscribe context must free it. We fill
// the buffer, park the forwarder, cancel, and assert the output channel closes.
func TestRedisSubscriptionForwarderExitsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Buffered so the feeder never blocks and cannot itself leak; more than the
	// out buffer (16) so the forwarder is forced to park on a full send.
	raw := make(chan *redis.Message, 64)
	for i := 0; i < 40; i++ {
		raw <- &redis.Message{Payload: "x"}
	}

	sub := &redisSubscription{ctx: ctx, raw: raw}
	out := sub.Messages()

	// Give the forwarder time to drain into the full out buffer and park on the
	// blocking send, without anyone reading out.
	time.Sleep(50 * time.Millisecond)

	cancel()

	// The forwarder must now return and close out. Drain buffered items until
	// the close is observed; a leak would hang here past the deadline.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-out:
			if !ok {
				return // closed: the forwarder exited on cancel
			}
		case <-deadline:
			t.Fatal("forwarder did not exit after the subscribe context was canceled")
		}
	}
}
