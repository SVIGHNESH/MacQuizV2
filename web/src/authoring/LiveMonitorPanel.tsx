import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../api/client'
import { formatRemaining } from '../player/model'
import type { components } from '../api/schema'

type LiveRosterRow = components['schemas']['LiveRosterRow']

const STATE_LABEL: Record<LiveRosterRow['state'], string> = {
  not_started: 'Not started',
  in_progress: 'In progress',
  disconnected: 'Disconnected',
  submitted: 'Submitted',
  kicked: 'Kicked',
}

// docs/05 section 5: the dashboard falls back to polling the snapshot every
// 10 s whenever the WebSocket cannot connect (restrictive school network).
const POLL_INTERVAL_MS = 10_000
const RECONNECT_DELAY_MS = 3_000

function monitorSocketURL(quizId: string): string {
  const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${protocol}//${location.host}/ws/quizzes/${quizId}/monitor`
}

// The docs/05 section 1 envelope every event arrives in: a type tag, the
// attempt the delta applies to, and the typed payload as raw JSON.
interface RealtimeEvent {
  type: string
  attempt_id: string
  payload: unknown
}

/**
 * The Milestone 5 teacher live dashboard (docs/05, docs/12 Milestone 5): the
 * roster snapshot from GET /quizzes/:id/live, kept current by streamed
 * WebSocket deltas with a polling fallback, plus the kick/readmit escalation.
 */
export default function LiveMonitorPanel({ quizId }: { quizId: string }) {
  const [roster, setRoster] = useState<LiveRosterRow[] | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [connected, setConnected] = useState(false)
  const [actionError, setActionError] = useState<string | null>(null)
  const [busyAttemptId, setBusyAttemptId] = useState<string | null>(null)
  // Server-minus-client clock estimate, the same cosmetic-countdown technique
  // AttemptPlayer uses, so a skewed local clock never misreports time left.
  const clockOffset = useRef(0)
  const [, forceTick] = useState(0)

  const loadSnapshot = useCallback(async () => {
    const result = await api
      .GET('/api/v1/quizzes/{id}/live', { params: { path: { id: quizId } } })
      .catch(() => null)
    if (!result?.data) {
      setLoadError(result?.error?.message ?? 'Could not load the live roster.')
      return
    }
    setLoadError(null)
    clockOffset.current = Date.parse(result.data.server_time) - Date.now()
    setRoster(result.data.roster)
  }, [quizId])

  useEffect(() => {
    void loadSnapshot()
  }, [loadSnapshot])

  // A once-per-second re-render to keep the "time left" cells ticking without
  // depending on any per-row timer or event.
  useEffect(() => {
    const timer = setInterval(() => forceTick((n) => n + 1), 1000)
    return () => clearInterval(timer)
  }, [])

  const applyEvent = useCallback((msg: RealtimeEvent) => {
    setRoster((prev) => {
      if (!prev) return prev
      switch (msg.type) {
        case 'attempt.progress': {
          const p = msg.payload as { answered_count: number; current_question: number | null }
          return prev.map((row) =>
            row.attempt_id === msg.attempt_id
              ? { ...row, answered_count: p.answered_count, current_question: p.current_question }
              : row,
          )
        }
        case 'attempt.violation': {
          const p = msg.payload as { violation_count: number }
          return prev.map((row) =>
            row.attempt_id === msg.attempt_id
              ? { ...row, violation_count: p.violation_count }
              : row,
          )
        }
        case 'attempt.disconnected': {
          return prev.map((row) =>
            row.attempt_id === msg.attempt_id && row.state === 'in_progress'
              ? { ...row, state: 'disconnected' }
              : row,
          )
        }
        case 'attempt.reconnected': {
          return prev.map((row) =>
            row.attempt_id === msg.attempt_id && row.state === 'disconnected'
              ? { ...row, state: 'in_progress' }
              : row,
          )
        }
        case 'attempt.kicked': {
          return prev.map((row) =>
            row.attempt_id === msg.attempt_id
              ? { ...row, state: 'kicked', status: 'kicked', submit_kind: 'kicked' }
              : row,
          )
        }
        case 'attempt.submitted': {
          const p = msg.payload as {
            submit_kind: LiveRosterRow['submit_kind']
            answered_count: number
          }
          return prev.map((row) =>
            row.attempt_id === msg.attempt_id
              ? {
                  ...row,
                  state: row.state === 'kicked' ? 'kicked' : 'submitted',
                  status: 'submitted',
                  submit_kind: row.state === 'kicked' ? row.submit_kind : p.submit_kind,
                  answered_count: p.answered_count,
                }
              : row,
          )
        }
        case 'attempt.graded': {
          const p = msg.payload as { score: number }
          return prev.map((row) =>
            row.attempt_id === msg.attempt_id ? { ...row, score: p.score } : row,
          )
        }
        default:
          return prev
      }
    })
  }, [])

  // WebSocket deltas with a polling fallback (docs/05 section 5). Connection
  // state and the reconnect loop live entirely in refs/timers, not React
  // state, so a flaky socket cannot leak overlapping sockets or timers.
  useEffect(() => {
    let cancelled = false
    let socket: WebSocket | null = null
    let pollTimer: ReturnType<typeof setInterval> | null = null
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null

    const stopPolling = () => {
      if (pollTimer) clearInterval(pollTimer)
      pollTimer = null
    }
    const startPolling = () => {
      if (pollTimer) return
      pollTimer = setInterval(() => void loadSnapshot(), POLL_INTERVAL_MS)
    }

    const connect = () => {
      if (cancelled) return
      socket = new WebSocket(monitorSocketURL(quizId))
      socket.onopen = () => {
        if (cancelled) return
        setConnected(true)
        stopPolling()
        // Reconcile any delta missed between the last connection and this one.
        void loadSnapshot()
      }
      socket.onmessage = (event) => {
        if (typeof event.data !== 'string') return
        let msg: RealtimeEvent
        try {
          msg = JSON.parse(event.data) as RealtimeEvent
        } catch {
          // A malformed frame is dropped, not fatal - the next snapshot
          // reconciles it.
          return
        }
        // attempt.started carries no question_count/max_score (those come
        // from the version join the snapshot query does) and it fires once
        // per attempt, so a full re-fetch is simplest and always correct.
        if (msg.type === 'attempt.started') {
          void loadSnapshot()
          return
        }
        applyEvent(msg)
      }
      socket.onclose = () => {
        if (cancelled) return
        setConnected(false)
        startPolling()
        reconnectTimer = setTimeout(connect, RECONNECT_DELAY_MS)
      }
      socket.onerror = () => {
        socket?.close()
      }
    }
    connect()

    return () => {
      cancelled = true
      stopPolling()
      if (reconnectTimer) clearTimeout(reconnectTimer)
      socket?.close()
    }
  }, [quizId, loadSnapshot, applyEvent])

  const escalate = async (attemptId: string, action: 'kick' | 'readmit') => {
    const reason = window.prompt(
      action === 'kick'
        ? 'Reason for removing this student?'
        : 'Reason for readmitting this student?',
    )
    if (!reason || !reason.trim()) return
    setBusyAttemptId(attemptId)
    setActionError(null)
    const result = await api
      .POST(`/api/v1/attempts/{id}/${action}`, {
        params: { path: { id: attemptId } },
        body: { reason: reason.trim() },
      })
      .catch(() => null)
    setBusyAttemptId(null)
    if (!result?.data) {
      setActionError(
        result?.error?.message ??
          `Could not ${action === 'kick' ? 'remove' : 'readmit'} the student.`,
      )
      return
    }
    void loadSnapshot()
  }

  if (loadError && !roster) {
    return <p className="form-error">{loadError}</p>
  }
  if (!roster) {
    return (
      <p className="boot-note" role="status">
        Loading live roster…
      </p>
    )
  }

  return (
    <section className="panel live-monitor-panel">
      <div className="stats-panel-head">
        <span className="card-title">Live roster</span>
        <span className={`save-badge ${connected ? 'save-badge-ok' : 'save-badge-bad'}`}>
          {connected ? 'Live' : 'Reconnecting…'}
        </span>
      </div>
      {actionError && <p className="form-error">{actionError}</p>}
      <div className="table-panel">
        <div className="quiz-table live-roster-table" role="table" aria-label="Live roster">
          <div className="qt-head" role="row">
            <span>Student</span>
            <span>State</span>
            <span>Progress</span>
            <span>Violations</span>
            <span>Time left</span>
            <span></span>
          </div>
          {roster.map((row) => {
            const remaining =
              row.deadline_at && (row.state === 'in_progress' || row.state === 'disconnected')
                ? Date.parse(row.deadline_at) - (Date.now() + clockOffset.current)
                : null
            return (
              <div className="qt-row" role="row" key={row.student_id}>
                <span className="live-roster-name" title={row.email}>
                  {row.full_name}
                </span>
                <span className={`chip chip-roster-${row.state}`}>
                  {STATE_LABEL[row.state]}
                </span>
                <span className="tabular">
                  {row.answered_count !== null && row.question_count !== null
                    ? `${row.current_question !== null ? `Q${row.current_question} · ` : ''}${row.answered_count} / ${row.question_count}`
                    : '—'}
                </span>
                <span className="tabular">{row.violation_count ?? 0}</span>
                <span className="tabular">
                  {remaining === null ? '—' : formatRemaining(remaining)}
                </span>
                <span className="qt-actions">
                  {(row.state === 'in_progress' || row.state === 'disconnected') && row.attempt_id && (
                    <button
                      className="button button-quiet-danger button-small"
                      type="button"
                      disabled={busyAttemptId === row.attempt_id}
                      onClick={() => void escalate(row.attempt_id!, 'kick')}
                    >
                      Kick
                    </button>
                  )}
                  {row.state === 'kicked' && row.attempt_id && (
                    <button
                      className="button button-quiet button-small"
                      type="button"
                      disabled={busyAttemptId === row.attempt_id}
                      onClick={() => void escalate(row.attempt_id!, 'readmit')}
                    >
                      Readmit
                    </button>
                  )}
                </span>
              </div>
            )
          })}
        </div>
      </div>
    </section>
  )
}
