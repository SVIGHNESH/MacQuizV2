import createClient from 'openapi-fetch'
import type { paths } from './schema'

/**
 * Fired when a 401 survived a refresh-and-retry cycle: the session is truly
 * gone and the UI should drop to signed-out (AuthProvider listens for this).
 */
export const SESSION_EXPIRED_EVENT = 'mq:session-expired'

// The access cookie lives 15 minutes (server AccessTokenTTL), far shorter
// than a quiz. Every API call therefore goes through authFetch below, which
// on a 401 refreshes the session once and replays the request, so an expired
// access token mid-attempt is invisible instead of an "authentication
// failed" wall (the top complaint in feedback/2026-08-18).

// Refreshing is single-flight per tab (shared promise) and serialized across
// tabs (Web Lock): the refresh token rotates on use, so two concurrent
// refreshes would trip the server's theft detector and revoke the session.
let refreshInFlight: Promise<boolean> | null = null

async function doRefresh(force: boolean): Promise<boolean> {
  // Another tab may have refreshed while we waited on the lock; if the
  // access cookie already works again, don't burn the rotation. The
  // keep-alive timer forces past this probe: it runs while the token is
  // still valid precisely to renew it before it expires.
  if (!force) {
    const me = await fetch('/api/v1/auth/me', { credentials: 'same-origin' })
    if (me.ok) return true
  }
  const r = await fetch('/api/v1/auth/refresh', {
    method: 'POST',
    credentials: 'same-origin',
  })
  // A raced refresh (another tab won by milliseconds) also returns 401 but
  // leaves the winner's cookies in the jar, so the caller's retry still
  // works; report success for anything that isn't a hard failure signal.
  return r.ok || r.status === 401
}

function refreshSession(force = false): Promise<boolean> {
  if (!refreshInFlight) {
    const run: Promise<boolean> =
      typeof navigator !== 'undefined' && navigator.locks
        ? navigator.locks
            .request('mq-session-refresh', () => doRefresh(force))
            .then((ok) => Promise.resolve(ok))
        : doRefresh(force)
    const flight = run
      .catch(() => false)
      .finally(() => {
        refreshInFlight = null
      })
    refreshInFlight = flight
    return flight
  }
  return refreshInFlight
}

/**
 * Rotate the session cookies now, even though the access token still works;
 * used by the proactive keep-alive timer so the WebSocket handshake (which
 * bypasses authFetch) never presents an expired cookie.
 */
export function keepSessionFresh(): Promise<boolean> {
  return refreshSession(true)
}

const AUTH_PATH_PREFIX = '/api/v1/auth/'

async function authFetch(request: Request): Promise<Response> {
  const path = new URL(request.url, location.origin).pathname
  // Auth endpoints manage the session themselves; retrying them through the
  // refresh path would recurse (and a login 401 is a real wrong password).
  if (path.startsWith(AUTH_PATH_PREFIX)) return fetch(request)

  // Clone before the first send: a consumed request body cannot be replayed.
  const retryCopy = request.clone()
  const response = await fetch(request)
  if (response.status !== 401) return response

  await refreshSession()
  const retried = await fetch(retryCopy)
  if (retried.status === 401) {
    window.dispatchEvent(new Event(SESSION_EXPIRED_EVENT))
  }
  return retried
}

// Typed client over api/openapi.yaml. Same-origin: the Vite dev proxy
// forwards to the Go API in dev, Caddy does in production, so httpOnly
// auth cookies (Milestone 1) work without CORS.
export const api = createClient<paths>({ baseUrl: '/', fetch: authFetch })
