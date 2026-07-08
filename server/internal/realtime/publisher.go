package realtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

// publishTimeout hard-bounds a single publish. docs/05 section 5 is explicit
// that "students' attempts never depend on the socket": the publish runs on
// the request goroutine after commit, so an unbounded call would let a slow or
// partitioned Redis stall the primary REST write path. This deadline (backed
// by the per-op client timeouts below) caps that cost - a publish to a
// blackholed host returns and drops well within it.
const publishTimeout = 2 * time.Second

// This file owns the "publish second" half of the docs/05 section 1 pipeline:
// "persist first, publish second - the event row is the source of truth; the
// publish is best-effort delivery". The attempt module has already committed
// the attempt_events row before it calls Publish, so a publish failure is
// logged and dropped, never surfaced: the live-roster snapshot
// (quiz.LiveRoster) reconciles any gap on connect, so no dashboard drifts.
// The gateway (this package's doc.go contract) subscribes to the same channel
// and fans each event out to quiz:{id}:monitor; that brick layers on top.

// eventsChannel is the Redis pub/sub channel every event for one quiz lands on
// (docs/05 section 1: publish to quiz:{quiz_id}:events). quiz_id lives in the
// channel name, so the envelope does not repeat it.
func eventsChannel(quizID string) string { return "quiz:" + quizID + ":events" }

// notifyChannel is the docs/05 section 3 user:{id}:notify channel: a per-user
// stream, separate from any quiz's events channel, since a notification (e.g.
// a new assignment) is not scoped to one quiz's audience of subscribers.
func notifyChannel(userID string) string { return "user:" + userID + ":notify" }

// Event is the envelope published on quiz:{quiz_id}:events. It carries the
// docs/05 section 2 event type, the attempt the delta applies to (the roster
// row the dashboard updates), and the typed payload as raw JSON.
type Event struct {
	Type      string          `json:"type"`
	AttemptID string          `json:"attempt_id"`
	Payload   json.RawMessage `json:"payload"`
}

// NotifyEvent is the envelope published on user:{id}:notify. It has no
// attempt_id: unlike Event, a notification is never scoped to one roster row.
type NotifyEvent struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Publisher relays committed attempt events onto Redis pub/sub. It satisfies
// attempt.EventPublisher, so the attempt module depends only on the small
// Publish method, never on go-redis.
type Publisher struct {
	rdb *redis.Client
	log *slog.Logger
}

// NewPublisher builds a Publisher from a redis:// URL. It validates the URL
// but does not dial: go-redis connects lazily and reconnects on its own, and
// docs/05 section 5 is explicit that "students' attempts never depend on the
// socket" - so a transiently unreachable Redis must never keep the API or the
// worker from booting. A publish against a down Redis logs and drops within
// publishTimeout.
func NewPublisher(redisURL string, log *slog.Logger) (*Publisher, error) {
	opt, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("parse redis url: %w", err)
	}
	// Bound every network op so a partitioned Redis fails fast instead of
	// blocking a request goroutine on the multi-second go-redis defaults
	// (DialTimeout 5s, ReadTimeout 3s, retried 3x). MaxRetries = -1 disables
	// retries entirely; 0 would mean "use the default of 3" in go-redis v9.
	opt.DialTimeout = publishTimeout
	opt.ReadTimeout = time.Second
	opt.WriteTimeout = time.Second
	opt.MaxRetries = -1
	return &Publisher{rdb: redis.NewClient(opt), log: log}, nil
}

// Publish marshals one event into the channel envelope and publishes it to the
// quiz's events channel. It is best-effort by contract (docs/05 section 1):
// the caller has already persisted the source-of-truth row, so every failure
// here is logged and swallowed rather than returned.
func (p *Publisher) Publish(ctx context.Context, quizID, attemptID, eventType string, payload any) {
	// Detach from the caller's cancellation and bound the call: the source-of-
	// truth row already committed, so a client disconnect (or worker shutdown)
	// right after commit must neither skip the relay nor let it block the
	// caller. WithoutCancel keeps request-scoped values but drops the deadline.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
	defer cancel()

	raw, err := json.Marshal(payload)
	if err != nil {
		p.log.Error("marshal realtime payload",
			"type", eventType, "attempt_id", attemptID, "err", err)
		return
	}
	env, err := json.Marshal(Event{Type: eventType, AttemptID: attemptID, Payload: raw})
	if err != nil {
		p.log.Error("marshal realtime envelope",
			"type", eventType, "attempt_id", attemptID, "err", err)
		return
	}
	if err := p.rdb.Publish(ctx, eventsChannel(quizID), env).Err(); err != nil {
		p.log.Warn("publish realtime event",
			"type", eventType, "quiz_id", quizID, "attempt_id", attemptID, "err", err)
	}
}

// PublishNotify marshals one event into the user:{id}:notify envelope and
// publishes it to that user's channel. Best-effort by the same contract as
// Publish: the caller has already committed whatever made the notification
// true (e.g. the quiz_assignments row), so a failure here is logged and
// swallowed rather than returned.
func (p *Publisher) PublishNotify(ctx context.Context, userID, eventType string, payload any) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
	defer cancel()

	raw, err := json.Marshal(payload)
	if err != nil {
		p.log.Error("marshal notify payload", "type", eventType, "user_id", userID, "err", err)
		return
	}
	env, err := json.Marshal(NotifyEvent{Type: eventType, Payload: raw})
	if err != nil {
		p.log.Error("marshal notify envelope", "type", eventType, "user_id", userID, "err", err)
		return
	}
	if err := p.rdb.Publish(ctx, notifyChannel(userID), env).Err(); err != nil {
		p.log.Warn("publish notify event", "type", eventType, "user_id", userID, "err", err)
	}
}

// Close releases the Redis connection pool.
func (p *Publisher) Close() error { return p.rdb.Close() }

// Ping verifies Redis is reachable. It backs the /healthz dependency check
// (docs/10-operations.md section 2: "/healthz checks DB connectivity, Redis
// connectivity, and queue depth"), so unlike Publish it returns the error
// instead of swallowing it.
func (p *Publisher) Ping(ctx context.Context) error {
	return p.rdb.Ping(ctx).Err()
}
