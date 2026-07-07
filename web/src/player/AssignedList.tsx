import { useEffect, useState } from 'react'
import { api } from '../api/client'
import {
  ASSIGNED_STATUS_LABEL,
  ATTEMPT_STATUS_LABEL,
  formatDuration,
  formatWhen,
  type AssignedQuiz,
} from './model'

/**
 * The student home: every assigned quiz with its window, the caller's own
 * attempt history, and the one action the quiz's state allows - start,
 * resume, or review. Scores come release-gated from the server; a null
 * score on a finished attempt reads as withheld, never as zero.
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

  return (
    <div className="assigned-list">
      <div className="page-head">
        <div>
          <p className="eyebrow">Student workspace</p>
          <h1 className="page-title">My quizzes</h1>
        </div>
      </div>

      {quizzes.length === 0 ? (
        <section className="panel empty-state">
          <h2 className="card-title">Nothing assigned yet</h2>
          <p className="hint">
            When a teacher assigns you a quiz it appears here with its open
            window and time limit.
          </p>
        </section>
      ) : (
        quizzes.map((quiz) => (
          <AssignedCard
            key={quiz.id}
            quiz={quiz}
            onStart={onStart}
            onResume={onResume}
            onReview={onReview}
          />
        ))
      )}
    </div>
  )
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
  const inProgress = quiz.attempts.find((a) => a.status === 'in_progress')
  const attemptsLeft = quiz.max_attempts - quiz.attempts.length
  const released = quiz.results_released_at !== null

  return (
    <section className="panel assigned-card">
      <header className="assigned-head">
        <h2 className="card-title assigned-title">{quiz.title}</h2>
        <span className={`chip chip-status chip-status-${quiz.status}`}>
          {ASSIGNED_STATUS_LABEL[quiz.status]}
        </span>
      </header>

      <p className="assigned-meta">
        {formatWhen(quiz.starts_at)} – {formatWhen(quiz.ends_at)}
        <span className="meta-dot" aria-hidden="true" />
        {formatDuration(quiz.duration_sec)} limit
        <span className="meta-dot" aria-hidden="true" />
        {quiz.question_count} question{quiz.question_count === 1 ? '' : 's'}
        <span className="meta-dot" aria-hidden="true" />
        {quiz.attempts.length} of {quiz.max_attempts} attempt
        {quiz.max_attempts === 1 ? '' : 's'} used
      </p>

      {quiz.attempts.length > 0 && (
        <ul className="attempt-list">
          {quiz.attempts.map((attempt) => (
            <li key={attempt.id} className="attempt-row">
              <span className="attempt-no">Attempt {attempt.attempt_no}</span>
              <span
                className={`chip chip-attempt chip-attempt-${attempt.status}`}
              >
                {ATTEMPT_STATUS_LABEL[attempt.status]}
              </span>
              <span className="attempt-when">
                {attempt.submitted_at
                  ? `Submitted ${formatWhen(attempt.submitted_at)}`
                  : `Started ${formatWhen(attempt.started_at)}`}
              </span>
              <span className="attempt-score">
                {attempt.score !== null
                  ? `${attempt.score} pts`
                  : attempt.status === 'in_progress'
                    ? ''
                    : 'Score withheld'}
              </span>
              {released && attempt.status === 'graded' && (
                <button
                  className="button button-small button-quiet"
                  type="button"
                  onClick={() => onReview(attempt.id)}
                >
                  Review
                </button>
              )}
            </li>
          ))}
        </ul>
      )}

      <div className="assigned-actions">
        {quiz.status === 'scheduled' && (
          <p className="assigned-note">Opens {formatWhen(quiz.starts_at)}.</p>
        )}
        {quiz.status === 'live' &&
          (inProgress ? (
            <button
              className="button button-primary"
              type="button"
              onClick={() => onResume(inProgress.id)}
            >
              Resume attempt
            </button>
          ) : attemptsLeft > 0 ? (
            <button
              className="button button-primary"
              type="button"
              onClick={() => onStart(quiz.id)}
            >
              {quiz.attempts.length === 0 ? 'Start quiz' : 'Start new attempt'}
            </button>
          ) : (
            <p className="assigned-note">No attempts left.</p>
          ))}
        {quiz.status === 'closed' && !released && (
          <p className="assigned-note">
            Closed. Results have not been released yet.
          </p>
        )}
      </div>
    </section>
  )
}
