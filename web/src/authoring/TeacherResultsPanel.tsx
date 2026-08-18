import { useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { api } from '../api/client'
import Avatar from '../components/Avatar'
import type { components } from '../api/schema'
import {
  correctAnswerLabel,
  correctAnswerText,
  keysOf,
  responseText,
} from '../lib/answers'
import { formatElapsed, formatRemaining, formatWhen } from '../player/model'

type ResultRow = components['schemas']['ResultRow']
type AttemptReview = components['schemas']['AttemptReview']

/**
 * The owner's per-student results table (GET /quizzes/:id/results) - the
 * course-feedback ask: which questions each student answered, which they
 * skipped, and what they put down. Every graded row drills into the
 * per-question review drawer (GET /attempts/:id/review), and the CSV
 * download lives here because this table is what it exports - available
 * from grading onward, unlike the analytics panel, which waits for the
 * rollup.
 */
export default function TeacherResultsPanel({ quizId }: { quizId: string }) {
  const [rows, setRows] = useState<ResultRow[] | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [reviewAttemptId, setReviewAttemptId] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      const result = await api
        .GET('/api/v1/quizzes/{id}/results', { params: { path: { id: quizId } } })
        .catch(() => null)
      if (cancelled) return
      if (!result?.data) {
        setLoadError(result?.error?.message ?? 'Could not load the results table.')
        return
      }
      setRows(result.data.results)
    })()
    return () => {
      cancelled = true
    }
  }, [quizId])

  return (
    <section className="panel results-panel" aria-label="Student results">
      <div className="results-panel-head">
        <div>
          <span className="card-title">Student results</span>
          <p className="hint results-panel-sub">
            One row per assigned student and attempt. Review opens the
            question-by-question detail.
          </p>
        </div>
        <a
          className="button button-quiet"
          href={`/api/v1/quizzes/${quizId}/results.csv`}
          download
        >
          Download results CSV
        </a>
      </div>

      {loadError && <p className="form-error">{loadError}</p>}
      {rows === null && !loadError && (
        <p className="boot-note" role="status">
          Loading results…
        </p>
      )}

      {rows !== null && rows.length === 0 && (
        <p className="hint">No students are assigned to this quiz.</p>
      )}

      {rows !== null && rows.length > 0 && (
        <div className="quiz-table results-table" role="table" aria-label="Results by student">
          <div className="qt-head" role="row">
            <span>Student</span>
            <span>Attempt</span>
            <span>Status</span>
            <span className="qt-num">Score</span>
            <span className="qt-num">Submitted</span>
            <span></span>
          </div>
          {rows.map((row) => {
            const state = rowState(row)
            const percent =
              row.score !== null && row.max_score !== null && row.max_score > 0
                ? Math.min(100, Math.max(0, (row.score / row.max_score) * 100))
                : null
            return (
              <div className="qt-row" role="row" key={`${row.student_id}-${row.attempt_no ?? 0}`}>
                <span className="results-name" title={row.email}>
                  <Avatar
                    userId={row.student_id}
                    fullName={row.full_name}
                    avatar={row.avatar}
                    size="small"
                  />
                  <span className="results-name-text">
                    <span className="results-name-full">{row.full_name}</span>
                    <span className="results-name-email">{row.email}</span>
                  </span>
                </span>
                <span className="tabular">
                  {row.attempt_no === null ? '—' : row.attempt_no}
                </span>
                <span className={`chip chip-roster-${state}`}>{STATE_LABEL[state]}</span>
                <span className="qt-num results-score">
                  {row.score === null ? (
                    <span className="tabular">—</span>
                  ) : (
                    <>
                      <span className="tabular">
                        {row.score}
                        <span className="results-score-denom">
                          {' '}
                          / {row.max_score ?? '?'}
                        </span>
                      </span>
                      {percent !== null && (
                        <span className="results-score-bar" aria-hidden="true">
                          <i style={{ width: `${percent}%` }} />
                        </span>
                      )}
                    </>
                  )}
                  {row.score_overridden && (
                    <span className="hint results-score-note">overridden to 0</span>
                  )}
                </span>
                <span className="qt-num qt-date tabular">
                  {row.submitted_at === null ? '—' : formatWhen(row.submitted_at)}
                </span>
                <span className="qt-actions">
                  {row.status === 'graded' && row.attempt_id && (
                    <button
                      className="button button-quiet button-small"
                      type="button"
                      onClick={() => setReviewAttemptId(row.attempt_id!)}
                    >
                      Review
                    </button>
                  )}
                </span>
              </div>
            )
          })}
        </div>
      )}

      {reviewAttemptId && (
        <ReviewDrawer
          attemptId={reviewAttemptId}
          onClose={() => setReviewAttemptId(null)}
        />
      )}
    </section>
  )
}

/**
 * The table's status vocabulary. A kicked attempt reads "Kicked" even after
 * grading flips its status to graded - submit_kind carries the fact - so the
 * teacher never loses sight of the removal.
 */
type RowState = 'not_started' | 'in_progress' | 'submitted' | 'graded' | 'kicked'

function rowState(row: ResultRow): RowState {
  if (row.attempt_id === null || row.status === null) return 'not_started'
  if (row.submit_kind === 'kicked' || row.status === 'kicked') return 'kicked'
  return row.status
}

const STATE_LABEL: Record<RowState, string> = {
  not_started: 'Not started',
  in_progress: 'In progress',
  submitted: 'Submitted',
  graded: 'Graded',
  kicked: 'Kicked',
}

/**
 * The per-question drill-down: one student's graded attempt, question by
 * question, in the order the player showed them. Skipped questions render
 * neutral, never red - a skip is not evidence of weakness (docs/07
 * section 3).
 */
function ReviewDrawer({
  attemptId,
  onClose,
}: {
  attemptId: string
  onClose: () => void
}) {
  const [review, setReview] = useState<AttemptReview | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const closeRef = useRef<HTMLButtonElement>(null)

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      const result = await api
        .GET('/api/v1/attempts/{id}/review', { params: { path: { id: attemptId } } })
        .catch(() => null)
      if (cancelled) return
      if (!result?.data) {
        setLoadError(
          result?.response.status === 409
            ? 'This attempt has not finished grading yet. Try again shortly.'
            : (result?.error?.message ?? 'Could not load the attempt review.'),
        )
        return
      }
      setReview(result.data)
    })()
    return () => {
      cancelled = true
    }
  }, [attemptId])

  useEffect(() => {
    closeRef.current?.focus()
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onClose()
    }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [onClose])

  const answeredCount =
    review?.questions.filter((q) => q.response !== null && q.response !== undefined)
      .length ?? 0
  const duration =
    review?.attempt.submitted_at != null
      ? formatElapsed(review.attempt.started_at, review.attempt.submitted_at)
      : null

  // Portaled to <body>: the panel's entrance animation retains a transform,
  // which would otherwise turn the panel into the containing block for this
  // fixed-position drawer and pin it inside the table card.
  return createPortal(
    <>
      <div className="review-drawer-overlay" onClick={onClose} aria-hidden="true" />
      <aside
        className="review-drawer"
        role="dialog"
        aria-modal="true"
        aria-label="Attempt review"
      >
        <header className="review-drawer-head">
          {review ? (
            <div className="review-drawer-student">
              <Avatar
                userId={review.student_id}
                fullName={review.full_name}
                avatar={review.avatar}
              />
              <div>
                <span className="card-title">
                  {review.full_name} · Attempt {review.attempt.attempt_no}
                </span>
                <span className="review-drawer-email">{review.email}</span>
              </div>
            </div>
          ) : (
            <span className="card-title">Attempt review</span>
          )}
          <button
            ref={closeRef}
            className="button button-quiet button-small review-drawer-close"
            type="button"
            onClick={onClose}
          >
            Close
          </button>
        </header>

        {loadError && <p className="form-error review-drawer-body">{loadError}</p>}
        {!review && !loadError && (
          <p className="boot-note review-drawer-body" role="status">
            Loading review…
          </p>
        )}

        {review && (
          <>
            <div className="review-drawer-meta tabular">
              <div>
                <span className="stat-tile-label">Score</span>
                <span className="review-meta-value">
                  {review.score} / {review.max_score}
                  {review.score_overridden ? ' (overridden to 0)' : ''}
                </span>
              </div>
              <div>
                <span className="stat-tile-label">Answered</span>
                <span className="review-meta-value">
                  {answeredCount} / {review.questions.length}
                </span>
              </div>
              {duration && (
                <div>
                  <span className="stat-tile-label">Duration</span>
                  <span className="review-meta-value">{duration}</span>
                </div>
              )}
              <div>
                <span className="stat-tile-label">Violations</span>
                <span className="review-meta-value">
                  {review.attempt.violation_count}
                </span>
              </div>
            </div>

            <div className="review-drawer-body">
              <p className="hint">
                Questions are in the order the student saw them.
                {review.released_at === null
                  ? ' Results are not released to students yet.'
                  : ''}
              </p>
              {review.questions.map((question, index) => (
                <ReviewQuestion key={question.id} question={question} index={index} />
              ))}
            </div>
          </>
        )}
      </aside>
    </>,
    document.body,
  )
}

type ReviewedQuestion = AttemptReview['questions'][number]

function ReviewQuestion({
  question,
  index,
}: {
  question: ReviewedQuestion
  index: number
}) {
  const verdict =
    question.is_correct === null
      ? { tone: 'skipped', label: 'Skipped' }
      : question.is_correct
        ? { tone: 'correct', label: 'Correct' }
        : { tone: 'incorrect', label: 'Incorrect' }
  const hasOptionList =
    (question.type === 'single' || question.type === 'multi') &&
    (question.options?.length ?? 0) > 0
  const pickedKeys = keysOf(question.response)
  const correctKeys = keysOf(question.correct)
  const studentAnswer = responseText(question)

  return (
    <article className="review-q">
      <header className="review-q-head">
        <span className="review-q-no tabular">Q{index + 1}</span>
        <span className="review-q-text">{question.body.text}</span>
        <span className={`review-verdict review-verdict-${verdict.tone}`}>
          {verdict.label}
        </span>
      </header>

      {hasOptionList ? (
        <div className="review-opts">
          {question.options!.map((option) => {
            const picked = pickedKeys.includes(option.key)
            const isKey = correctKeys.includes(option.key)
            return (
              <div
                key={option.key}
                className={`review-opt${picked ? ' review-opt-picked' : ''}`}
              >
                <span className="review-opt-letter">{option.key.toUpperCase()}</span>
                <span className="review-opt-text">{option.text}</span>
                {isKey && (
                  <span className="review-opt-tag review-opt-tag-key">
                    Correct answer
                  </span>
                )}
                {picked && !isKey && (
                  <span className="review-opt-tag review-opt-tag-pick">
                    Student's pick
                  </span>
                )}
              </div>
            )
          })}
        </div>
      ) : (
        <div className="review-freeform">
          <p>
            <span className="review-freeform-label">Student's answer</span>
            {studentAnswer !== '' ? (
              studentAnswer
            ) : (
              <span className="hint">No answer saved.</span>
            )}
          </p>
          <p>
            <span className="review-freeform-label">
              {correctAnswerLabel(question)}
            </span>
            {correctAnswerText(question)}
          </p>
        </div>
      )}

      <footer className="review-q-foot tabular">
        <span>
          Points {question.points_awarded} / {question.points}
        </span>
        {question.time_spent_ms !== null && (
          <span>Time {formatRemaining(question.time_spent_ms)}</span>
        )}
      </footer>
    </article>
  )
}
