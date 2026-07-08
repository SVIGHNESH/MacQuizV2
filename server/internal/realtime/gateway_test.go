package realtime

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"macquiz/server/internal/authusers"
)

const testQuizID = "11111111-1111-1111-1111-111111111111"

// fakeSubscriber is a Redis-free Subscriber: every Subscribe hands back the
// same in-memory stream, so a test can push payloads with emit() and watch
// them arrive on the socket. It proves the socket lifecycle, fan-out, and
// context detachment without a live Redis - only the thin go-redis wiring is
// left to the gated smoke test.
type fakeSubscriber struct{ ch chan string }

func newFakeSubscriber() *fakeSubscriber { return &fakeSubscriber{ch: make(chan string, 8)} }

func (f *fakeSubscriber) Subscribe(context.Context, string) (Subscription, error) {
	return fakeSubscription{ch: f.ch}, nil
}
func (f *fakeSubscriber) Close() error        { return nil }
func (f *fakeSubscriber) emit(payload string) { f.ch <- payload }

type fakeSubscription struct{ ch chan string }

func (f fakeSubscription) Messages() <-chan string { return f.ch }
func (f fakeSubscription) Close() error            { return nil }

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// mountMonitor wires handleMonitor behind an injected actor (bypassing the JWT
// path RequireAuth would otherwise run) and an optional request timeout, so a
// test drives the real handler without a database or signed token. It returns
// the httptest server and the ws:// URL for the monitor endpoint.
func mountMonitor(t *testing.T, g *Gateway, actor authusers.User, reqTimeout time.Duration) (*httptest.Server, string) {
	t.Helper()
	r := chi.NewRouter()
	if reqTimeout > 0 {
		r.Use(middleware.Timeout(reqTimeout))
	}
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(authusers.WithActor(req.Context(), actor)))
		})
	})
	r.Get("/ws/quizzes/{id}/monitor", g.handleMonitor)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/quizzes/" + testQuizID + "/monitor"
	return srv, wsURL
}

func ownerIs(ownerID string) QuizOwnerFunc {
	return func(context.Context, string) (string, bool, error) { return ownerID, true, nil }
}

func admin() authusers.User {
	return authusers.User{ID: "admin-1", Role: "admin", Status: "active"}
}

// TestMonitorRelaysEvents proves the happy path: an owning teacher connects,
// and a payload pushed to the subscription arrives verbatim on the socket.
func TestMonitorRelaysEvents(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := newFakeSubscriber()
	owner := authusers.User{ID: "teacher-1", Role: "teacher", Status: "active"}
	g := NewGateway(base, fake, nil, ownerIs(owner.ID), nil, discardLog())
	_, wsURL := mountMonitor(t, g, owner, 0)

	ctx, c := dial(t, wsURL)
	defer c.CloseNow()

	fake.emit(`{"type":"attempt.started","attempt_id":"a-1","payload":{}}`)
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(data); !strings.Contains(got, `"attempt.started"`) {
		t.Fatalf("relayed %q, want the emitted envelope", got)
	}
}

// TestMonitorSocketOutlivesRequestTimeout is the regression guard for the root
// router's middleware.Timeout: the handler must detach the socket lifetime from
// the request context, or every monitor socket would be guillotined when that
// timeout fires. We mount a deliberately short 300 ms timeout, wait past it,
// then push - the message must still arrive.
func TestMonitorSocketOutlivesRequestTimeout(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := newFakeSubscriber()
	g := NewGateway(base, fake, nil, ownerIs("admin-1"), nil, discardLog())
	_, wsURL := mountMonitor(t, g, admin(), 300*time.Millisecond)

	ctx, c := dial(t, wsURL)
	defer c.CloseNow()

	// Sleep well past the request timeout; a socket bound to the request
	// context would already be closed here.
	time.Sleep(600 * time.Millisecond)
	fake.emit(`{"type":"attempt.progress","attempt_id":"a-2","payload":{}}`)

	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	_, data, err := c.Read(readCtx)
	if err != nil {
		t.Fatalf("socket died at the request timeout instead of outliving it: %v", err)
	}
	if !strings.Contains(string(data), `"attempt.progress"`) {
		t.Fatalf("relayed %q, want the post-timeout envelope", data)
	}
}

// TestMonitorShutdownClosesSocket proves the socket is bound to the gateway's
// base context: canceling it (the SIGTERM path) ends the pump and closes the
// socket, so graceful shutdown never blocks on an idle monitor.
func TestMonitorShutdownClosesSocket(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	fake := newFakeSubscriber()
	g := NewGateway(base, fake, nil, ownerIs("admin-1"), nil, discardLog())
	_, wsURL := mountMonitor(t, g, admin(), 0)

	ctx, c := dial(t, wsURL)
	defer c.CloseNow()

	cancel() // simulate process shutdown
	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	if _, _, err := c.Read(readCtx); err == nil {
		t.Fatal("expected the socket to close when the base context is canceled")
	}
}

// TestMonitorNonOwnerTeacher404 proves a teacher who does not own the quiz is
// refused before any upgrade with a 404 (existence is not leaked), mirroring
// GET /quizzes/:id/live.
func TestMonitorNonOwnerTeacher404(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := NewGateway(base, newFakeSubscriber(), nil, ownerIs("someone-else"), nil, discardLog())
	srv, _ := mountMonitor(t, g, authusers.User{ID: "teacher-9", Role: "teacher", Status: "active"}, 0)

	resp := plainGet(t, srv.URL+"/ws/quizzes/"+testQuizID+"/monitor")
	if resp != http.StatusNotFound {
		t.Fatalf("non-owner teacher status = %d, want 404", resp)
	}
}

// TestMonitorUnknownQuiz404 proves an unknown quiz (owner lookup found=false)
// is a 404, even for an admin who could watch any quiz that existed.
func TestMonitorUnknownQuiz404(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	notFound := func(context.Context, string) (string, bool, error) { return "", false, nil }
	g := NewGateway(base, newFakeSubscriber(), nil, notFound, nil, discardLog())
	srv, _ := mountMonitor(t, g, admin(), 0)

	resp := plainGet(t, srv.URL+"/ws/quizzes/"+testQuizID+"/monitor")
	if resp != http.StatusNotFound {
		t.Fatalf("unknown quiz status = %d, want 404", resp)
	}
}

// TestMonitorBadUUID404 proves a malformed quiz id never reaches the owner
// lookup or a Postgres uuid cast.
func TestMonitorBadUUID404(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := NewGateway(base, newFakeSubscriber(), nil, ownerIs("admin-1"), nil, discardLog())
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(authusers.WithActor(req.Context(), admin())))
		})
	})
	r.Get("/ws/quizzes/{id}/monitor", g.handleMonitor)
	srv := httptest.NewServer(r)
	defer srv.Close()

	if resp := plainGet(t, srv.URL+"/ws/quizzes/not-a-uuid/monitor"); resp != http.StatusNotFound {
		t.Fatalf("bad uuid status = %d, want 404", resp)
	}
}

// TestRequireStaffRejectsStudent proves the coarse role gate answers 403 for a
// student, before the owner-vs-admin resource decision is ever reached.
func TestRequireStaffRejectsStudent(t *testing.T) {
	cases := []struct {
		role string
		want int
	}{
		{"student", http.StatusForbidden},
		{"teacher", http.StatusOK},
		{"admin", http.StatusOK},
	}
	for _, tc := range cases {
		next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
		h := requireStaff(next)
		req := httptest.NewRequest(http.MethodGet, "/ws/quizzes/"+testQuizID+"/monitor", nil)
		req = req.WithContext(authusers.WithActor(req.Context(),
			authusers.User{ID: "u", Role: tc.role, Status: "active"}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("role %s status = %d, want %d", tc.role, rec.Code, tc.want)
		}
	}
}

// TestMonitorThroughWrappedResponseWriter proves the upgrade still hijacks
// through chi's ResponseWriter-wrapping middleware (RequestID + Recoverer wrap
// the writer the way httpserver.New does), so the real server stack cannot
// silently break the handshake.
func TestMonitorThroughWrappedResponseWriter(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := newFakeSubscriber()
	g := NewGateway(base, fake, nil, ownerIs("admin-1"), nil, discardLog())

	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Recoverer)
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(authusers.WithActor(req.Context(), admin())))
		})
	})
	r.Get("/ws/quizzes/{id}/monitor", g.handleMonitor)
	srv := httptest.NewServer(r)
	defer srv.Close()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/quizzes/" + testQuizID + "/monitor"
	ctx, c := dial(t, wsURL)
	defer c.CloseNow()

	fake.emit(`{"type":"attempt.submitted","attempt_id":"a-3","payload":{}}`)
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read through wrapped writer: %v", err)
	}
	if !strings.Contains(string(data), `"attempt.submitted"`) {
		t.Fatalf("relayed %q, want the emitted envelope", data)
	}
}

const testAttemptID = "22222222-2222-2222-2222-222222222222"

func attemptOwnedBy(studentID string) AttemptOwnerFunc {
	return func(context.Context, string) (string, string, bool, error) {
		return studentID, testQuizID, true, nil
	}
}

func student(id string) authusers.User {
	return authusers.User{ID: id, Role: "student", Status: "active"}
}

// mountAttempt wires handleAttempt behind an injected actor, mirroring
// mountMonitor: it returns the httptest server and the ws:// URL for the
// attempt channel endpoint.
func mountAttempt(t *testing.T, g *Gateway, actor authusers.User) (*httptest.Server, string) {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(authusers.WithActor(req.Context(), actor)))
		})
	})
	r.Get("/ws/attempts/{id}", g.handleAttempt)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws/attempts/" + testAttemptID
	return srv, wsURL
}

// TestAttemptRelaysKickThenCloses proves the happy path from docs/06 section
// 4 step 4: the owning student receives the kick envelope for their own
// attempt, and the socket force-closes right after.
func TestAttemptRelaysKickThenCloses(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := newFakeSubscriber()
	g := NewGateway(base, fake, nil, ownerIs("teacher-1"), nil, discardLog())
	g.SetAttemptOwner(attemptOwnedBy("student-1"))
	_, wsURL := mountAttempt(t, g, student("student-1"))

	ctx, c := dial(t, wsURL)
	defer c.CloseNow()

	fake.emit(`{"type":"attempt.kicked","attempt_id":"` + testAttemptID + `","payload":{"reason":"cheating"}}`)
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(data); !strings.Contains(got, `"attempt.kicked"`) {
		t.Fatalf("relayed %q, want the kicked envelope", got)
	}
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("expected the socket to force-close after delivering the kick")
	}
}

// TestAttemptIgnoresUnrelatedEvents proves the channel only ever relays a
// kick for this attempt (or a quiz-wide banner) - not the full monitor event
// stream, and not a kick belonging to a different attempt on the same quiz.
func TestAttemptIgnoresUnrelatedEvents(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := newFakeSubscriber()
	g := NewGateway(base, fake, nil, ownerIs("teacher-1"), nil, discardLog())
	g.SetAttemptOwner(attemptOwnedBy("student-1"))
	_, wsURL := mountAttempt(t, g, student("student-1"))

	ctx, c := dial(t, wsURL)
	defer c.CloseNow()

	fake.emit(`{"type":"attempt.progress","attempt_id":"` + testAttemptID + `","payload":{}}`)
	fake.emit(`{"type":"attempt.kicked","attempt_id":"some-other-attempt","payload":{}}`)
	fake.emit(`{"type":"attempt.submitted","attempt_id":"` + testAttemptID + `","payload":{}}`)
	fake.emit(`{"type":"quiz.extended","attempt_id":"","payload":{"new_ends_at":"2026-01-01T00:00:00Z"}}`)

	readCtx, readCancel := context.WithTimeout(ctx, 2*time.Second)
	defer readCancel()
	_, data, err := c.Read(readCtx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := string(data); !strings.Contains(got, `"quiz.extended"`) {
		t.Fatalf("relayed %q, want only the quiz-wide banner (progress/submitted/other-attempt-kick must be dropped)", got)
	}
}

// TestAttemptNonOwnerStudent404 proves a student who does not own the
// attempt is refused before any upgrade, existence never leaked.
func TestAttemptNonOwnerStudent404(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := NewGateway(base, newFakeSubscriber(), nil, ownerIs("teacher-1"), nil, discardLog())
	g.SetAttemptOwner(attemptOwnedBy("student-1"))
	srv, _ := mountAttempt(t, g, student("student-9"))

	resp := plainGet(t, srv.URL+"/ws/attempts/"+testAttemptID)
	if resp != http.StatusNotFound {
		t.Fatalf("non-owner student status = %d, want 404", resp)
	}
}

// TestAttemptTeacherRejected404 proves a teacher (even the quiz's own
// teacher) cannot subscribe to a student's attempt channel: docs/04-api.md
// section 6 scopes it to the attempt owner only.
func TestAttemptTeacherRejected404(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := NewGateway(base, newFakeSubscriber(), nil, ownerIs("teacher-1"), nil, discardLog())
	g.SetAttemptOwner(attemptOwnedBy("student-1"))
	srv, _ := mountAttempt(t, g, authusers.User{ID: "teacher-1", Role: "teacher", Status: "active"})

	resp := plainGet(t, srv.URL+"/ws/attempts/"+testAttemptID)
	if resp != http.StatusNotFound {
		t.Fatalf("teacher status = %d, want 404", resp)
	}
}

// TestAttemptUnknownAttempt404 proves an unknown attempt (owner lookup
// found=false) is a 404.
func TestAttemptUnknownAttempt404(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	notFound := func(context.Context, string) (string, string, bool, error) { return "", "", false, nil }
	g := NewGateway(base, newFakeSubscriber(), nil, ownerIs("teacher-1"), nil, discardLog())
	g.SetAttemptOwner(notFound)
	srv, _ := mountAttempt(t, g, student("student-1"))

	resp := plainGet(t, srv.URL+"/ws/attempts/"+testAttemptID)
	if resp != http.StatusNotFound {
		t.Fatalf("unknown attempt status = %d, want 404", resp)
	}
}

// TestAttemptNoOwnerFuncWired404 proves a Gateway that never had
// SetAttemptOwner called (every deploy before this brick lands, and every
// test exercising only the monitor channel) answers 404 instead of a nil
// function panic.
func TestAttemptNoOwnerFuncWired404(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	g := NewGateway(base, newFakeSubscriber(), nil, ownerIs("teacher-1"), nil, discardLog())
	srv, _ := mountAttempt(t, g, student("student-1"))

	resp := plainGet(t, srv.URL+"/ws/attempts/"+testAttemptID)
	if resp != http.StatusNotFound {
		t.Fatalf("no attempt owner wired status = %d, want 404", resp)
	}
}

// dial opens a WebSocket client to url and returns a context for its
// operations. The dialer sends no Origin header, so Accept's same-origin check
// passes without OriginPatterns.
func dial(t *testing.T, url string) (context.Context, *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	c, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	return ctx, c
}

// plainGet issues an ordinary HTTP GET (no upgrade headers) and returns the
// status code, for asserting the pre-upgrade authorization outcomes.
func plainGet(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
