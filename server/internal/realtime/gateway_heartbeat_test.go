package realtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// setHeartbeatTimeout shrinks the package's heartbeatTimeout for the
// duration of a test, so a heartbeat-timeout test runs in milliseconds
// instead of the production 25s. Restored via the returned func, matching
// this file's straight-line teardown style.
func setHeartbeatTimeout(t *testing.T, d time.Duration) func() {
	t.Helper()
	prev := heartbeatTimeout
	heartbeatTimeout = d
	return func() { heartbeatTimeout = prev }
}

// TestAttemptHeartbeatTimeoutFiresDisconnected proves docs/05 section 5: a
// socket that never sends a heartbeat trips heartbeatTimeout and calls the
// wired AttemptDisconnectedFunc exactly once with this attempt's ID.
func TestAttemptHeartbeatTimeoutFiresDisconnected(t *testing.T) {
	defer setHeartbeatTimeout(t, 100*time.Millisecond)()

	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := newFakeSubscriber()
	g := NewGateway(base, fake, nil, ownerIs("teacher-1"), nil, discardLog())
	g.SetAttemptOwner(attemptOwnedBy("student-1"))

	var mu sync.Mutex
	var calls []string
	done := make(chan struct{}, 8)
	g.SetAttemptDisconnected(func(_ context.Context, attemptID string, _ time.Time) {
		mu.Lock()
		calls = append(calls, attemptID)
		mu.Unlock()
		done <- struct{}{}
	})
	_, wsURL := mountAttempt(t, g, student("student-1"))

	_, c := dial(t, wsURL)
	defer c.CloseNow()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for AttemptDisconnectedFunc to fire")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != testAttemptID {
		t.Fatalf("calls = %v, want exactly one call with %q", calls, testAttemptID)
	}
}

// TestAttemptHeartbeatKeepsAliveWithoutDisconnecting proves a socket that
// sends frames faster than heartbeatTimeout never trips it - the ordinary
// case of a student's tab sending its regular heartbeat.
func TestAttemptHeartbeatKeepsAliveWithoutDisconnecting(t *testing.T) {
	defer setHeartbeatTimeout(t, 150*time.Millisecond)()

	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := newFakeSubscriber()
	g := NewGateway(base, fake, nil, ownerIs("teacher-1"), nil, discardLog())
	g.SetAttemptOwner(attemptOwnedBy("student-1"))

	fired := make(chan struct{}, 1)
	g.SetAttemptDisconnected(func(context.Context, string, time.Time) { fired <- struct{}{} })
	_, wsURL := mountAttempt(t, g, student("student-1"))

	ctx, c := dial(t, wsURL)
	defer c.CloseNow()

	for i := 0; i < 4; i++ {
		time.Sleep(60 * time.Millisecond)
		if err := c.Write(ctx, websocket.MessageText, []byte("heartbeat")); err != nil {
			t.Fatalf("write heartbeat: %v", err)
		}
	}

	select {
	case <-fired:
		t.Fatal("AttemptDisconnectedFunc fired despite regular heartbeats")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestAttemptReconnectedFiresAfterDisconnect proves the docs/05 section 2
// "flag cleared" half: once heartbeatTimeout has already fired, the next
// heartbeat frame calls AttemptReconnectedFunc.
func TestAttemptReconnectedFiresAfterDisconnect(t *testing.T) {
	defer setHeartbeatTimeout(t, 80*time.Millisecond)()

	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := newFakeSubscriber()
	g := NewGateway(base, fake, nil, ownerIs("teacher-1"), nil, discardLog())
	g.SetAttemptOwner(attemptOwnedBy("student-1"))

	disconnected := make(chan struct{}, 1)
	reconnected := make(chan struct{}, 1)
	g.SetAttemptDisconnected(func(context.Context, string, time.Time) { disconnected <- struct{}{} })
	g.SetAttemptReconnected(func(context.Context, string) { reconnected <- struct{}{} })
	_, wsURL := mountAttempt(t, g, student("student-1"))

	ctx, c := dial(t, wsURL)
	defer c.CloseNow()

	select {
	case <-disconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the initial disconnect")
	}

	if err := c.Write(ctx, websocket.MessageText, []byte("heartbeat")); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}

	select {
	case <-reconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for AttemptReconnectedFunc to fire")
	}
}

// TestAttemptHeartbeatFrameNotEchoed proves a heartbeat frame is consumed by
// the read loop, never relayed back to the client - the channel's outbound
// contract (kick + quiz.* banners only) is unaffected by inbound heartbeats.
func TestAttemptHeartbeatFrameNotEchoed(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := newFakeSubscriber()
	g := NewGateway(base, fake, nil, ownerIs("teacher-1"), nil, discardLog())
	g.SetAttemptOwner(attemptOwnedBy("student-1"))
	_, wsURL := mountAttempt(t, g, student("student-1"))

	ctx, c := dial(t, wsURL)
	defer c.CloseNow()

	if err := c.Write(ctx, websocket.MessageText, []byte("heartbeat")); err != nil {
		t.Fatalf("write heartbeat: %v", err)
	}
	fake.emit(`{"type":"quiz.closed","attempt_id":"","payload":{}}`)

	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	_, data, err := c.Read(readCtx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(data); got != `{"type":"quiz.closed","attempt_id":"","payload":{}}` {
		t.Fatalf("first relayed frame = %q, want only the quiz.closed banner (heartbeat must not echo)", got)
	}
}
