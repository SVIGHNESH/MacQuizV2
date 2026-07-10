import { useEffect, useReducer, useRef, useState } from 'react'
import { api } from '../api/client'
import { useToast } from '../toast/context'
import {
  coerceResponse,
  formatRemaining,
  formatWhen,
  isAnswered,
  type AttemptDetail,
  type AttemptQuestion,
  type ResponseValue,
} from './model'

export type PlayerEntry =
  | { kind: 'start'; quizId: string }
  | { kind: 'resume'; attemptId: string }

const AUTOSAVE_DELAY_MS = 600

// docs/11 section 5 "calm under pressure": urgency is expressed once, and the
// design's St2 frame fixes the moment - the timer goes red-tint under 2:00.
const TIMER_URGENT_MS = 120_000

type GuardrailType = 'fullscreen' | 'focus' | 'clipboard'

const QUESTION_TYPE_LABEL: Record<AttemptQuestion['type'], string> = {
  single: 'Single choice',
  multi: 'Multiple choice',
  truefalse: 'True or false',
  short: 'Short answer',
}

// docs/06 section 3: a warn-class report (a guardrail set to warn, or the
// clipboard guardrail, which is always logged-not-counted) still needs
// student-facing copy even though it never touches violation_count.
const GUARDRAIL_WARN_NOTICE: Record<GuardrailType, string> = {
  fullscreen: 'You left fullscreen. This has been recorded.',
  focus: 'You left the quiz window. This has been recorded.',
  clipboard: 'Copy, cut, and paste are disabled during this quiz.',
}

const ATTEMPT_SOCKET_RECONNECT_MS = 3_000
// docs/05 section 5: "the attempt WebSocket sends a heartbeat every 10s" -
// matches server/internal/realtime/gateway.go's heartbeatTimeout (25s, 2.5x
// this), which flags the dashboard row "disconnected" once it lapses.
const HEARTBEAT_INTERVAL_MS = 10_000
// Matches server/internal/realtime/gateway.go's statusSessionReplaced: the
// close code the gateway force-closes a stale attempt:{id} socket with when
// a second device connects (docs/08 section 1 "single active session").
const SESSION_REPLACED_CLOSE_CODE = 4001

type Phase =
  | { kind: 'loading' }
  | { kind: 'load-error'; message: string }
  | { kind: 'playing' }
  | { kind: 'submitting' }
  // The attempt reached a terminal state under us: the student submitted,
  // the clock ran out, the teacher removed them, or the server refused a
  // write because the attempt is already terminal (submitted from another
  // tab, force-closed).
  | { kind: 'done'; reason: 'manual' | 'timeup' | 'closed' | 'kicked' }

// The docs/05 section 1 envelope every event on the attempt:{id} channel
// arrives in, same shape LiveMonitorPanel already decodes on the teacher
// side.
interface RealtimeEvent {
  type: string
  attempt_id: string
  payload: unknown
}

function attemptSocketURL(attemptId: string): string {
  const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${protocol}//${location.host}/ws/attempts/${attemptId}`
}

/**
 * The attempt player (docs/08): the snapshot questions without the key, a
 * cosmetic countdown driven by the server deadline plus a clock-offset
 * estimate, per-question debounced autosave, and the manual submit leg.
 * The server is the authority on time - when a write answers 409 the
 * player locks rather than argues.
 */
export default function AttemptPlayer({
  entry,
  onExit,
}: {
  entry: PlayerEntry
  onExit: () => void
}) {
  const { showToast } = useToast()
  const [phase, setPhase] = useState<Phase>({ kind: 'loading' })
  const [detail, setDetail] = useState<AttemptDetail | null>(null)
  const [answers, setAnswers] = useState<Record<string, ResponseValue>>({})
  const [remainingMs, setRemainingMs] = useState<number | null>(null)
  const [currentIndex, setCurrentIndex] = useState(0)
  const [confirming, setConfirming] = useState(false)
  // Flags are a navigation aid the student uses to come back to a question,
  // not state the grader cares about - the server has no field for them, so
  // they live and die with this mount.
  const [flagged, setFlagged] = useState<ReadonlySet<string>>(new Set())
  const [saveError, setSaveError] = useState<string | null>(null)
  // Autosave bookkeeping lives in refs (timers must survive renders);
  // bump forces a render so the indicator reads the current counts.
  const [, bump] = useReducer((n: number) => n + 1, 0)
  const timers = useRef(new Map<string, ReturnType<typeof setTimeout>>())
  const latest = useRef(new Map<string, ResponseValue>())
  const dirty = useRef(new Set<string>())
  const inflight = useRef(new Set<string>())
  // Server-minus-client clock estimate; the countdown is cosmetic (docs/08).
  const clockOffset = useRef(0)
  const submitted = useRef(false)

  // Milestone 6 client guardrails (docs/06 section 3). The player root is the
  // fullscreen target and the scope for the clipboard/context-menu block.
  const playerRoot = useRef<HTMLDivElement>(null)
  const [fullscreenOk, setFullscreenOk] = useState(true)
  const awaySince = useRef<number | null>(null)
  // Guardrail event listeners close over the phase at mount time; this ref
  // lets reportViolation see the current phase without re-subscribing them.
  const phaseRef = useRef(phase)
  useEffect(() => {
    phaseRef.current = phase
  }, [phase])

  // docs/05 section 3 / docs/06 section 4 step 4: the attempt:{id} socket
  // carries the kick lockout's reason text and quiz-wide extend/close
  // banners. kickReason backs the 'kicked' done screen; quizBanner is a
  // dismissable strip shown while still playing.
  const [kickReason, setKickReason] = useState<string | null>(null)
  const [quizBanner, setQuizBanner] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      const result =
        entry.kind === 'start'
          ? await api
              .POST('/api/v1/quizzes/{id}/attempts', {
                params: { path: { id: entry.quizId } },
              })
              .catch(() => null)
          : await api
              .GET('/api/v1/attempts/{id}', {
                params: { path: { id: entry.attemptId } },
              })
              .catch(() => null)
      if (cancelled) return
      if (!result?.data) {
        setPhase({
          kind: 'load-error',
          message:
            result?.error?.message ??
            'Could not open the attempt. Go back and retry.',
        })
        return
      }
      const loaded = result.data
      clockOffset.current = Date.parse(loaded.now) - Date.now()
      const initial: Record<string, ResponseValue> = {}
      for (const answer of loaded.answers) {
        const question = loaded.questions.find(
          (q) => q.id === answer.question_id,
        )
        const value = question
          ? coerceResponse(question.type, answer.response)
          : undefined
        if (value !== undefined) initial[answer.question_id] = value
      }
      setAnswers(initial)
      setDetail(loaded)
      setPhase({ kind: 'playing' })
    })()
    return () => {
      cancelled = true
    }
    // The entry is fixed for the lifetime of one player mount.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // The countdown tick. At zero the player locks and submits once; within
  // the server's grace the manual leg lands, past it the deadline job has
  // already force-submitted and the 409 resolves to the same terminal state.
  useEffect(() => {
    if (!detail || phase.kind !== 'playing') return
    const deadline = Date.parse(detail.attempt.deadline_at)
    const tick = () => {
      const left = deadline - (Date.now() + clockOffset.current)
      setRemainingMs(left)
      if (left <= 0 && !submitted.current) {
        submitted.current = true
        void submitNow('timeup')
      }
    }
    tick()
    const timer = setInterval(tick, 500)
    return () => clearInterval(timer)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [detail, phase.kind])

  // Pending debounce timers die with the player; the answers they covered
  // are already in `latest`, and submit flushes those explicitly.
  useEffect(() => {
    const pending = timers.current
    return () => pending.forEach((t) => clearTimeout(t))
  }, [])

  const lock = (reason: 'timeup' | 'closed' | 'kicked') => {
    timers.current.forEach((t) => clearTimeout(t))
    timers.current.clear()
    setPhase({ kind: 'done', reason })
  }

  // The REST fallback the docs call for (the attempt socket does not exist
  // yet): one request is one violation, no client-side dedup. The server is
  // the ladder's authority - a counted report that crosses max_violations
  // auto-submits under us, which the response's attempt.status reveals.
  const reportViolation = async (type: GuardrailType, durationMs?: number) => {
    if (!detail || phaseRef.current.kind !== 'playing') return
    const result = await api
      .POST('/api/v1/attempts/{id}/events', {
        params: { path: { id: detail.attempt.id } },
        body: { type, duration_ms: durationMs ?? null },
      })
      .catch(() => null)
    if (result?.data) {
      const nextAttempt = result.data.attempt
      setDetail((prev) => (prev ? { ...prev, attempt: nextAttempt } : prev))
      if (nextAttempt.status !== 'in_progress') {
        // The violation ladder's auto_submit fired server-side.
        lock('closed')
        return
      }
      showToast(
        result.data.counted
          ? `Violation ${nextAttempt.violation_count} of ${detail.guardrails.max_violations} - stay in the quiz window.`
          : GUARDRAIL_WARN_NOTICE[type],
        'warning',
      )
      return
    }
    const code = result?.error?.code
    if (code === 'ATTEMPT_KICKED') {
      lock('kicked')
    } else if (code === 'ATTEMPT_ALREADY_SUBMITTED') {
      lock('closed')
    }
    // GUARDRAIL_OFF and network failures are not worth surfacing: the report
    // was best-effort evidence, not something the student can act on.
  }

  // Fullscreen lock (docs/06: "leaving raises a violation and overlays a
  // 'return to fullscreen' blocker"). Requesting fullscreen right after the
  // attempt loads rides the same user gesture that started the attempt.
  useEffect(() => {
    if (phase.kind !== 'playing' || !detail || detail.guardrails.fullscreen === 'off') {
      return
    }
    const request = document.documentElement.requestFullscreen?.()
    if (request) {
      request.then(
        () => setFullscreenOk(true),
        () => setFullscreenOk(false),
      )
    } else {
      setFullscreenOk(false)
    }
    const onChange = () => {
      const active = document.fullscreenElement != null
      setFullscreenOk(active)
      if (!active) void reportViolation('fullscreen')
    }
    document.addEventListener('fullscreenchange', onChange)
    return () => document.removeEventListener('fullscreenchange', onChange)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [phase.kind])

  // Focus tracking (docs/06: "visibilitychange + blur", reported with the
  // away duration). awaySince guards against double-counting when blur and
  // visibilitychange both fire for the same tab switch.
  useEffect(() => {
    if (phase.kind !== 'playing' || !detail || detail.guardrails.focus_tracking === 'off') {
      return
    }
    const markAway = () => {
      if (awaySince.current === null) awaySince.current = Date.now()
    }
    const markBack = () => {
      if (awaySince.current === null) return
      const durationMs = Date.now() - awaySince.current
      awaySince.current = null
      void reportViolation('focus', durationMs)
    }
    const onVisibility = () => (document.hidden ? markAway() : markBack())
    document.addEventListener('visibilitychange', onVisibility)
    window.addEventListener('blur', markAway)
    window.addEventListener('focus', markBack)
    return () => {
      document.removeEventListener('visibilitychange', onVisibility)
      window.removeEventListener('blur', markAway)
      window.removeEventListener('focus', markBack)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [phase.kind])

  // Clipboard / context-menu block (docs/06: "usage attempts logged"),
  // scoped to the player rather than the whole document.
  useEffect(() => {
    const node = playerRoot.current
    if (phase.kind !== 'playing' || !detail || !detail.guardrails.block_clipboard || !node) {
      return
    }
    const block = (e: Event) => {
      e.preventDefault()
      void reportViolation('clipboard')
    }
    node.addEventListener('copy', block)
    node.addEventListener('cut', block)
    node.addEventListener('paste', block)
    node.addEventListener('contextmenu', block)
    return () => {
      node.removeEventListener('copy', block)
      node.removeEventListener('cut', block)
      node.removeEventListener('paste', block)
      node.removeEventListener('contextmenu', block)
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [phase.kind])

  // The attempt:{id} socket (docs/05 section 3): the primary delivery path
  // for the kick lockout and quiz.extended/closed banners, with the REST
  // 409 fallback above covering a dropped connection. Reconnects on its own
  // timer, entirely outside React state, so a flaky socket never leaks
  // overlapping connections; it only runs while still playing; a lock via
  // any other path unmounts this effect and the socket is closed.
  useEffect(() => {
    if (phase.kind !== 'playing' || !detail) return
    const attemptId = detail.attempt.id
    let cancelled = false
    let socket: WebSocket | null = null
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null
    let heartbeatTimer: ReturnType<typeof setInterval> | null = null

    const stopHeartbeat = () => {
      if (heartbeatTimer) clearInterval(heartbeatTimer)
      heartbeatTimer = null
    }

    const connect = () => {
      if (cancelled) return
      socket = new WebSocket(attemptSocketURL(attemptId))
      // docs/05 section 5: any frame counts as the heartbeat on the server
      // side, so the content itself carries no meaning.
      socket.onopen = () => {
        heartbeatTimer = setInterval(() => {
          if (socket?.readyState === WebSocket.OPEN) socket.send('heartbeat')
        }, HEARTBEAT_INTERVAL_MS)
      }
      socket.onmessage = (event) => {
        if (typeof event.data !== 'string') return
        let msg: RealtimeEvent
        try {
          msg = JSON.parse(event.data) as RealtimeEvent
        } catch {
          return
        }
        switch (msg.type) {
          case 'attempt.kicked': {
            const p = msg.payload as { reason?: string }
            setKickReason(p.reason?.trim() || null)
            lock('kicked')
            break
          }
          case 'quiz.extended': {
            const p = msg.payload as { ends_at: string }
            setQuizBanner(`Your teacher extended this quiz - new end time ${formatWhen(p.ends_at)}.`)
            // Refresh so the countdown reflects any deadline_at the
            // extension pulled forward (docs/06: least(started_at +
            // duration, new ends_at)).
            void api
              .GET('/api/v1/attempts/{id}', { params: { path: { id: attemptId } } })
              .then((result) => {
                if (cancelled || !result.data) return
                clockOffset.current = Date.parse(result.data.now) - Date.now()
                setDetail(result.data)
              })
              .catch(() => {})
            break
          }
          case 'quiz.closed': {
            setQuizBanner('Your teacher closed this quiz. Any saved answers will be submitted shortly.')
            break
          }
          default:
            break
        }
      }
      socket.onclose = (event) => {
        stopHeartbeat()
        if (cancelled) return
        // docs/08 section 1 "single active session": the server closes this
        // socket with 4001 when the same attempt was opened elsewhere.
        // Reconnecting would just re-invalidate that other tab/device right
        // back, so this socket stays down instead of racing it forever.
        if (event.code === SESSION_REPLACED_CLOSE_CODE) {
          setQuizBanner('This attempt was opened in another window or device. This window is no longer live.')
          return
        }
        reconnectTimer = setTimeout(connect, ATTEMPT_SOCKET_RECONNECT_MS)
      }
      socket.onerror = () => {
        socket?.close()
      }
    }
    connect()

    return () => {
      cancelled = true
      stopHeartbeat()
      if (reconnectTimer) clearTimeout(reconnectTimer)
      socket?.close()
    }
    // Keyed on the attempt id, not the whole `detail` object: reportViolation
    // and the quiz.extended refresh above both call setDetail while playing,
    // and neither should tear down and reconnect a perfectly good socket.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [phase.kind, detail?.attempt.id])

  const saveOne = async (questionId: string): Promise<boolean> => {
    if (!detail) return false
    const value = latest.current.get(questionId)
    if (value === undefined) return true
    inflight.current.add(questionId)
    dirty.current.delete(questionId)
    bump()
    const result = await api
      .PUT('/api/v1/attempts/{id}/answers/{questionId}', {
        params: { path: { id: detail.attempt.id, questionId } },
        body: { response: value },
      })
      .catch(() => null)
    inflight.current.delete(questionId)
    if (result?.data) {
      clockOffset.current = Date.parse(result.data.now) - Date.now()
      setSaveError(null)
      // A newer edit landed while this one was in flight; it re-marked the
      // question dirty and its own debounce timer is already running.
      bump()
      return true
    }
    if (result?.response.status === 409) {
      // The attempt is terminal server-side; nothing more can be written.
      lock(result?.error?.code === 'ATTEMPT_KICKED' ? 'kicked' : 'closed')
      return false
    }
    dirty.current.add(questionId)
    setSaveError(
      result?.error?.message ??
        'Could not save your answer. It will retry on your next change.',
    )
    bump()
    return false
  }

  const setAnswer = (questionId: string, value: ResponseValue) => {
    setAnswers((prev) => ({ ...prev, [questionId]: value }))
    latest.current.set(questionId, value)
    dirty.current.add(questionId)
    const existing = timers.current.get(questionId)
    if (existing) clearTimeout(existing)
    timers.current.set(
      questionId,
      setTimeout(() => {
        timers.current.delete(questionId)
        void saveOne(questionId)
      }, AUTOSAVE_DELAY_MS),
    )
    bump()
  }

  const submitNow = async (reason: 'manual' | 'timeup') => {
    if (!detail) return
    setPhase({ kind: 'submitting' })
    // Flush whatever the debounce is still holding - the submit funnel
    // takes what is autosaved, so unsaved answers must land first.
    const unsaved = [...dirty.current]
    timers.current.forEach((t) => clearTimeout(t))
    timers.current.clear()
    await Promise.all(unsaved.map((questionId) => saveOne(questionId)))
    const result = await api
      .POST('/api/v1/attempts/{id}/submit', {
        params: { path: { id: detail.attempt.id } },
      })
      .catch(() => null)
    if (result?.data || result?.response.status === 409) {
      // 200 is the idempotent funnel; 409 means past grace, where the
      // deadline job owns the submission - either way the attempt is done.
      if (reason === 'manual') {
        showToast('Quiz submitted - graded instantly.', 'success')
      }
      setPhase({ kind: 'done', reason })
      return
    }
    setSaveError(
      result?.error?.message ?? 'Could not submit. Check your connection.',
    )
    submitted.current = false
    setPhase({ kind: 'playing' })
    setConfirming(false)
  }

  if (phase.kind === 'loading') {
    return (
      <p className="boot-note" role="status">
        Loading…
      </p>
    )
  }

  if (phase.kind === 'load-error') {
    return (
      <div className="player">
        <p className="form-error">{phase.message}</p>
        <button className="button button-quiet" type="button" onClick={onExit}>
          Back to assigned quizzes
        </button>
      </div>
    )
  }

  if (!detail) return null

  // St3: the lockout is its own danger-tinted surface, not a card on the page
  // ground. Red is the human decision (docs/11 section 5), so it is spent here
  // and nowhere else in the student's flow.
  if (phase.kind === 'done' && phase.reason === 'kicked') {
    return (
      <div className="lockout" role="alert">
        <div className="lockout-body">
          <span className="lockout-mark" aria-hidden="true">
            !
          </span>
          <h1 className="lockout-title">You've been removed from this quiz</h1>
          <p className="lockout-copy">
            Your teacher ended your attempt.{' '}
            <b>Your work has been kept and will be graded.</b> You can't rejoin
            unless a teacher readmits you - that would start a fresh attempt.
          </p>
          {kickReason && (
            <div className="lockout-reason">
              <span className="eyebrow">Reason given</span>
              <span className="lockout-reason-text">{kickReason}</span>
            </div>
          )}
          <button className="button button-quiet" type="button" onClick={onExit}>
            Back to assigned quizzes
          </button>
        </div>
      </div>
    )
  }

  if (phase.kind === 'done') {
    return (
      <div className="player-done-screen">
        <section className="card player-done">
          <span
            className={`chip chip-lifecycle ${phase.reason === 'manual' ? 'chip-lifecycle-submitted' : 'chip-lifecycle-closed'}`}
          >
            {phase.reason === 'manual' ? (
              <>
                <span className="chip-dot" aria-hidden="true" />
                Submitted
              </>
            ) : (
              'Closed'
            )}
          </span>
          <h1 className="player-done-title">
            {phase.reason === 'timeup'
              ? 'Time is up'
              : phase.reason === 'closed'
                ? 'This attempt is closed'
                : 'Attempt submitted'}
          </h1>
          <p className="hint">
            {phase.reason === 'timeup'
              ? 'The time limit ran out, so your saved answers were submitted for you.'
              : phase.reason === 'closed'
                ? 'The attempt already ended - your saved answers stand as the submission.'
                : 'Your answers are in. Scores appear on your quiz list once results are released.'}
          </p>
          <button
            className="button button-primary player-done-action"
            type="button"
            onClick={onExit}
          >
            Back to assigned quizzes
          </button>
        </section>
      </div>
    )
  }

  const saving = inflight.current.size > 0 || dirty.current.size > 0
  const unansweredCount = detail.questions.filter(
    (q) => !isAnswered(answers[q.id]),
  ).length
  const urgent = remainingMs !== null && remainingMs < TIMER_URGENT_MS

  // One question shows at a time; the sidebar grid jumps anywhere. Publish
  // requires at least one question (docs/04), so the snapshot is never empty.
  const questionCount = detail.questions.length
  const safeIndex = Math.min(currentIndex, questionCount - 1)
  const currentQuestion = detail.questions[safeIndex]

  const fullscreenGuarded =
    phase.kind === 'playing' && detail.guardrails.fullscreen !== 'off' && !fullscreenOk

  const answeredCount = questionCount - unansweredCount
  const currentFlagged = flagged.has(currentQuestion.id)
  const toggleFlag = () => {
    setFlagged((prev) => {
      const next = new Set(prev)
      if (next.has(currentQuestion.id)) next.delete(currentQuestion.id)
      else next.add(currentQuestion.id)
      return next
    })
  }

  return (
    <div className="player" ref={playerRoot}>
      <header className="player-topbar">
        <div className="player-identity">
          <span className="brand-mark brand-mark-small" aria-hidden="true">
            M
          </span>
          <div className="player-identity-text">
            <h1 className="player-quiz-title">{detail.quiz_title}</h1>
            <p className="player-quiz-sub">
              Attempt {detail.attempt.attempt_no} · autosaves as you go
            </p>
          </div>
        </div>
        <div className="player-topbar-right">
          <span
            className={`save-state ${saveError ? 'save-state-bad' : saving ? 'save-state-busy' : 'save-state-ok'}`}
            role="status"
          >
            <span className="save-state-dot" aria-hidden="true" />
            {saveError ? 'Not saved' : saving ? 'Saving…' : 'All changes saved'}
          </span>
          <div className={`player-timer${urgent ? ' player-timer-urgent' : ''}`}>
            <span className="player-timer-value tabular" role="timer" aria-label="Time remaining">
              {remainingMs === null ? '–:––' : formatRemaining(remainingMs)}
            </span>
            <span className="player-timer-label">Time left</span>
          </div>
        </div>
      </header>

      {(saveError || quizBanner) && (
        <div className="player-banners">
          {saveError && <p className="form-error">{saveError}</p>}
          {quizBanner && (
            <p className="quiz-banner" role="status">
              <span>{quizBanner}</span>
              <button
                className="quiz-banner-dismiss"
                type="button"
                onClick={() => setQuizBanner(null)}
                aria-label="Dismiss"
              >
                ×
              </button>
            </p>
          )}
        </div>
      )}

      {fullscreenGuarded && (
        <div className="fullscreen-lock-overlay" role="alertdialog" aria-modal="true">
          <div className="fullscreen-lock-card">
            <span className="chip chip-lifecycle chip-lifecycle-warning">
              Fullscreen exited
            </span>
            <h2 className="fullscreen-lock-title">
              Return to fullscreen to continue
            </h2>
            <p className="hint">
              Your timer is still running against the server deadline. This exit
              was recorded as a counted violation (
              <span className="tabular">
                {detail.attempt.violation_count} of{' '}
                {detail.guardrails.max_violations}
              </span>
              ).
            </p>
            <button
              className="button button-primary fullscreen-lock-action"
              type="button"
              onClick={() => {
                void document.documentElement
                  .requestFullscreen?.()
                  ?.then(() => setFullscreenOk(true))
              }}
            >
              Re-enter fullscreen
            </button>
          </div>
        </div>
      )}

      <div className="player-body">
        <nav className="player-nav" aria-label="Question navigator">
          <div className="player-nav-head">
            <span className="eyebrow">Questions</span>
            <span className="player-nav-count tabular">
              {answeredCount} of {questionCount} answered
            </span>
          </div>
          <ol className="nav-grid">
            {detail.questions.map((question, index) => {
              const answered = isAnswered(answers[question.id])
              const current = index === safeIndex
              const flag = flagged.has(question.id)
              // Answered-ness and current-ness are independent facts; the
              // current cell just paints over the answered one.
              const state = `${answered ? ' nav-cell-answered' : ' nav-cell-empty'}${current ? ' nav-cell-current' : ''}`
              return (
                <li className="nav-cell-slot" key={question.id}>
                  <button
                    type="button"
                    className={`nav-cell${state}`}
                    aria-current={current ? 'true' : undefined}
                    aria-label={`Question ${question.position}, ${answered ? 'answered' : 'unanswered'}${flag ? ', flagged' : ''}`}
                    onClick={() => setCurrentIndex(index)}
                  >
                    {question.position}
                  </button>
                  {flag && <span className="nav-flag" aria-hidden="true" />}
                </li>
              )
            })}
          </ol>
          <div className="nav-legend">
            <span className="legend-row">
              <span className="legend-flag" aria-hidden="true" />
              Flagged for review
            </span>
            <span className="legend-row">
              <span className="legend-answered" aria-hidden="true" />
              Answered
            </span>
          </div>
        </nav>

        <section className="player-pane">
          <div className="player-question-area">
            <div className="player-question-head">
              <div className="player-question-headline">
                <span className="player-question-eyebrow">
                  Question {safeIndex + 1} of {questionCount}
                  <span className="player-question-marks tabular">
                    {' · '}
                    {currentQuestion.points}{' '}
                    {currentQuestion.points === 1 ? 'mark' : 'marks'}
                    {currentQuestion.penalty > 0 &&
                      ` · −${currentQuestion.penalty} if wrong`}
                  </span>
                </span>
                <h2 className="player-question-text">
                  {currentQuestion.body.text}
                </h2>
              </div>
              <span className="chip chip-type">
                {QUESTION_TYPE_LABEL[currentQuestion.type]}
              </span>
            </div>
            <PlayerQuestion
              question={currentQuestion}
              value={answers[currentQuestion.id]}
              disabled={phase.kind === 'submitting'}
              onChange={(value) => setAnswer(currentQuestion.id, value)}
            />
          </div>

          <footer className="player-footer">
            {confirming ? (
              <>
                <p className="player-footer-note">
                  {unansweredCount > 0
                    ? `${unansweredCount} question${unansweredCount === 1 ? ' is' : 's are'} unanswered. Submit anyway?`
                    : 'All questions answered. Submit now?'}
                </p>
                <div className="player-footer-actions">
                  <button
                    className="button button-quiet"
                    type="button"
                    disabled={phase.kind === 'submitting'}
                    onClick={() => setConfirming(false)}
                  >
                    Keep working
                  </button>
                  <button
                    className="button button-commit"
                    type="button"
                    disabled={phase.kind === 'submitting'}
                    onClick={() => {
                      submitted.current = true
                      void submitNow('manual')
                    }}
                  >
                    {phase.kind === 'submitting' ? 'Submitting…' : 'Submit now'}
                  </button>
                </div>
              </>
            ) : (
              <>
                <button
                  className="button button-quiet"
                  type="button"
                  disabled={safeIndex === 0}
                  onClick={() => setCurrentIndex(safeIndex - 1)}
                >
                  ← Previous
                </button>
                <div className="player-footer-actions">
                  <button
                    className={`button button-flag${currentFlagged ? ' button-flag-on' : ''}`}
                    type="button"
                    aria-pressed={currentFlagged}
                    onClick={toggleFlag}
                  >
                    ⚑ {currentFlagged ? 'Flagged' : 'Flag'}
                  </button>
                  <button
                    className="button button-primary"
                    type="button"
                    disabled={safeIndex === questionCount - 1}
                    onClick={() => setCurrentIndex(safeIndex + 1)}
                  >
                    Next →
                  </button>
                  <button
                    className="button button-commit"
                    type="button"
                    onClick={() => setConfirming(true)}
                  >
                    Review and submit
                  </button>
                </div>
              </>
            )}
          </footer>
        </section>
      </div>
    </div>
  )
}

function PlayerQuestion({
  question,
  value,
  disabled,
  onChange,
}: {
  question: AttemptQuestion
  value: ResponseValue | undefined
  disabled: boolean
  onChange: (value: ResponseValue) => void
}) {
  return (
    <fieldset className="player-fieldset" disabled={disabled}>
      <legend className="visually-hidden">{question.body.text}</legend>

      {question.type === 'single' && (
        <div className="option-list">
          {(question.options ?? []).map((option) => (
            <label
              key={option.key}
              className={`option-row${value === option.key ? ' option-row-selected' : ''}`}
            >
              <input
                type="radio"
                name={`q-${question.id}`}
                checked={value === option.key}
                onChange={() => onChange(option.key)}
              />
              <span className="option-key">{option.key.toUpperCase()}</span>
              <span className="option-static">{option.text}</span>
            </label>
          ))}
        </div>
      )}

      {question.type === 'multi' && (
        <div className="option-list">
          {(question.options ?? []).map((option) => {
            const picked = Array.isArray(value) && value.includes(option.key)
            return (
              <label
                key={option.key}
                className={`option-row${picked ? ' option-row-selected' : ''}`}
              >
                <input
                  type="checkbox"
                  checked={picked}
                  onChange={() => {
                    const current = Array.isArray(value) ? value : []
                    onChange(
                      picked
                        ? current.filter((k) => k !== option.key)
                        : [...current, option.key].sort(),
                    )
                  }}
                />
                <span className="option-key">{option.key.toUpperCase()}</span>
                <span className="option-static">{option.text}</span>
              </label>
            )
          })}
        </div>
      )}

      {question.type === 'truefalse' && (
        <div className="option-list">
          {[true, false].map((bool) => (
            <label
              key={String(bool)}
              className={`option-row${value === bool ? ' option-row-selected' : ''}`}
            >
              <input
                type="radio"
                name={`q-${question.id}`}
                checked={value === bool}
                onChange={() => onChange(bool)}
              />
              <span className="option-key">{bool ? 'T' : 'F'}</span>
              <span className="option-static">{bool ? 'True' : 'False'}</span>
            </label>
          ))}
        </div>
      )}

      {question.type === 'short' && (
        <input
          className="input player-short-input"
          type="text"
          placeholder="Type your answer"
          value={typeof value === 'string' ? value : ''}
          onChange={(e) => onChange(e.target.value)}
        />
      )}
    </fieldset>
  )
}
