package realtime

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// TestAttemptSecondDeviceClosesFirst proves docs/08 section 1's single active
// session: a second connection to the same attempt:{id} channel force-closes
// whatever socket was already open for that attempt, with the
// statusSessionReplaced close code the client relies on to skip its
// otherwise-automatic reconnect.
func TestAttemptSecondDeviceClosesFirst(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := newFakeSubscriber()
	g := NewGateway(base, fake, nil, ownerIs("teacher-1"), nil, discardLog())
	g.SetAttemptOwner(attemptOwnedBy("student-1"))
	_, wsURL := mountAttempt(t, g, student("student-1"))

	firstCtx, first := dial(t, wsURL)
	defer first.CloseNow()
	time.Sleep(50 * time.Millisecond) // let the first connection register itself before the second arrives

	_, second := dial(t, wsURL)
	defer second.CloseNow()

	_, _, err := first.Read(firstCtx)
	if err == nil {
		t.Fatal("expected the first socket to be force-closed by the second connection")
	}
	if got := websocket.CloseStatus(err); got != statusSessionReplaced {
		t.Fatalf("first socket close status = %v, want %v", got, statusSessionReplaced)
	}
}

// TestAttemptSessionInvalidatedLogged proves the second-device takeover calls
// the wired SessionInvalidatedFunc exactly once with the attempt ID, so a
// deploy can append the docs/08 section 1 audit row.
func TestAttemptSessionInvalidatedLogged(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := newFakeSubscriber()
	g := NewGateway(base, fake, nil, ownerIs("teacher-1"), nil, discardLog())
	g.SetAttemptOwner(attemptOwnedBy("student-1"))

	var mu sync.Mutex
	var calls []string
	done := make(chan struct{}, 8)
	g.SetSessionInvalidated(func(_ context.Context, attemptID string) {
		mu.Lock()
		calls = append(calls, attemptID)
		mu.Unlock()
		done <- struct{}{}
	})
	_, wsURL := mountAttempt(t, g, student("student-1"))

	_, first := dial(t, wsURL)
	defer first.CloseNow()
	time.Sleep(50 * time.Millisecond)
	_, second := dial(t, wsURL)
	defer second.CloseNow()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for SessionInvalidatedFunc to be called")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0] != testAttemptID {
		t.Fatalf("calls = %v, want exactly one call with %q", calls, testAttemptID)
	}
}

// TestAttemptFirstConnectDoesNotInvalidate proves the very first socket for an
// attempt never triggers the invalidation callback - only a genuine second
// connection does.
func TestAttemptFirstConnectDoesNotInvalidate(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := newFakeSubscriber()
	g := NewGateway(base, fake, nil, ownerIs("teacher-1"), nil, discardLog())
	g.SetAttemptOwner(attemptOwnedBy("student-1"))

	called := make(chan struct{}, 1)
	g.SetSessionInvalidated(func(context.Context, string) { called <- struct{}{} })
	_, wsURL := mountAttempt(t, g, student("student-1"))

	ctx, c := dial(t, wsURL)
	defer c.CloseNow()

	select {
	case <-called:
		t.Fatal("SessionInvalidatedFunc fired on the first connection")
	case <-time.After(200 * time.Millisecond):
	}

	// The lone socket should still be alive and relaying normally.
	fake.emit(`{"type":"attempt.progress","attempt_id":"` + testAttemptID + `","payload":{}}`)
	fake.emit(`{"type":"quiz.extended","attempt_id":"","payload":{}}`)
	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	if _, _, err := c.Read(readCtx); err != nil {
		t.Fatalf("read: %v", err)
	}
}
