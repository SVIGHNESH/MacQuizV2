package realtime

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
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
// events. The user:{id}:notify channel, heartbeat/disconnect tracking, and
// the current question wiring are separate bricks that still layer on top.

const (
	// pingInterval keeps the socket (and any intermediate proxy) alive across
	// the long idle gaps between events on a quiet quiz.
	pingInterval = 30 * time.Second
	// writeTimeout bounds a single frame write/ping so one stuck client can
	// never wedge its pump goroutine.
	writeTimeout = 10 * time.Second
)

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

// Gateway is the WebSocket monitor endpoint. It holds no quiz state: the
// snapshot the dashboard reconciles against comes from GET /quizzes/:id/live,
// and the gateway only streams the deltas published after it.
type Gateway struct {
	sub          Subscriber
	auth         *authusers.Service
	owner        QuizOwnerFunc
	attemptOwner AttemptOwnerFunc
	origins      []string
	log          *slog.Logger
	metrics      *telemetry.Metrics

	// baseCtx is the socket lifetime, derived from the serve process context
	// so every open socket closes on shutdown. Critically it is NOT the
	// request context: the root router's middleware.Timeout(30s) cancels the
	// request context at 30 s, which would guillotine every monitor socket.
	baseCtx context.Context
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

// NewGateway wires the gateway. ctx is the serve-process lifetime; when it is
// canceled (SIGTERM) every open socket's pump returns, so graceful shutdown
// does not block on the router's Timeout grace. origins is the allowed set of
// WebSocket Origin patterns (coder/websocket rejects cross-origin by default);
// an empty list means same-origin only.
func NewGateway(ctx context.Context, sub Subscriber, auth *authusers.Service, owner QuizOwnerFunc, origins []string, log *slog.Logger) *Gateway {
	return &Gateway{sub: sub, auth: auth, owner: owner, origins: origins, log: log, baseCtx: ctx}
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

	ctx := c.CloseRead(connCtx)
	g.pumpAttempt(ctx, c, sub.Messages(), attemptID)
}

// pumpAttempt relays only what the docs/05 section 3 attempt:{id} channel
// promises: the kick lockout message for this attempt, and any quiz-wide
// banner event (a "quiz." prefix, e.g. the still-unbuilt quiz.extended/
// quiz.closed). It deliberately drops every other event on the shared
// quiz:{quiz_id}:events stream - attempt.progress/submitted/graded/violation
// are for the teacher's monitor, not an echo back to the student who already
// knows their own state. It returns after delivering a kick for this
// attempt: docs/06 section 4 step 4, "delivers a lockout message... then
// force-closes it" - a kicked attempt has nothing further to receive.
func (g *Gateway) pumpAttempt(ctx context.Context, c *websocket.Conn, msgs <-chan string, attemptID string) {
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
