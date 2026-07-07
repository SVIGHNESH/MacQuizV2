package realtime

import (
	"context"
	"fmt"

	"github.com/redis/go-redis/v9"
)

// This file owns the "subscribe" side of the docs/05 pipeline: the gateway
// consumes the quiz:{quiz_id}:events channel the Publisher writes to and fans
// each envelope out to the authorized monitor sockets. It is split behind the
// Subscriber/Subscription interfaces so the gateway's socket lifecycle and
// fan-out are unit-testable with a fake, and only the thin go-redis wiring
// needs a live Redis to exercise (mirroring publisher.go's split).

// Subscriber opens per-channel subscriptions to the realtime bus. One
// subscription is opened per connected monitor socket; at the handful of
// teachers-per-quiz the monitor audience carries, a subscription-per-socket
// is simpler and robust, and the doc's own scaling note (doc.go) defers a
// shared per-quiz fan-out to the process split past ~3-4k sockets.
type Subscriber interface {
	Subscribe(ctx context.Context, channel string) (Subscription, error)
	Close() error
}

// Subscription delivers raw channel payloads until its context is done or it
// is closed. Messages is a receive-only stream; a closed stream (channel
// closed) signals the subscription ended and the socket should drain and go.
type Subscription interface {
	Messages() <-chan string
	Close() error
}

// redisSubscriber is the go-redis-backed Subscriber. Unlike the Publisher's
// client (whose sub-second ReadTimeout fails a partitioned publish fast), the
// subscribe client keeps go-redis's default timeouts: a pub/sub receive blocks
// for the health-check interval, so a short ReadTimeout would churn the
// subscription. A slow Redis here degrades delivery, never a write path.
type redisSubscriber struct {
	rdb *redis.Client
}

// NewRedisSubscriber builds a Subscriber from a redis:// URL. Like
// NewPublisher it validates the URL but does not dial; go-redis connects
// lazily, so a transiently unreachable Redis never blocks the API from
// booting - the gateway just delivers nothing until Redis returns.
func NewRedisSubscriber(redisURL string) (Subscriber, error) {
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	return &redisSubscriber{rdb: redis.NewClient(opt)}, nil
}

func (s *redisSubscriber) Subscribe(ctx context.Context, channel string) (Subscription, error) {
	ps := s.rdb.Subscribe(ctx, channel)
	// Force the SUBSCRIBE round-trip now so a dead Redis surfaces as a clean
	// pre-upgrade error instead of a silently-empty socket.
	if _, err := ps.Receive(ctx); err != nil {
		_ = ps.Close()
		return nil, fmt.Errorf("subscribe %s: %w", channel, err)
	}
	return &redisSubscription{ctx: ctx, ps: ps, raw: ps.Channel()}, nil
}

func (s *redisSubscriber) Close() error { return s.rdb.Close() }

type redisSubscription struct {
	ctx context.Context
	ps  *redis.PubSub
	raw <-chan *redis.Message
	out chan string
}

func (r *redisSubscription) Messages() <-chan string {
	// Lazily adapt *redis.Message to the payload-only stream the gateway
	// consumes. The forwarding goroutine ends when go-redis closes raw
	// (on ps.Close) OR when the subscribe context is canceled - the latter
	// matters because a slow socket can park this goroutine on a full-buffer
	// send after the pump has already gone, where a closed raw would never be
	// observed. The gateway cancels that context on every disconnect, so
	// selecting on it here is what actually frees the goroutine.
	if r.out == nil {
		r.out = make(chan string, 16)
		go func() {
			defer close(r.out)
			for msg := range r.raw {
				select {
				case r.out <- msg.Payload:
				case <-r.ctx.Done():
					return
				}
			}
		}()
	}
	return r.out
}

func (r *redisSubscription) Close() error { return r.ps.Close() }
