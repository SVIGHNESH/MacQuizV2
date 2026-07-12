import { useCallback, useEffect, useRef, useState } from 'react'
import { api } from '../api/client'
import { formatRemaining, formatWhen } from '../player/model'
import DestructiveConfirmModal from '../components/DestructiveConfirmModal'
import AudienceEditor from './AudienceEditor'
import type { components } from '../api/schema'

type LiveRosterRow = components['schemas']['LiveRosterRow']
type Quiz = components['schemas']['Quiz']
type ViolationTally = components['schemas']['ViolationTally']

// docs/06 section 3's three guardrails, named the way a teacher reading the
// hover would describe what the student did, not the way the wire names it.
const VIOLATION_LABEL: Record<ViolationTally['type'], string> = {
  fullscreen: 'Left fullscreen',
  focus: 'Left the tab',
  clipboard: 'Copy, paste, or right-click',
}

/**
 * The roster badge's "types on hover" text (docs/05 section 2, docs/06
 * section 3: "an amber badge with count and types on hover").
 *
 * The tallies count every logged attempt.violation; violation_count is the
 * narrower ladder tally, which a warn-policy or clipboard report never
 * advances. When they differ, the difference is the point - the teacher is
 * looking at evidence that exists but does not push the student up the
 * ladder - so the summary says so rather than showing two numbers that look
 * like they contradict each other.
 */
function violationSummary(tallies: ViolationTally[], counted: number): string {
  const parts = tallies.map((t) => {
    const times = `${VIOLATION_LABEL[t.type] ?? t.type} ×${t.count}`
    // Focus loss is the only guardrail that measures a span (docs/06: the
    // teacher sees "left the tab for 40 s"); the rest report an instant.
    return t.total_duration_ms === null
      ? times
      : `${times} (${Math.round(t.total_duration_ms / 1000)}s)`
  })
  const total = tallies.reduce((n, t) => n + t.count, 0)
  const summary = `Logged: ${parts.join(', ')}.`
  return total === counted
    ? summary
    : `${summary} ${counted} of ${total} count toward the limit.`
}

// The per-type breakdown is never accumulated from deltas, only re-read from
// the snapshot (see the attempt.violation case below). This coalesces that
// re-read on its trailing edge, so a burst of violations costs one GET and
// that GET is issued strictly after the last of them committed. 2 s mirrors
// docs/05 section 5's coalescing window for attempt.progress.
const VIOLATION_REFETCH_MS = 2_000

// docs/06: the teacher's "extend a live quiz" control moves ends_at later. The
// design doc draws it as a fixed "+5 min" step, the smallest useful nudge.
const EXTEND_STEP_MS = 5 * 60_000

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
export default function LiveMonitorPanel({
  quizId,
  quizTitle,
  onQuizUpdate,
  onNotice,
}: {
  quizId: string
  quizTitle: string
  /** Lets the parent editor react to a force-close/extend (status/ends_at). */
  onQuizUpdate?: (quiz: Quiz) => void
  /**
   * Raises a docs/05 section 2 window banner to the editor shell. It is not
   * rendered here because a quiz.closed unmounts this panel - the editor drops
   * the live tab the moment the status flips - so a banner this component owned
   * would disappear in the same frame it appeared, exactly when the teacher
   * most needs to be told why the screen changed.
   */
  onNotice?: (text: string) => void
}) {
  const [roster, setRoster] = useState<LiveRosterRow[] | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [connected, setConnected] = useState(false)
  const [actionError, setActionError] = useState<string | null>(null)
  const [busyAttemptId, setBusyAttemptId] = useState<string | null>(null)
  const [showAudienceEditor, setShowAudienceEditor] = useState(false)
  const [quizEndsAt, setQuizEndsAt] = useState<string | null>(null)
  const [extending, setExtending] = useState(false)
  const [pendingForceClose, setPendingForceClose] = useState(false)
  const [forceClosing, setForceClosing] = useState(false)
  const [pendingEscalation, setPendingEscalation] = useState<{
    attemptId: string
    studentName: string
    action: 'kick' | 'readmit'
  } | null>(null)
  // Server-minus-client clock estimate, the same cosmetic-countdown technique
  // AttemptPlayer uses, so a skewed local clock never misreports time left.
  const clockOffset = useRef(0)
  const [, forceTick] = useState(0)
  // Snapshots are requested from several places (mount, the polling fallback,
  // attempt.started, a violation burst), so responses can land out of order.
  // Only the newest request issued may write the roster; an older one that
  // overtakes it would rewind rows past state the dashboard already applied.
  const snapshotSeq = useRef(0)
  const violationRefetch = useRef<ReturnType<typeof setTimeout> | null>(null)

  const loadSnapshot = useCallback(async () => {
    const seq = ++snapshotSeq.current
    const result = await api
      .GET('/api/v1/quizzes/{id}/live', { params: { path: { id: quizId } } })
      .catch(() => null)
    if (seq !== snapshotSeq.current) return
    if (!result?.data) {
      setLoadError(result?.error?.message ?? 'Could not load the live roster.')
      return
    }
    setLoadError(null)
    clockOffset.current = Date.parse(result.data.server_time) - Date.now()
    setRoster(result.data.roster)
    setQuizEndsAt(result.data.quiz.ends_at)
  }, [quizId])

  useEffect(() => {
    void loadSnapshot()
  }, [loadSnapshot])

  useEffect(
    () => () => {
      if (violationRefetch.current) clearTimeout(violationRefetch.current)
    },
    [],
  )

  // A once-per-second re-render to keep the "time left" cells ticking without
  // depending on any per-row timer or event.
  useEffect(() => {
    const timer = setInterval(() => forceTick((n) => n + 1), 1000)
    return () => clearInterval(timer)
  }, [])

  // A close this panel did not initiate - the scheduler's sweep reaching
  // ends_at, or a force-close from the teacher's other window - arrives only as
  // an event, so the editor is still holding a live quiz. Re-read it and hand
  // it up, which is the same transition the force-close REST response drives.
  const refreshQuiz = useCallback(async () => {
    const result = await api
      .GET('/api/v1/quizzes/{id}', { params: { path: { id: quizId } } })
      .catch(() => null)
    if (result?.data) onQuizUpdate?.(result.data.quiz)
  }, [quizId, onQuizUpdate])

  const applyEvent = useCallback((msg: RealtimeEvent) => {
    // Scheduled outside the updater below, which must stay pure: React invokes
    // it twice under StrictMode.
    if (msg.type === 'attempt.violation') {
      if (violationRefetch.current) clearTimeout(violationRefetch.current)
      violationRefetch.current = setTimeout(
        () => void loadSnapshot(),
        VIOLATION_REFETCH_MS,
      )
    }
    // docs/05 section 2: a window change is a "banner to teacher and all
    // in-progress students". The student half is AttemptPlayer's; this is the
    // teacher's. Both fire for the teacher's own extend/force-close too -
    // suppressing the echo would buy nothing and cost a self-only blind spot.
    if (msg.type === 'quiz.extended') {
      const p = msg.payload as { ends_at: string }
      setQuizEndsAt(p.ends_at)
      onNotice?.(`Quiz extended to ${formatWhen(p.ends_at)}.`)
      // The server clamps every in-progress deadline_at into the new window, so
      // the roster's "time left" column is stale until it is re-read.
      void loadSnapshot()
      return
    }
    if (msg.type === 'quiz.closed') {
      onNotice?.('This quiz has been closed.')
      void refreshQuiz()
      return
    }
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
          // violation_count is absolute, so the badge moves the instant the
          // delta lands. The per-type breakdown behind it is deliberately NOT
          // folded in here: the delta carries one violation, not the tally,
          // and a client that adds them up cannot be made exactly-once. A
          // delta for an attempt this roster has not learned of yet (the row
          // stays not_started until attempt.started's re-fetch returns) matches
          // no row and is dropped, and a re-fetch landing mid-burst would
          // overwrite whatever had accumulated. Both are silent, permanent
          // losses in a counter nothing later corrects. So SQL stays the only
          // place a breakdown is computed; the re-read is scheduled above.
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
  }, [loadSnapshot, onNotice, refreshQuiz])

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

  // docs/06 + api/openapi extendQuiz: push ends_at EXTEND_STEP_MS later. The
  // server clamps each in-progress attempt's deadline to the new window, so no
  // student loses time; a stale ends_at answers 409/422, surfaced as an error.
  const extend = async () => {
    if (!quizEndsAt) return
    setExtending(true)
    setActionError(null)
    const newEndsAt = new Date(Date.parse(quizEndsAt) + EXTEND_STEP_MS).toISOString()
    const result = await api
      .POST('/api/v1/quizzes/{id}/extend', {
        params: { path: { id: quizId } },
        body: { ends_at: newEndsAt },
      })
      .catch(() => null)
    setExtending(false)
    if (!result?.data) {
      setActionError(result?.error?.message ?? 'Could not extend the quiz.')
      return
    }
    setQuizEndsAt(result.data.quiz.ends_at)
    onQuizUpdate?.(result.data.quiz)
    void loadSnapshot()
  }

  // api/openapi forceCloseQuiz: end the live quiz now. Irreversible - every
  // open attempt is force-submitted and graded - so it goes through the
  // two-step confirm, without a reason (the endpoint records none).
  const forceClose = async () => {
    setForceClosing(true)
    setActionError(null)
    const result = await api
      .POST('/api/v1/quizzes/{id}/close', { params: { path: { id: quizId } } })
      .catch(() => null)
    setForceClosing(false)
    if (!result?.data) {
      setActionError(result?.error?.message ?? 'Could not force-close the quiz.')
      return
    }
    setPendingForceClose(false)
    // Flips the parent to Closed, which unmounts this panel in favor of the
    // results/analytics view - so this is the last state we touch here.
    onQuizUpdate?.(result.data.quiz)
  }

  const escalate = async (reason: string) => {
    if (!pendingEscalation) return
    const { attemptId, action } = pendingEscalation
    setBusyAttemptId(attemptId)
    setActionError(null)
    const result = await api
      .POST(`/api/v1/attempts/{id}/${action}`, {
        params: { path: { id: attemptId } },
        body: { reason },
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
    setPendingEscalation(null)
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
        <button
          className="button button-quiet button-small"
          type="button"
          onClick={() => setShowAudienceEditor((v) => !v)}
        >
          {showAudienceEditor ? 'Hide audience' : 'Manage audience'}
        </button>
        <button
          className="button button-quiet button-small"
          type="button"
          id="extend-quiz-button"
          disabled={extending || !quizEndsAt}
          onClick={() => void extend()}
        >
          {extending ? 'Extending…' : 'Extend +5 min'}
        </button>
        <button
          className="button button-quiet-danger button-small"
          type="button"
          id="force-close-button"
          onClick={() => setPendingForceClose(true)}
        >
          Force-close
        </button>
      </div>
      {showAudienceEditor && <AudienceEditor quizId={quizId} live />}
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
                <span className="tabular">
                  {row.violations.length === 0 ? (
                    (row.violation_count ?? 0)
                  ) : (
                    <>
                      <span
                        className="violation-badge"
                        data-testid="violation-badge"
                        title={violationSummary(row.violations, row.violation_count ?? 0)}
                      >
                        {row.violation_count ?? 0}
                      </span>
                      {/* title= is hover-only and this cell is not focusable,
                          so the same sentence is spoken from the row itself. */}
                      <span className="visually-hidden" data-testid="violation-spoken">
                        {' '}
                        {violationSummary(row.violations, row.violation_count ?? 0)}
                      </span>
                    </>
                  )}
                </span>
                <span className="tabular">
                  {remaining === null ? '—' : formatRemaining(remaining)}
                </span>
                <span className="qt-actions">
                  {(row.state === 'in_progress' || row.state === 'disconnected') && row.attempt_id && (
                    <button
                      className="button button-quiet-danger button-small"
                      type="button"
                      disabled={busyAttemptId === row.attempt_id}
                      onClick={() =>
                        setPendingEscalation({
                          attemptId: row.attempt_id!,
                          studentName: row.full_name,
                          action: 'kick',
                        })
                      }
                    >
                      Kick
                    </button>
                  )}
                  {row.state === 'kicked' && row.attempt_id && (
                    <button
                      className="button button-quiet button-small"
                      type="button"
                      disabled={busyAttemptId === row.attempt_id}
                      onClick={() =>
                        setPendingEscalation({
                          attemptId: row.attempt_id!,
                          studentName: row.full_name,
                          action: 'readmit',
                        })
                      }
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

      {pendingForceClose && (
        <DestructiveConfirmModal
          title="Force-close this quiz?"
          subtitle={quizTitle}
          consequence="This ends the quiz for everyone now. Every in-progress attempt is submitted as-is and graded immediately. It cannot be reopened."
          confirmLabel="Force-close quiz"
          busy={forceClosing}
          error={actionError}
          onCancel={() => setPendingForceClose(false)}
          onConfirm={() => void forceClose()}
        />
      )}

      {pendingEscalation && (
        <DestructiveConfirmModal
          title={pendingEscalation.action === 'kick' ? 'Confirm removal' : 'Confirm readmission'}
          subtitle={`${pendingEscalation.studentName} · ${quizTitle}`}
          reasonLabel={
            pendingEscalation.action === 'kick'
              ? 'Reason for removing this student'
              : 'Reason for readmitting this student'
          }
          consequence={
            pendingEscalation.action === 'kick'
              ? 'This ends their attempt immediately. Work already saved is kept for grading.'
              : // docs/06 section 4: re-admission is a new attempt, not a
                // resurrection - the kicked attempt stays in the record.
                'This grants one fresh attempt with whatever time remains. Their removed attempt is kept in the record.'
          }
          confirmLabel={pendingEscalation.action === 'kick' ? 'Remove student' : 'Readmit student'}
          busy={busyAttemptId === pendingEscalation.attemptId}
          error={actionError}
          onCancel={() => setPendingEscalation(null)}
          onConfirm={(reason) => void escalate(reason)}
        />
      )}
    </section>
  )
}
