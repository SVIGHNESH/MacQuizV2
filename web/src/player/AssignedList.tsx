import { useEffect, useState } from 'react'
import { api } from '../api/client'
import {
  formatClock,
  formatDayAndClock,
  formatDuration,
  formatWhen,
  type AssignedQuiz,
  type AttemptSummary,
} from './model'

/**
 * The student home (docs/11 St1): every assigned quiz as one card carrying a
 * lifecycle chip, its window, and the single action its state allows - start,
 * resume, or review. Scores come release-gated from the server; a null score
 * on a finished attempt reads as withheld, never as zero.
 *
 * The All/To do/Done pills filter the same list client-side - the assigned
 * endpoint returns every quiz in one page, so there is nothing to re-fetch.
 */
export default function AssignedList({
  onStart,
  onResume,
  onReview,
}: {
  onStart: (quizId: string) => void
  onResume: (attemptId: string) => void
  onReview: (attemptId: string) => void
}) {
  const [quizzes, setQuizzes] = useState<AssignedQuiz[] | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [filter, setFilter] = useState<Filter>('all')

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      const result = await api.GET('/api/v1/quizzes/assigned').catch(() => null)
      if (cancelled) return
      if (!result?.data) {
        setLoadError(
          result?.error?.message ??
            'Could not load your quizzes. Reload to retry.',
        )
        return
      }
      setQuizzes(result.data.quizzes)
    })()
    return () => {
      cancelled = true
    }
  }, [])

  if (loadError) return <p className="form-error">{loadError}</p>
  if (!quizzes) {
    return (
      <p className="boot-note" role="status">
        Loading…
      </p>
    )
  }

  // "Open" is what the student can act on right now, not merely what is
  // inside its window - a submitted quiz in a live window is not open to them.
  const openCount = quizzes.filter((quiz) => {
    const kind = cardState(quiz).kind
    return kind === 'start' || kind === 'resume'
  }).length

  const shown = quizzes.filter(
    (quiz) => filter === 'all' || bucket(cardState(quiz)) === filter,
  )

  return (
    <div className="assigned">
      <div className="page-head">
        <div>
          <h1 className="page-title">Assigned quizzes</h1>
          <p className="assigned-subtitle">
            {openCount === 0
              ? 'Nothing open right now'
              : `${openCount} open`}{' '}
            · times shown in your local time
          </p>
        </div>
        {quizzes.length > 0 && (
          <div
            className="assigned-filter"
            role="group"
            aria-label="Filter quizzes"
          >
            {FILTERS.map(({ value, label }) => (
              <button
                key={value}
                className="assigned-filter-pill"
                type="button"
                aria-pressed={filter === value}
                onClick={() => setFilter(value)}
              >
                {label}
              </button>
            ))}
          </div>
        )}
      </div>

      {quizzes.length > 0 && (
        <p className="visually-hidden" role="status">
          {`Showing ${shown.length} of ${quizzes.length} ${
            quizzes.length === 1 ? 'quiz' : 'quizzes'
          }`}
        </p>
      )}

      {quizzes.length === 0 ? (
        <section className="empty-state">
          <h2 className="card-title">Nothing assigned yet</h2>
          <p className="hint">
            When a teacher assigns you a quiz it appears here with its open
            window and time limit.
          </p>
        </section>
      ) : shown.length === 0 ? (
        <section className="empty-state">
          <h2 className="card-title">
            {filter === 'todo' ? 'Nothing left to do' : 'Nothing finished yet'}
          </h2>
          <p className="hint">
            {filter === 'todo'
              ? 'Every quiz assigned to you has been taken or has closed.'
              : 'Quizzes you have taken, or that have closed, collect here.'}
          </p>
        </section>
      ) : (
        <div className="assigned-grid">
          {shown.map((quiz) => (
            <AssignedCard
              key={quiz.id}
              quiz={quiz}
              onStart={onStart}
              onResume={onResume}
              onReview={onReview}
            />
          ))}
        </div>
      )}
    </div>
  )
}

type Filter = 'all' | 'todo' | 'done'

const FILTERS: { value: Filter; label: string }[] = [
  { value: 'all', label: 'All' },
  { value: 'todo', label: 'To do' },
  { value: 'done', label: 'Done' },
]

/**
 * Which pill a card answers to, read off the same CardState the chip and the
 * action button use, so a card can never sit under "To do" while offering no
 * action. "To do" is work the student still owes: one they can start or
 * resume now, and one that has not opened yet. Everything else - taken,
 * awaiting release, out of attempts, closed, or terminated - is "Done", in
 * the sense that nothing more is being asked of them.
 */
function bucket(state: CardState): Exclude<Filter, 'all'> {
  switch (state.kind) {
    case 'start':
    case 'resume':
    case 'scheduled':
      return 'todo'
    case 'removed':
    case 'awaiting-release':
    case 'released':
    case 'exhausted':
    case 'closed':
      return 'done'
  }
}

/** The attempt whose state the card speaks for: the student's most recent. */
function latestAttempt(quiz: AssignedQuiz): AttemptSummary | undefined {
  return quiz.attempts.reduce<AttemptSummary | undefined>(
    (latest, attempt) =>
      !latest || attempt.attempt_no > latest.attempt_no ? attempt : latest,
    undefined,
  )
}

/**
 * One card, one state, one action. The chip and the button are read off the
 * same value so they can never disagree - and an open window with attempts
 * left always wins over a finished attempt, otherwise submitting attempt 1
 * would strand a student who is entitled to attempt 2.
 */
type CardState =
  | { kind: 'removed' }
  | { kind: 'resume'; attempt: AttemptSummary }
  | { kind: 'start'; fresh: boolean }
  | { kind: 'scheduled' }
  | { kind: 'awaiting-release' }
  | { kind: 'released'; attempt: AttemptSummary; score: number }
  | { kind: 'exhausted' }
  | { kind: 'closed' }

function cardState(quiz: AssignedQuiz): CardState {
  const latest = latestAttempt(quiz)
  const inProgress = quiz.attempts.find((a) => a.status === 'in_progress')
  const attemptsLeft = quiz.max_attempts - quiz.attempts.length
  const released = quiz.results_released_at !== null

  if (latest?.status === 'kicked') return { kind: 'removed' }
  if (inProgress) return { kind: 'resume', attempt: inProgress }
  if (quiz.status === 'live' && attemptsLeft > 0) {
    return { kind: 'start', fresh: quiz.attempts.length === 0 }
  }
  if (quiz.status === 'scheduled') return { kind: 'scheduled' }
  if (latest && released && latest.score !== null) {
    return { kind: 'released', attempt: latest, score: latest.score }
  }
  if (latest) return { kind: 'awaiting-release' }
  if (quiz.status === 'live') return { kind: 'exhausted' }
  return { kind: 'closed' }
}

function AssignedCard({
  quiz,
  onStart,
  onResume,
  onReview,
}: {
  quiz: AssignedQuiz
  onStart: (quizId: string) => void
  onResume: (attemptId: string) => void
  onReview: (attemptId: string) => void
}) {
  const state = cardState(quiz)

  return (
    <section className="assigned-card">
      <header className="assigned-card-head">
        <CardChip state={state} />
        <span className="assigned-attempts tabular">
          {state.kind === 'released' ? (
            <span className="assigned-score">{state.score} pts</span>
          ) : (
            <>
              {quiz.attempts.length} / {quiz.max_attempts} attempt
              {quiz.max_attempts === 1 ? '' : 's'}
            </>
          )}
        </span>
      </header>

      <div className="assigned-card-title">
        <h2 className="card-title">{quiz.title}</h2>
        <p className="assigned-meta">
          {quiz.question_count} question{quiz.question_count === 1 ? '' : 's'} ·{' '}
          {formatDuration(quiz.duration_sec)}
          {quiz.status === 'live' && ` · closes ${formatClock(quiz.ends_at)}`}
        </p>
      </div>

      <CardBody quiz={quiz} state={state} />

      <div className="assigned-card-action">
        {state.kind === 'resume' ? (
          <button
            className="button button-primary"
            type="button"
            onClick={() => onResume(state.attempt.id)}
          >
            Resume attempt
          </button>
        ) : state.kind === 'start' ? (
          <button
            className="button button-primary"
            type="button"
            onClick={() => onStart(quiz.id)}
          >
            {state.fresh ? 'Start attempt' : 'Start new attempt'}
          </button>
        ) : state.kind === 'released' ? (
          <button
            className="button button-quiet"
            type="button"
            onClick={() => onReview(state.attempt.id)}
          >
            Review answers
          </button>
        ) : (
          <button className="button button-quiet" type="button" disabled>
            {state.kind === 'scheduled'
              ? `Opens ${formatWhen(quiz.starts_at)}`
              : state.kind === 'awaiting-release'
                ? 'Results pending'
                : state.kind === 'exhausted'
                  ? 'No attempts left'
                  : 'Closed'}
          </button>
        )}
      </div>
    </section>
  )
}

function CardChip({ state }: { state: CardState }) {
  switch (state.kind) {
    case 'removed':
      return (
        <span className="chip chip-lifecycle chip-lifecycle-removed">
          Removed
        </span>
      )
    case 'resume':
    case 'start':
    case 'exhausted':
      return (
        <span className="chip chip-lifecycle chip-lifecycle-live">
          <span className="chip-dot" aria-hidden="true" />
          Live
        </span>
      )
    case 'scheduled':
      return (
        <span className="chip chip-lifecycle chip-lifecycle-scheduled">
          Scheduled
        </span>
      )
    case 'awaiting-release':
      return (
        <span className="chip chip-lifecycle chip-lifecycle-submitted">
          <span className="chip-dot" aria-hidden="true" />
          Submitted
        </span>
      )
    case 'released':
    case 'closed':
      return (
        <span className="chip chip-lifecycle chip-lifecycle-closed">Closed</span>
      )
  }
}

/** The one line between the title and the action that explains the state. */
function CardBody({ quiz, state }: { quiz: AssignedQuiz; state: CardState }) {
  switch (state.kind) {
    case 'removed':
      return (
        <p className="assigned-note assigned-note-danger">
          Attempt terminated · your work was kept and will be graded.
        </p>
      )
    case 'resume':
      return (
        <p className="assigned-status">
          In progress · started {formatWhen(state.attempt.started_at)}
        </p>
      )
    case 'start':
    case 'exhausted':
      return (
        <p className="assigned-status">
          Closes {formatWhen(quiz.ends_at)} · one sitting once you start.
        </p>
      )
    case 'scheduled':
      return (
        <div className="assigned-window">
          <span className="assigned-window-open">
            Opens {formatDayAndClock(quiz.starts_at)}
          </span>
          <span className="assigned-window-close">
            Closes {formatDayAndClock(quiz.ends_at)}
          </span>
        </div>
      )
    case 'awaiting-release':
      return (
        <p className="assigned-note">
          Results not released yet. Your score appears here once your teacher
          releases them.
        </p>
      )
    case 'released':
      return (
        <p className="assigned-status">
          {state.attempt.submitted_at
            ? `Released · submitted ${formatWhen(state.attempt.submitted_at)}`
            : 'Released · score visible'}
        </p>
      )
    case 'closed':
      return <p className="assigned-status">This quiz is no longer open.</p>
  }
}
