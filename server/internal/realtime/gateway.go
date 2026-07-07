package realtime

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"github.com/coder/websocket"
	"github.com/go-chi/chi/v5"

	"macquiz/server/internal/authusers"
	"macquiz/server/internal/httpapi"
)

// This file owns hop 3 of the docs/05 section 1 pipeline: the gateway,
// subscribed to quiz:{id}:events, fans each committed event out to every
// authorized WebSocket on the teacher/admin monitor channel. It relays, it
// never decides (doc.go): auth is one Can() check at subscribe (docs/05
// section 3), and every payload is the source-of-truth envelope the attempt
// module already persisted and published.
//
// This brick lands the quiz:{id}:monitor channel only; the attempt:{id} and
// user:{id}:notify channels, heartbeat/disconnect tracking, and the current
// question wiring are separate bricks that layer on top.

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

// Gateway is the WebSocket monitor endpoint. It holds no quiz state: the
// snapshot the dashboard reconciles against comes from GET /quizzes/:id/live,
// and the gateway only streams the deltas published after it.
type Gateway struct {
	sub     Subscriber
	auth    *authusers.Service
	owner   QuizOwnerFunc
	origins []string
	log     *slog.Logger

	// baseCtx is the socket lifetime, derived from the serve process context
	// so every open socket closes on shutdown. Critically it is NOT the
	// request context: the root router's middleware.Timeout(30s) cancels the
	// request context at 30 s, which would guillotine every monitor socket.
	baseCtx context.Context
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
