import { useEffect, useRef } from 'react'
import { AUTH_REVOKED_CLOSE_CODE } from './wsCloseCodes'

/**
 * docs/05 section 3's user:{id}:notify envelope: the same shape as the
 * attempt:{id} channel's RealtimeEvent (AttemptPlayer.tsx) minus attempt_id -
 * a notification is addressed to a person, never scoped to one roster row.
 */
export interface NotifyEvent {
  type: string
  payload: unknown
}

const RECONNECT_MS = 3_000

function notifySocketURL(userId: string): string {
  const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${protocol}//${location.host}/ws/users/${userId}/notify`
}

/**
 * Holds this user's notify socket open for the whole signed-in session and
 * hands every event to onEvent. Both workspaces need it and for the same
 * reason: the things it carries (a quiz assigned, a guardrail tripped) happen
 * while the recipient is looking at something else entirely, so the socket
 * cannot belong to any one screen.
 *
 * onEvent is held in a ref, so a caller may pass a fresh closure on every
 * render without tearing the socket down and rebuilding it - the trap a plain
 * dependency would set, since the handler almost always closes over state it
 * wants to update. The socket's whole lifecycle stays outside React state,
 * mirroring AttemptPlayer.tsx's attempt:{id} socket: it reconnects on drop and
 * closes only when the user changes or the shell unmounts.
 */
export function useNotifySocket(userId: string, onEvent: (event: NotifyEvent) => void): void {
  const handler = useRef(onEvent)
  useEffect(() => {
    handler.current = onEvent
  }, [onEvent])

  useEffect(() => {
    let cancelled = false
    let socket: WebSocket | null = null
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null

    const connect = () => {
      if (cancelled) return
      socket = new WebSocket(notifySocketURL(userId))
      socket.onmessage = (event) => {
        if (typeof event.data !== 'string') return
        let msg: NotifyEvent
        try {
          msg = JSON.parse(event.data) as NotifyEvent
        } catch {
          return
        }
        handler.current(msg)
      }
      socket.onclose = (event) => {
        if (cancelled) return
        // docs/05 section 3's revalidation closed us because the account is no
        // longer active. A reconnect would fail the same check every 3s until
        // the tab is closed; the next REST call the user makes will bounce them
        // to the login screen anyway.
        if (event.code === AUTH_REVOKED_CLOSE_CODE) return
        reconnectTimer = setTimeout(connect, RECONNECT_MS)
      }
      socket.onerror = () => {
        socket?.close()
      }
    }
    connect()

    return () => {
      cancelled = true
      if (reconnectTimer) clearTimeout(reconnectTimer)
      socket?.close()
    }
  }, [userId])
}
