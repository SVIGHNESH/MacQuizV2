/**
 * The private-use close codes (RFC 6455 section 7.4.2) the gateway closes a
 * socket with, mirroring server/internal/realtime/gateway.go. Both mean "do
 * not reconnect": every socket in this app otherwise retries on close, and a
 * retry after either of these would either boot the other device right back or
 * be refused at the handshake, three seconds at a time, forever.
 */

/**
 * docs/08 section 1 "single active session": the same attempt was opened in
 * another window or device, so this socket lost the slot.
 */
export const SESSION_REPLACED_CLOSE_CODE = 4001

/**
 * docs/05 section 3's periodic revalidation: the account was disabled or
 * deleted, or the resource is no longer this user's to watch. Reconnecting is
 * pointless - the handshake re-runs the same check that just failed.
 */
export const AUTH_REVOKED_CLOSE_CODE = 4003
