package realtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/httpapi"
	"macquiz/server/internal/telemetry"
)

// This file owns hop 3 of the docs/05 section 1 pipeline: the gateway,
// subscribed to quiz:{id}:events, fans each committed event out to every
// authorized WebSocket on the teacher/admin monitor channel. It relays, it
// never decides (doc.go): auth is one Can() check at subscribe (docs/05
// section 3), and every payload is the source-of-truth envelope the attempt
// module already persisted and published.
//
// This brick also lands the attempt:{id} student channel (docs/05 section 3):
// the attempt's owning student subscribes to the same quiz:{id}:events stream
// and receives only the events scoped to their own attempt (today: the kick
// lockout message, docs/06 section 4 step 4) plus any quiz-wide banner
// events. It also enforces docs/08 section 1's single active session: only
// one attempt:{id} socket per attempt may be open at a time, a second
// device's connect force-closes the first.
//
// It also lands the user:{id}:notify channel (docs/05 section 3): a much
// simpler relay than either of the above, since the channel's own name is
// the authorization decision - "the user themselves" (docs/04-api.md section
// 6) needs no owner lookup, just comparing the URL id to the authenticated
// actor. It carries quiz.assigned/quiz.unassigned (quiz/events.go), the only
// notifications the codebase produces today.
//
// Finally, this file owns heartbeat/disconnect tracking (docs/05 section 5):
// the attempt:{id} socket now runs a real read loop instead of CloseRead's
// discard-everything drain, treating any inbound frame from the student
// client as a heartbeat. If heartbeatTimeout elapses with no heartbeat, the
// gateway calls AttemptDisconnectedFunc (attempt.disconnected); a heartbeat
// that arrives afterward calls AttemptReconnectedFunc (attempt.reconnected).
// Both land on the same quiz:{quiz_id}:events stream the monitor dashboard
// already watches, so the amber "disconnected" row needs no new channel.

const (
	// pingInterval keeps the socket (and any intermediate proxy) alive across
	// the long idle gaps between events on a quiet quiz.
	pingInterval = 30 * time.Second
	// writeTimeout bounds a single frame write/ping so one stuck client can
	// never wedge its pump goroutine.
	writeTimeout = 10 * time.Second
)

// heartbeatTimeout is how long the attempt:{id} socket waits for an inbound
// frame before the gateway marks the attempt disconnected. docs/05 section 5
// fixes the client's heartbeat cadence at 10s; 2.5x that absorbs a couple of
// missed beats (a slow network tick, a backgrounded tab throttling timers)
// without flapping the dashboard on every minor hiccup. A var, not a const,
// so tests can shrink it rather than sleeping 25s real time.
var heartbeatTimeout = 25 * time.Second

// QuizOwnerFunc resolves a quiz's owning teacher for the subscribe-time
// authorization decision. found is false for an unknown quiz, which the
// gateway answers as 404 - existence is never leaked to a non-owner (docs/04
// section 1), matching quiz.LiveRoster.
type QuizOwnerFunc func(ctx context.Context, quizID string) (ownerID string, found bool, err error)

// AttemptOwnerFunc resolves an attempt's owning student and quiz for the
// attempt:{id} channel's subscribe-time authorization (docs/04-api.md
// section 6: "the student who owns the attempt"). found is false for an
// unknown attempt, answered as 404 exactly like QuizOwnerFunc.
type AttemptOwnerFunc func(ctx context.Context, attemptID string) (studentID, quizID string, found bool, err error)

// eventKicked is the docs/05 section 2 event type this channel force-closes
// on (docs/06 section 4 step 4). Duplicated as a literal rather than imported
// from the attempt package, which this package does not (and should not)
// depend on - the gateway only ever relays the envelope it is handed.
const eventKicked = "attempt.kicked"

// statusSessionReplaced is the close code the gateway uses to force-close a
// student's stale attempt:{id} socket when a second device's connects for the
// same attempt (docs/08 section 1 "single active session"). RFC 6455 section
// 7.4.2 reserves 4000-4999 for private application use. The client
// (web/src/player/AttemptPlayer.tsx) checks for this code to skip the
// reconnect it otherwise always attempts on close - without it, each device's
// reconnect would boot the other in an endless loop.
const statusSessionReplaced websocket.StatusCode = 4001

// SessionInvalidatedFunc records that an attempt's stale socket was replaced
// by a newer connection (docs/08 section 1: "...logged as an event the
// teacher can see"). Called after the old socket has already been told to
// close - it is a best-effort audit write, never a gate on the new
// connection - so a slow or failing implementation only delays the log
// entry, not the student who just reconnected.
type SessionInvalidatedFunc func(ctx context.Context, attemptID string)

// AttemptDisconnectedFunc records that an attempt:{id} socket has gone quiet
// past heartbeatTimeout (docs/05 section 5). lastSeenAt is the time of the
// last heartbeat frame the gateway actually received (or the connect time, if
// none ever arrived). Best-effort, like SessionInvalidatedFunc: it runs after
// the dashboard-facing publish is already in flight, never a gate on it.
type AttemptDisconnectedFunc func(ctx context.Context, attemptID string, lastSeenAt time.Time)

// AttemptReconnectedFunc records that a heartbeat arrived on a socket the
// gateway had already marked disconnected (docs/05 section 2: "Flag
// cleared").
type AttemptReconnectedFunc func(ctx context.Context, attemptID string)

// Gateway is the WebSocket monitor endpoint. It holds no quiz state: the
// snapshot the dashboard reconciles against comes from GET /quizzes/:id/live,
// and the gateway only streams the deltas published after it.
type Gateway struct {
	sub                 Subscriber
	auth                *authusers.Service
	owner               QuizOwnerFunc
	attemptOwner        AttemptOwnerFunc
	sessionInvalidated  SessionInvalidatedFunc
	attemptDisconnected AttemptDisconnectedFunc
	attemptReconnected  AttemptReconnectedFunc
	origins             []string
	log                 *slog.Logger
	metrics             *telemetry.Metrics

	// baseCtx is the socket lifetime, derived from the serve process context
	// so every open socket closes on shutdown. Critically it is NOT the
	// request context: the root router's middleware.Timeout(30s) cancels the
	// request context at 30 s, which would guillotine every monitor socket.
	baseCtx context.Context

	// activeAttempts tracks the one open attempt:{id} socket per attempt
	// (docs/08 section 1 "single active session"), keyed by attempt ID. A
	// second device's connection force-closes whatever conn is currently
	// registered before replacing it; this map is the only quiz state the
	// gateway holds (everything else is either request-scoped or fetched from
	// the snapshot endpoint).
	attemptsMu     sync.Mutex
	activeAttempts map[string]*websocket.Conn
}

// SetMetrics wires the docs/10-operations.md section 2 WebSocket connection
// count. Optional: a Gateway with no metrics set records nothing, which is
// what every existing test gets.
func (g *Gateway) SetMetrics(m *telemetry.Metrics) {
	g.metrics = m
}

// SetAttemptOwner wires the attempt:{id} channel's owner lookup. Optional, a
// setter like SetMetrics rather than a NewGateway parameter, so every
// existing caller and test that only exercises the monitor channel compiles
// unchanged; a Gateway with no attempt owner wired answers every attempt
// socket request 404 (attemptOwner is checked for nil before ever being
// called).
func (g *Gateway) SetAttemptOwner(f AttemptOwnerFunc) {
	g.attemptOwner = f
}

// SetSessionInvalidated wires the single-active-session audit log (docs/08
// section 1). Optional, a setter like SetMetrics: a Gateway with none set
// still enforces one socket per attempt, it just does not log it, which is
// what every existing test gets.
func (g *Gateway) SetSessionInvalidated(f SessionInvalidatedFunc) {
	g.sessionInvalidated = f
}

// SetAttemptDisconnected wires the heartbeat-timeout handler (docs/05 section
// 5). Unlike SessionInvalidatedFunc (a pure audit log layered on top of a
// close the gateway always performs), this callback is the only thing that
// appends the attempt_events row *and* publishes attempt.disconnected to the
// dashboard - a Gateway with none set still tracks heartbeats but the
// dashboard never learns about a lapse, which is what every existing test
// gets.
func (g *Gateway) SetAttemptDisconnected(f AttemptDisconnectedFunc) {
	g.attemptDisconnected = f
}

// SetAttemptReconnected wires the reconnect handler, the counterpart to
// SetAttemptDisconnected.
func (g *Gateway) SetAttemptReconnected(f AttemptReconnectedFunc) {
	g.attemptReconnected = f
}

// NewGateway wires the gateway. ctx is the serve-process lifetime; when it is
// canceled (SIGTERM) every open socket's pump returns, so graceful shutdown
// does not block on the router's Timeout grace. origins is the allowed set of
// WebSocket Origin patterns (coder/websocket rejects cross-origin by default);
// an empty list means same-origin only.
func NewGateway(ctx context.Context, sub Subscriber, auth *authusers.Service, owner QuizOwnerFunc, origins []string, log *slog.Logger) *Gateway {
	return &Gateway{
		sub: sub, auth: auth, owner: owner, origins: origins, log: log, baseCtx: ctx,
		activeAttempts: make(map[string]*websocket.Conn),
	}
}

// Routes returns the /ws route group. Authentication rides the same
// RequireAuth middleware as the REST surface - a browser cannot set an
// Authorization header on a WebSocket handshake, but RequireAuth also accepts
// the access-token cookie, which the browser does send. The coarse
// teacher-or-admin role gate answers 403; the owner-vs-admin resource decision
// stays in the handler and answers 404, exactly mirroring GET /quizzes/:id/live.
func (g *Gateway) Routes() http.Handler {
	r := chi.NewRouter()
	r.Use(g.auth.RequireAuth, authusers.RequirePasswordChanged)
	r.Group(func(r chi.Router) {
		r.Use(requireStaff)
		r.Get("/quizzes/{id}/monitor", g.handleMonitor)
	})
	r.Get("/attempts/{id}", g.handleAttempt)
	r.Get("/users/{id}/notify", g.handleNotify)
	return r
}

// handleMonitor authorizes the subscribe, upgrades the connection, and streams
// quiz:{id}:events to the teacher/admin dashboard until the client disconnects
// or the process shuts down.
func (g *Gateway) handleMonitor(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	quizID := chi.URLParam(r, "id")
	if !uuidShape.MatchString(quizID) {
		httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound, "no such quiz")
		return
	}

	// Authorize before the upgrade so a denial (or an unknown quiz) is a clean
	// HTTP status, never a half-open socket. found+Can together answer 404 for
	// both the absent quiz and the non-owning teacher (docs/05 section 3).
	ownerID, found, err := g.owner(r.Context(), quizID)
	if err != nil {
		g.log.Error("resolve quiz owner", "quiz_id", quizID, "err", err)
		httpapi.WriteError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	if !found || !authusers.Can(actor, authusers.ActionQuizWatchLive, authusers.Resource{OwnerID: ownerID}) {
		httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound, "no such quiz")
		return
	}

	// Subscribe before upgrading: a Redis failure here is a clean 500, not a
	// socket that connects and then silently delivers nothing.
	connCtx, cancel := context.WithCancel(g.baseCtx)
	defer cancel()
	sub, err := g.sub.Subscribe(connCtx, eventsChannel(quizID))
	if err != nil {
		g.log.Error("subscribe monitor channel", "quiz_id", quizID, "err", err)
		httpapi.WriteError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	defer sub.Close()

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: g.origins})
	if err != nil {
		// Accept has already written the failure response.
		return
	}
	defer c.CloseNow()
	g.metrics.IncWSConnections(g.baseCtx)
	defer g.metrics.DecWSConnections(g.baseCtx)

	// CloseRead drains and discards inbound frames (the monitor socket is
	// write-only to the client) and returns a context canceled when the client
	// goes away. Derived from connCtx, so the pump also ends on shutdown.
	ctx := c.CloseRead(connCtx)
	g.pump(ctx, c, sub.Messages())
}

// pump relays payloads to the socket and keeps it warm with periodic pings. It
// returns - closing the socket via the caller's defer - on client disconnect,
// process shutdown, a write error, or the subscription ending.
func (g *Gateway) pump(ctx context.Context, c *websocket.Conn, msgs <-chan string) {
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		case payload, ok := <-msgs:
			if !ok {
				return
			}
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.Write(wctx, websocket.MessageText, []byte(payload))
			cancel()
			if err != nil {
				return
			}
		}
	}
}

// handleAttempt authorizes the subscribe against the attempt's own student,
// then streams the attempt's kick lockout message (and any future quiz-wide
// banner) until the client disconnects, the kick is delivered, or the
// process shuts down. There is no coarse role gate to mount this behind
// (unlike handleMonitor's requireStaff): a student is exactly who this
// channel is for, so the owner check below is the only gate.
func (g *Gateway) handleAttempt(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	attemptID := chi.URLParam(r, "id")
	if !uuidShape.MatchString(attemptID) || g.attemptOwner == nil {
		httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound, "no such attempt")
		return
	}

	// Authorize before the upgrade, exactly as handleMonitor does: a denial or
	// an unknown attempt is a clean 404, never a half-open socket. Only the
	// owning student may ever pass (docs/04-api.md section 6) - a teacher or
	// admin gets the same 404 as a stranger, since this channel carries no
	// resource they are entitled to watch.
	studentID, quizID, found, err := g.attemptOwner(r.Context(), attemptID)
	if err != nil {
		g.log.Error("resolve attempt owner", "attempt_id", attemptID, "err", err)
		httpapi.WriteError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	if !found || actor.Role != "student" || actor.ID != studentID {
		httpapi.WriteError(w, http.StatusNotFound, httpapi.CodeNotFound, "no such attempt")
		return
	}

	// Subscribe before upgrading, same reasoning as handleMonitor: a Redis
	// failure here is a clean 500, not a socket that connects and then
	// silently delivers nothing.
	connCtx, cancel := context.WithCancel(g.baseCtx)
	defer cancel()
	sub, err := g.sub.Subscribe(connCtx, eventsChannel(quizID))
	if err != nil {
		g.log.Error("subscribe attempt channel", "attempt_id", attemptID, "err", err)
		httpapi.WriteError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	defer sub.Close()

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: g.origins})
	if err != nil {
		// Accept has already written the failure response.
		return
	}
	defer c.CloseNow()
	g.metrics.IncWSConnections(g.baseCtx)
	defer g.metrics.DecWSConnections(g.baseCtx)

	g.replaceActiveAttempt(attemptID, c)
	defer g.clearActiveAttempt(attemptID, c)

	// A real read loop, not CloseRead's discard-and-reject-on-data-frame drain:
	// docs/05 section 5's heartbeat is a genuine inbound frame this channel
	// must now accept. Reading still keeps coder/websocket's internal
	// ping/pong handling alive, CloseRead's other job.
	hb := make(chan struct{})
	go g.readAttemptHeartbeats(connCtx, c, cancel, hb)
	g.pumpAttempt(connCtx, c, sub.Messages(), attemptID, hb)
}

// readAttemptHeartbeats is the attempt:{id} socket's read loop. Any inbound
// frame at all counts as a heartbeat (docs/05 section 5: the client's
// heartbeat is the only thing it ever sends on this channel) - the content is
// deliberately unchecked, since the read itself is the whole signal. cancel
// is called on a read error (peer gone, or our own side force-closing the
// conn elsewhere) so pumpAttempt's ctx.Done() unblocks and the handler
// unwinds the same way a CloseRead-driven disconnect used to.
func (g *Gateway) readAttemptHeartbeats(ctx context.Context, c *websocket.Conn, cancel context.CancelFunc, hb chan<- struct{}) {
	for {
		if _, _, err := c.Read(ctx); err != nil {
			cancel()
			return
		}
		select {
		case hb <- struct{}{}:
		case <-ctx.Done():
			return
		}
	}
}

// replaceActiveAttempt registers c as the one open socket for attemptID,
// force-closing (and logging) whatever connection held that slot before it -
// docs/08 section 1's "single active session": a second device's connect
// invalidates the first. The close runs in its own goroutine since the close
// handshake can block up to 5s waiting for the stale peer, and that must
// never delay accepting the new connection.
func (g *Gateway) replaceActiveAttempt(attemptID string, c *websocket.Conn) {
	g.attemptsMu.Lock()
	prev := g.activeAttempts[attemptID]
	g.activeAttempts[attemptID] = c
	g.attemptsMu.Unlock()
	if prev == nil {
		return
	}
	go func() {
		_ = prev.Close(statusSessionReplaced, "opened in another window or device")
	}()
	if g.sessionInvalidated != nil {
		go g.sessionInvalidated(g.baseCtx, attemptID)
	}
}

// clearActiveAttempt removes c's registration on socket teardown, but only if
// it is still the current holder: a socket that was itself replaced must not
// clobber the newer connection's entry when its own handler unwinds later.
func (g *Gateway) clearActiveAttempt(attemptID string, c *websocket.Conn) {
	g.attemptsMu.Lock()
	defer g.attemptsMu.Unlock()
	if g.activeAttempts[attemptID] == c {
		delete(g.activeAttempts, attemptID)
	}
}

// pumpAttempt relays what the docs/05 section 3 attempt:{id} channel
// promises: the kick lockout message for this attempt, and any quiz-wide
// banner event (a "quiz." prefix, e.g. quiz.extended/quiz.closed). It
// deliberately drops every other event on the shared quiz:{quiz_id}:events
// stream - attempt.progress/submitted/graded/violation are for the teacher's
// monitor, not an echo back to the student who already knows their own
// state; attempt.disconnected/reconnected (below) are the same - the
// dashboard's concern, not this socket's own client. It returns after
// delivering a kick for this attempt: docs/06 section 4 step 4, "delivers a
// lockout message... then force-closes it" - a kicked attempt has nothing
// further to receive.
//
// It also owns the docs/05 section 5 heartbeat: hb fires once per inbound
// frame (readAttemptHeartbeats). heartbeatTimeout without one calls
// AttemptDisconnectedFunc exactly once (disconnected latches until a
// heartbeat resets it - no repeat firing on an already-flagged row); the next
// heartbeat after that calls AttemptReconnectedFunc.
func (g *Gateway) pumpAttempt(ctx context.Context, c *websocket.Conn, msgs <-chan string, attemptID string, hb <-chan struct{}) {
	ping := time.NewTicker(pingInterval)
	defer ping.Stop()
	lastSeen := time.Now()
	hbTimer := time.NewTimer(heartbeatTimeout)
	defer hbTimer.Stop()
	disconnected := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		case <-hb:
			lastSeen = time.Now()
			if !hbTimer.Stop() {
				select {
				case <-hbTimer.C:
				default:
				}
			}
			hbTimer.Reset(heartbeatTimeout)
			if disconnected {
				disconnected = false
				if g.attemptReconnected != nil {
					go g.attemptReconnected(g.baseCtx, attemptID)
				}
			}
		case <-hbTimer.C:
			disconnected = true
			if g.attemptDisconnected != nil {
				go g.attemptDisconnected(g.baseCtx, attemptID, lastSeen)
			}
		case payload, ok := <-msgs:
			if !ok {
				return
			}
			var env Event
			if err := json.Unmarshal([]byte(payload), &env); err != nil {
				g.log.Warn("decode attempt channel envelope", "attempt_id", attemptID, "err", err)
				continue
			}
			isKick := env.Type == eventKicked && env.AttemptID == attemptID
			isBanner := strings.HasPrefix(env.Type, "quiz.")
			if !isKick && !isBanner {
				continue
			}
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.Write(wctx, websocket.MessageText, []byte(payload))
			cancel()
			if err != nil {
				return
			}
			if isKick {
				return
			}
		}
	}
}

// handleNotify authorizes the subscribe against the URL id matching the
// caller's own id (docs/04-api.md section 6: "the user themselves" - no
// owner lookup needed, unlike handleMonitor/handleAttempt, since the
// resource the channel names is the actor), then streams every notification
// published to that user's channel until the client disconnects or the
// process shuts down. There is no "not found" ambiguity to preserve here (a
// user always knows their own id), so a mismatched id is a plain 403.
func (g *Gateway) handleNotify(w http.ResponseWriter, r *http.Request) {
	actor, _ := authusers.ActorFrom(r.Context())
	userID := chi.URLParam(r, "id")
	if !uuidShape.MatchString(userID) || actor.ID != userID {
		httpapi.WriteError(w, http.StatusForbidden, httpapi.CodeForbidden, "you may only subscribe to your own notifications")
		return
	}

	// Subscribe before upgrading, same reasoning as handleMonitor/handleAttempt:
	// a Redis failure here is a clean 500, not a socket that connects and then
	// silently delivers nothing.
	connCtx, cancel := context.WithCancel(g.baseCtx)
	defer cancel()
	sub, err := g.sub.Subscribe(connCtx, notifyChannel(userID))
	if err != nil {
		g.log.Error("subscribe notify channel", "user_id", userID, "err", err)
		httpapi.WriteError(w, http.StatusInternalServerError, "INTERNAL", "internal error")
		return
	}
	defer sub.Close()

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: g.origins})
	if err != nil {
		// Accept has already written the failure response.
		return
	}
	defer c.CloseNow()
	g.metrics.IncWSConnections(g.baseCtx)
	defer g.metrics.DecWSConnections(g.baseCtx)

	ctx := c.CloseRead(connCtx)
	g.pump(ctx, c, sub.Messages())
}

// requireStaff is the coarse teacher-or-admin role gate on the monitor
// surface (docs/05 section 3), mirroring quiz.requireStaff: a student is 403
// here, and the owner-vs-admin resource decision (404 for a non-owning
// teacher) is left to the handler's Can() check.
func requireStaff(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u, ok := authusers.ActorFrom(r.Context()); !ok ||
			(u.Role != "teacher" && u.Role != "admin") {
			httpapi.WriteError(w, http.StatusForbidden, httpapi.CodeForbidden,
				"live monitoring is for teachers and admins")
			return
		}
		next.ServeHTTP(w, r)
	})
}

var uuidShape = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
