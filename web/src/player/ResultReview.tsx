import { useEffect, useState } from 'react'
import { api } from '../api/client'
import {
  formatWhen,
  type AttemptResult,
  type ResultQuestion,
} from './model'

/**
 * The released review (docs/08): the score, the per-question grading, and -
 * only here, after release - the answer key. The server refuses this read
 * until the quiz's results are released, so a 409 renders as "not yet",
 * never as an error.
 */
export default function ResultReview({
  attemptId,
  onBack,
}: {
  attemptId: string
  onBack: () => void
}) {
  const [result, setResult] = useState<AttemptResult | null>(null)
  const [notReleased, setNotReleased] = useState(false)
  const [loadError, setLoadError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      const response = await api
        .GET('/api/v1/attempts/{id}/result', {
          params: { path: { id: attemptId } },
        })
        .catch(() => null)
      if (cancelled) return
      if (response?.data) {
        setResult(response.data)
        return
      }
      if (response?.response.status === 409) {
        setNotReleased(true)
        return
      }
      setLoadError(
        response?.error?.message ?? 'Could not load the results. Retry later.',
      )
    })()
    return () => {
      cancelled = true
    }
  }, [attemptId])

  if (loadError) {
    return (
      <div className="review">
        <p className="form-error">{loadError}</p>
        <button className="button button-quiet" type="button" onClick={onBack}>
          Back to my quizzes
        </button>
      </div>
    )
  }

  if (notReleased) {
    return (
      <div className="review">
        <section className="panel player-done">
          <h1 className="card-title">Results not released yet</h1>
          <p className="hint">
            Your teacher has not released the results for this quiz. Check
            back later.
          </p>
          <button
            className="button button-primary"
            type="button"
            onClick={onBack}
          >
            Back to my quizzes
          </button>
        </section>
      </div>
    )
  }

  if (!result) {
    return (
      <p className="boot-note" role="status">
        Loading…
      </p>
    )
  }

  return (
    <div className="review">
      <button className="back-button" type="button" onClick={onBack}>
        ← My quizzes
      </button>

      <header className="page-head">
        <div>
          <p className="eyebrow">Attempt {result.attempt.attempt_no} results</p>
          <h1 className="page-title">{result.quiz_title}</h1>
        </div>
      </header>

      <section className="panel score-banner">
        <span className="score-figure tabular">
          {result.score} <span className="score-max">/ {result.max_score}</span>
        </span>
        <span className="score-caption">
          points
          {result.percentile !== null && (
            <>
              <span className="meta-dot" aria-hidden="true" />
              {formatPercentile(result.percentile)} percentile
            </>
          )}
          <span className="meta-dot" aria-hidden="true" />
          released {formatWhen(result.released_at)}
        </span>
      </section>

      <ol className="review-questions">
        {result.questions.map((question) => (
          <li key={question.id} className="panel review-question">
            <ReviewQuestion question={question} />
          </li>
        ))}
      </ol>
    </div>
  )
}

/** Renders a percentile (0-100) as an ordinal, e.g. 92 -> "92nd". */
function formatPercentile(percentile: number): string {
  const rounded = Math.round(percentile)
  const mod100 = rounded % 100
  const suffix =
    mod100 >= 11 && mod100 <= 13
      ? 'th'
      : (['th', 'st', 'nd', 'rd'][rounded % 10] ?? 'th')
  return `${rounded}${suffix}`
}

function verdictOf(q: ResultQuestion): {
  className: string
  label: string
} {
  if (q.is_correct === null) return { className: 'skipped', label: 'Not answered' }
  if (q.is_correct) return { className: 'correct', label: 'Correct' }
  return { className: 'incorrect', label: 'Incorrect' }
}

/** The key and the response, rendered per question type. */
function ReviewQuestion({ question }: { question: ResultQuestion }) {
  const verdict = verdictOf(question)

  return (
    <div>
      <div className="player-question-head">
        <span className="question-index">{question.position}</span>
        <span className="player-question-text">{question.body.text}</span>
        <span className={`chip chip-verdict chip-verdict-${verdict.className}`}>
          {verdict.label}
        </span>
        <span className="player-question-points tabular">
          {question.points_awarded} / {question.points} pt
          {question.points === 1 ? '' : 's'}
        </span>
      </div>

      {(question.type === 'single' || question.type === 'multi') && (
        <ChoiceReview question={question} />
      )}
      {question.type === 'truefalse' && <TrueFalseReview question={question} />}
      {question.type === 'short' && <ShortReview question={question} />}
    </div>
  )
}

function ChoiceReview({ question }: { question: ResultQuestion }) {
  const correctKeys = new Set(
    question.type === 'single'
      ? typeof question.correct === 'string'
        ? [question.correct]
        : []
      : Array.isArray(question.correct)
        ? question.correct.filter((k): k is string => typeof k === 'string')
        : [],
  )
  const pickedKeys = new Set(
    question.type === 'single'
      ? typeof question.response === 'string'
        ? [question.response]
        : []
      : Array.isArray(question.response)
        ? question.response.filter((k): k is string => typeof k === 'string')
        : [],
  )

  return (
    <div className="option-list">
      {(question.options ?? []).map((option) => {
        const isKey = correctKeys.has(option.key)
        const picked = pickedKeys.has(option.key)
        const tone = isKey
          ? 'review-option-key'
          : picked
            ? 'review-option-wrong'
            : ''
        return (
          <div key={option.key} className={`option-row review-option ${tone}`}>
            <span className="option-key">{option.key.toUpperCase()}</span>
            <span className="option-static">{option.text}</span>
            {picked && <span className="review-tag">Your answer</span>}
            {isKey && (
              <span className="review-tag review-tag-key">Correct answer</span>
            )}
          </div>
        )
      })}
    </div>
  )
}

function TrueFalseReview({ question }: { question: ResultQuestion }) {
  return (
    <div className="option-list">
      {[true, false].map((bool) => {
        const isKey = question.correct === bool
        const picked = question.response === bool
        const tone = isKey
          ? 'review-option-key'
          : picked
            ? 'review-option-wrong'
            : ''
        return (
          <div key={String(bool)} className={`option-row review-option ${tone}`}>
            <span className="option-key">{bool ? 'T' : 'F'}</span>
            <span className="option-static">{bool ? 'True' : 'False'}</span>
            {picked && <span className="review-tag">Your answer</span>}
            {isKey && (
              <span className="review-tag review-tag-key">Correct answer</span>
            )}
          </div>
        )
      })}
    </div>
  )
}

function ShortReview({ question }: { question: ResultQuestion }) {
  const accepted = (question.correct as { accepted?: unknown })?.accepted
  const acceptedList = Array.isArray(accepted)
    ? accepted.filter((a): a is string => typeof a === 'string')
    : []

  return (
    <dl className="short-review">
      <div className="short-review-row">
        <dt>Your answer</dt>
        <dd>
          {typeof question.response === 'string' && question.response !== ''
            ? question.response
            : '(no answer)'}
        </dd>
      </div>
      <div className="short-review-row">
        <dt>Accepted answer{acceptedList.length === 1 ? '' : 's'}</dt>
        <dd>{acceptedList.join(', ')}</dd>
      </div>
    </dl>
  )
}
