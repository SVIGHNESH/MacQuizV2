package realtime

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"macquiz/server/internal/authusers"
)

// This file covers docs/05 section 3's periodic authorization revalidation:
// every open socket re-runs its subscribe-time decision once per
// revalidateInterval, so disabling an account, demoting a role, or moving a
// quiz to a new owner drops the socket instead of streaming to it for the
// rest of the exam. Every test here shrinks the interval to milliseconds
// rather than waiting the production minute.

// setRevalidateInterval shrinks the package's revalidateInterval for the
// duration of a test, mirroring setHeartbeatTimeout's straight-line teardown.
func setRevalidateInterval(t *testing.T, d time.Duration) func() {
	t.Helper()
	prev := revalidateInterval
	revalidateInterval = d
	return func() { revalidateInterval = prev }
}

// fakeUsers is a mutable stand-in for authusers.Service.UserByID: a test flips
// the account mid-socket (disable it, demote it, delete it) and the next
// revalidation tick sees the new value, which is precisely the scenario the
// feature exists for.
type fakeUsers struct {
	mu   sync.Mutex
	user authusers.User
	err  error
}

func newFakeUsers(u authusers.User) *fakeUsers { return &fakeUsers{user: u} }

func (f *fakeUsers) lookup(context.Context, string) (authusers.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.user, f.err
}

func (f *fakeUsers) set(u authusers.User, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.user, f.err = u, err
}

// mutableOwner is a QuizOwnerFunc whose answer a test can change mid-socket,
// standing in for a quiz whose owner is reassigned.
type mutableOwner struct {
	mu      sync.Mutex
	ownerID string
}

func (m *mutableOwner) fn(context.Context, string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ownerID, true, nil
}

func (m *mutableOwner) set(ownerID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ownerID = ownerID
}

// expectAuthRevoked asserts the socket closed with statusAuthRevoked - the
// code that tells the client not to reconnect - within the deadline.
func expectAuthRevoked(t *testing.T, ctx context.Context, c *websocket.Conn) {
	t.Helper()
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, _, err := c.Read(readCtx)
	if err == nil {
		t.Fatal("socket stayed open after authorization was revoked")
	}
	if got := websocket.CloseStatus(err); got != statusAuthRevoked {
		t.Fatalf("close status = %d, want %d (auth revoked); err = %v", got, statusAuthRevoked, err)
	}
}

// expectStillRelaying asserts the socket is alive by pushing an event and
// reading it back - the only proof that outlasts several revalidation ticks.
func expectStillRelaying(t *testing.T, ctx context.Context, c *websocket.Conn, fake *fakeSubscriber) {
	t.Helper()
	fake.emit(`{"type":"attempt.progress","attempt_id":"a-1","payload":{}}`)
	readCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, data, err := c.Read(readCtx)
	if err != nil {
		t.Fatalf("socket died instead of surviving revalidation: %v", err)
	}
	if got := string(data); got == "" {
		t.Fatal("empty payload")
	}
}

// TestMonitorRevalidationKeepsActiveUserConnected proves the ordinary case:
// an owning teacher whose account stays active keeps their socket across many
// revalidation ticks. Without this, a fail-closed bug in the revalidate path
// would show up as "every monitor drops after a minute" in production only.
func TestMonitorRevalidationKeepsActiveUserConnected(t *testing.T) {
	defer setRevalidateInterval(t, 50*time.Millisecond)()

	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := newFakeSubscriber()
	owner := authusers.User{ID: "teacher-1", Role: "teacher", Status: "active"}
	g := NewGateway(base, fake, nil, ownerIs(owner.ID), nil, discardLog())
	g.SetUserLookup(newFakeUsers(owner).lookup)
	_, wsURL := mountMonitor(t, g, owner, 0)

	ctx, c := dial(t, wsURL)
	defer c.CloseNow()

	time.Sleep(250 * time.Millisecond) // 5 revalidations
	expectStillRelaying(t, ctx, c, fake)
}

// TestMonitorRevalidationClosesDisabledUser is the headline case: an admin
// disables a teacher mid-exam and their live monitor drops within one
// interval, the same way their next REST request would be refused
// (authusers.RequireAuth reloads the account on every call).
func TestMonitorRevalidationClosesDisabledUser(t *testing.T) {
	defer setRevalidateInterval(t, 50*time.Millisecond)()

	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := authusers.User{ID: "teacher-1", Role: "teacher", Status: "active"}
	users := newFakeUsers(owner)
	g := NewGateway(base, newFakeSubscriber(), nil, ownerIs(owner.ID), nil, discardLog())
	g.SetUserLookup(users.lookup)
	_, wsURL := mountMonitor(t, g, owner, 0)

	ctx, c := dial(t, wsURL)
	defer c.CloseNow()

	users.set(authusers.User{ID: owner.ID, Role: "teacher", Status: "disabled"}, nil)
	expectAuthRevoked(t, ctx, c)
}

// TestMonitorRevalidationClosesDeletedUser proves a deleted account (the
// lookup's sql.ErrNoRows, exactly what RequireAuth reads as "account no
// longer exists") is a revocation, not a transient error to retry.
func TestMonitorRevalidationClosesDeletedUser(t *testing.T) {
	defer setRevalidateInterval(t, 50*time.Millisecond)()

	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := authusers.User{ID: "teacher-1", Role: "teacher", Status: "active"}
	users := newFakeUsers(owner)
	g := NewGateway(base, newFakeSubscriber(), nil, ownerIs(owner.ID), nil, discardLog())
	g.SetUserLookup(users.lookup)
	_, wsURL := mountMonitor(t, g, owner, 0)

	ctx, c := dial(t, wsURL)
	defer c.CloseNow()

	users.set(authusers.User{}, sql.ErrNoRows)
	expectAuthRevoked(t, ctx, c)
}

// TestMonitorRevalidationClosesDemotedTeacher proves the resource check runs
// against the FRESH user, not the one cached at subscribe time: a teacher
// demoted to student still "owns" the quiz row, so only a re-read of the role
// can revoke the socket. This is the whole reason freshActor returns the user.
func TestMonitorRevalidationClosesDemotedTeacher(t *testing.T) {
	defer setRevalidateInterval(t, 50*time.Millisecond)()

	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := authusers.User{ID: "teacher-1", Role: "teacher", Status: "active"}
	users := newFakeUsers(owner)
	g := NewGateway(base, newFakeSubscriber(), nil, ownerIs(owner.ID), nil, discardLog())
	g.SetUserLookup(users.lookup)
	_, wsURL := mountMonitor(t, g, owner, 0)

	ctx, c := dial(t, wsURL)
	defer c.CloseNow()

	users.set(authusers.User{ID: owner.ID, Role: "student", Status: "active"}, nil)
	expectAuthRevoked(t, ctx, c)
}

// TestMonitorRevalidationClosesOnOwnerChange proves the resource half of the
// check: the account is untouched, but the quiz now belongs to someone else,
// so Can() says no and the socket drops.
func TestMonitorRevalidationClosesOnOwnerChange(t *testing.T) {
	defer setRevalidateInterval(t, 50*time.Millisecond)()

	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := authusers.User{ID: "teacher-1", Role: "teacher", Status: "active"}
	quizOwner := &mutableOwner{ownerID: owner.ID}
	g := NewGateway(base, newFakeSubscriber(), nil, quizOwner.fn, nil, discardLog())
	g.SetUserLookup(newFakeUsers(owner).lookup)
	_, wsURL := mountMonitor(t, g, owner, 0)

	ctx, c := dial(t, wsURL)
	defer c.CloseNow()

	quizOwner.set("teacher-2")
	expectAuthRevoked(t, ctx, c)
}

// TestMonitorRevalidationSurvivesLookupOutage pins the fail-open-on-outage
// decision: a Postgres blip is not a revocation. Dropping every live socket on
// a transient database error would turn a hiccup into an exam-wide outage, so
// the gateway logs it and re-checks on the next tick - and once the lookup
// recovers as *disabled*, the socket still closes.
func TestMonitorRevalidationSurvivesLookupOutage(t *testing.T) {
	defer setRevalidateInterval(t, 50*time.Millisecond)()

	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := newFakeSubscriber()
	owner := authusers.User{ID: "teacher-1", Role: "teacher", Status: "active"}
	users := newFakeUsers(owner)
	g := NewGateway(base, fake, nil, ownerIs(owner.ID), nil, discardLog())
	g.SetUserLookup(users.lookup)
	_, wsURL := mountMonitor(t, g, owner, 0)

	ctx, c := dial(t, wsURL)
	defer c.CloseNow()

	users.set(authusers.User{}, errors.New("connection refused"))
	time.Sleep(250 * time.Millisecond) // 5 failed revalidations
	expectStillRelaying(t, ctx, c, fake)

	users.set(authusers.User{ID: owner.ID, Role: "teacher", Status: "disabled"}, nil)
	expectAuthRevoked(t, ctx, c)
}

// TestAttemptRevalidationClosesDisabledStudent proves the attempt:{id} socket
// carries the same guarantee: a student disabled mid-attempt loses the channel
// that would otherwise keep delivering their kick/banner events. (Their
// answers are already refused by RequireAuth on the REST write path.)
func TestAttemptRevalidationClosesDisabledStudent(t *testing.T) {
	defer setRevalidateInterval(t, 50*time.Millisecond)()

	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	stu := student("student-1")
	users := newFakeUsers(stu)
	g := NewGateway(base, newFakeSubscriber(), nil, ownerIs("teacher-1"), nil, discardLog())
	g.SetAttemptOwner(attemptOwnedBy(stu.ID))
	g.SetUserLookup(users.lookup)
	_, wsURL := mountAttempt(t, g, stu)

	ctx, c := dial(t, wsURL)
	defer c.CloseNow()

	users.set(authusers.User{ID: stu.ID, Role: "student", Status: "disabled"}, nil)
	expectAuthRevoked(t, ctx, c)
}

// TestNotifyRevalidationClosesDisabledUser proves the notify channel is
// covered too. Its subscribe-time check is a pure id comparison (the channel's
// name is its authorization), so the account reload is the only thing
// revalidation can add here - and it is exactly what a disabled account needs.
func TestNotifyRevalidationClosesDisabledUser(t *testing.T) {
	defer setRevalidateInterval(t, 50*time.Millisecond)()

	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	user := student(testUserID)
	users := newFakeUsers(user)
	g := NewGateway(base, newFakeSubscriber(), nil, ownerIs("teacher-1"), nil, discardLog())
	g.SetUserLookup(users.lookup)
	_, wsURL := mountNotify(t, g, user)

	ctx, c := dial(t, wsURL)
	defer c.CloseNow()

	users.set(authusers.User{ID: user.ID, Role: "student", Status: "disabled"}, nil)
	expectAuthRevoked(t, ctx, c)
}

// TestRevalidationWithoutUserLookupSkipsAccountCheck proves the nil-lookup
// seam: a Gateway built with no auth service (every existing test, and any
// caller that never wires one) revalidates only the resource half rather than
// panicking or closing every socket.
func TestRevalidationWithoutUserLookupSkipsAccountCheck(t *testing.T) {
	defer setRevalidateInterval(t, 50*time.Millisecond)()

	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := newFakeSubscriber()
	owner := authusers.User{ID: "teacher-1", Role: "teacher", Status: "active"}
	g := NewGateway(base, fake, nil, ownerIs(owner.ID), nil, discardLog())
	_, wsURL := mountMonitor(t, g, owner, 0)

	ctx, c := dial(t, wsURL)
	defer c.CloseNow()

	time.Sleep(250 * time.Millisecond)
	expectStillRelaying(t, ctx, c, fake)
}
