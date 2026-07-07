import { useEffect, useReducer, useRef, useState } from 'react'
import { api } from '../api/client'
import {
  coerceResponse,
  formatRemaining,
  isAnswered,
  type AttemptDetail,
  type AttemptQuestion,
  type ResponseValue,
} from './model'

export type PlayerEntry =
  | { kind: 'start'; quizId: string }
  | { kind: 'resume'; attemptId: string }

const AUTOSAVE_DELAY_MS = 600

type Phase =
  | { kind: 'loading' }
  | { kind: 'load-error'; message: string }
  | { kind: 'playing' }
  | { kind: 'submitting' }
  // The attempt reached a terminal state under us: the student submitted,
  // the clock ran out, or the server refused a write because the attempt
  // is already terminal (submitted from another tab, force-closed).
  | { kind: 'done'; reason: 'manual' | 'timeup' | 'closed' }

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
  const [phase, setPhase] = useState<Phase>({ kind: 'loading' })
  const [detail, setDetail] = useState<AttemptDetail | null>(null)
  const [answers, setAnswers] = useState<Record<string, ResponseValue>>({})
  const [remainingMs, setRemainingMs] = useState<number | null>(null)
  const [confirming, setConfirming] = useState(false)
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

  const lock = (reason: 'timeup' | 'closed') => {
    timers.current.forEach((t) => clearTimeout(t))
    timers.current.clear()
    setPhase({ kind: 'done', reason })
  }

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
      lock('closed')
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
          Back to my quizzes
        </button>
      </div>
    )
  }

  if (!detail) return null

  if (phase.kind === 'done') {
    return (
      <div className="player">
        <section className="panel player-done">
          <h1 className="card-title">
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
            className="button button-primary"
            type="button"
            onClick={onExit}
          >
            Back to my quizzes
          </button>
        </section>
      </div>
    )
  }

  const saving = inflight.current.size > 0 || dirty.current.size > 0
  const unansweredCount = detail.questions.filter(
    (q) => !isAnswered(answers[q.id]),
  ).length
  const urgent = remainingMs !== null && remainingMs < 60_000

  return (
    <div className="player">
      <header className="player-topbar">
        <div className="player-heading">
          <p className="eyebrow">Attempt {detail.attempt.attempt_no}</p>
          <h1 className="page-title">{detail.quiz_title}</h1>
        </div>
        <span
          className={`save-badge ${saveError ? 'save-badge-bad' : saving ? 'save-badge-busy' : 'save-badge-ok'}`}
          role="status"
        >
          {saveError ? 'Not saved' : saving ? 'Saving…' : 'All changes saved'}
        </span>
        <span
          className={`countdown${urgent ? ' countdown-urgent' : ''}`}
          role="timer"
          aria-label="Time remaining"
        >
          {remainingMs === null ? '–:––' : formatRemaining(remainingMs)}
        </span>
      </header>

      {saveError && <p className="form-error">{saveError}</p>}

      <ol className="player-questions">
        {detail.questions.map((question) => (
          <li key={question.id} className="panel player-question">
            <PlayerQuestion
              question={question}
              value={answers[question.id]}
              disabled={phase.kind === 'submitting'}
              onChange={(value) => setAnswer(question.id, value)}
            />
          </li>
        ))}
      </ol>

      <footer className="panel player-footer">
        {confirming ? (
          <>
            <p className="player-footer-note">
              {unansweredCount > 0
                ? `${unansweredCount} question${unansweredCount === 1 ? ' is' : 's are'} unanswered. Submit anyway?`
                : 'All questions answered. Submit now?'}
            </p>
            <button
              className="button button-primary"
              type="button"
              disabled={phase.kind === 'submitting'}
              onClick={() => {
                submitted.current = true
                void submitNow('manual')
              }}
            >
              {phase.kind === 'submitting' ? 'Submitting…' : 'Submit now'}
            </button>
            <button
              className="button button-quiet"
              type="button"
              disabled={phase.kind === 'submitting'}
              onClick={() => setConfirming(false)}
            >
              Keep working
            </button>
          </>
        ) : (
          <>
            <p className="player-footer-note">
              {unansweredCount > 0
                ? `${unansweredCount} of ${detail.questions.length} questions still unanswered.`
                : 'Every question has an answer.'}
            </p>
            <button
              className="button button-primary"
              type="button"
              onClick={() => setConfirming(true)}
            >
              Submit attempt
            </button>
          </>
        )}
      </footer>
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
      <legend className="player-question-head">
        <span className="question-index">{question.position}</span>
        <span className="player-question-text">{question.body.text}</span>
        <span className="player-question-points tabular">
          {question.points} pt{question.points === 1 ? '' : 's'}
        </span>
      </legend>

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
