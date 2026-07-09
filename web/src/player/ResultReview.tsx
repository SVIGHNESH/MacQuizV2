import { useEffect, useState } from 'react'
import { api } from '../api/client'
import Leaderboard from './Leaderboard'
import {
  formatElapsed,
  formatWhen,
  type AttemptResult,
  type Leaderboard as LeaderboardData,
  type ResultQuestion,
} from './model'

/**
 * The released review (docs/11 St4): the score as the screen's one inverted
 * hero card, then the answer key - exposed here and only here, after release
 * (docs/08). The server refuses this read until results are released, so a
 * 409 renders as the withheld card (St4b), never as an error.
 *
 * The leaderboard (St5) rides the same release gate, so it is fetched
 * alongside the result and simply omitted when it fails - a missing
 * scoreboard must never cost the student their answer key.
 */
export default function ResultReview({
  attemptId,
  onBack,
}: {
  attemptId: string
  onBack: () => void
}) {
  const [result, setResult] = useState<AttemptResult | null>(null)
  const [board, setBoard] = useState<LeaderboardData | null>(null)
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
        const ranked = await api
          .GET('/api/v1/attempts/{id}/leaderboard', {
            params: { path: { id: attemptId } },
          })
          .catch(() => null)
        if (!cancelled && ranked?.data) setBoard(ranked.data)
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
          Back to assigned quizzes
        </button>
      </div>
    )
  }

  // St4b: submitted and graded, score withheld until the teacher releases.
  if (notReleased) {
    return (
      <div className="review-withheld">
        <section className="card withheld-card">
          <span className="chip chip-lifecycle chip-lifecycle-submitted">
            <span className="chip-dot" aria-hidden="true" />
            Submitted
          </span>
          <h1 className="withheld-title">Results not released yet</h1>
          <p className="hint">
            Your attempt was received and graded automatically. Your score is
            withheld until your teacher releases results.
          </p>
          <div className="withheld-score" aria-label="Score not released">
            <span className="withheld-score-mask" aria-hidden="true">
              ••&nbsp;%
            </span>
            <span className="withheld-score-label">not released</span>
          </div>
          <button
            className="button button-quiet withheld-action"
            type="button"
            onClick={onBack}
          >
            Back to assigned quizzes
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

  const correctCount = result.questions.filter((q) => q.is_correct).length
  const percent =
    result.max_score > 0
      ? Math.round((result.score / result.max_score) * 100)
      : null
  // The clock the student actually raced: start to submit, against the budget
  // the server pinned at start (least(duration, quiz end)).
  const timeTaken = result.attempt.submitted_at
    ? formatElapsed(result.attempt.started_at, result.attempt.submitted_at)
    : null
  const budget = formatElapsed(
    result.attempt.started_at,
    result.attempt.deadline_at,
  )
  const myRank = board?.entries.find((entry) => entry.is_self) ?? null

  return (
    <div className="review">
      <header className="review-head">
        <button className="back-button" type="button" onClick={onBack}>
          ← Assigned quizzes
        </button>
        <h1 className="page-title">{result.quiz_title} - your result</h1>
        <p className="review-subtitle">
          Attempt {result.attempt.attempt_no} · released{' '}
          {formatWhen(result.released_at)}
        </p>
      </header>

      {result.score_overridden && (
        <p className="quiz-banner review-override" role="status">
          This attempt was scored zero after your removal from the quiz.
        </p>
      )}

      {/* At most one inverted ink card per screen: the hero number. */}
      <div className="stat-cards">
        <div className="stat-card stat-card-hero">
          <span className="stat-card-eyebrow">Score</span>
          <span className="stat-card-hero-value tabular score-figure">
            {percent === null ? `${result.score}` : `${percent}%`}
          </span>
          <span className="stat-card-hero-sub tabular">
            {correctCount} of {result.questions.length} correct
          </span>
        </div>
        <div className="stat-card">
          <span className="stat-card-value tabular">
            {result.score} / {result.max_score}
          </span>
          <span className="stat-card-label">Points</span>
        </div>
        {timeTaken && (
          <div className="stat-card">
            <span className="stat-card-value tabular">{timeTaken}</span>
            <span className="stat-card-label">Time taken · of {budget}</span>
          </div>
        )}
        {myRank && board && (
          <div className="stat-card">
            <span className="stat-card-value tabular">
              {formatOrdinal(myRank.rank)}
            </span>
            <span className="stat-card-label">Rank · of {board.total}</span>
          </div>
        )}
        {result.percentile !== null && (
          <div className="stat-card">
            <span className="stat-card-value tabular">
              {formatOrdinal(result.percentile)}
            </span>
            <span className="stat-card-label">Percentile · in this quiz</span>
          </div>
        )}
      </div>

      {board && board.entries.length > 1 && (
        <Leaderboard
          quizTitle={board.quiz_title}
          entries={board.entries}
          total={board.total}
        />
      )}

      <section className="answer-key">
        <span className="eyebrow answer-key-eyebrow">Answer key</span>
        <div className="answer-key-table">
          <div className="answer-key-header">
            <span>#</span>
            <span>Question</span>
            <span>Your answer</span>
            <span className="answer-key-verdict-head">Verdict</span>
          </div>
          {result.questions.map((question) => (
            <AnswerRow key={question.id} question={question} />
          ))}
        </div>
      </section>
    </div>
  )
}

/** Renders a percentile or a rank as an ordinal, e.g. 92 -> "92nd". */
function formatOrdinal(value: number): string {
  const rounded = Math.round(value)
  const mod100 = rounded % 100
  const suffix =
    mod100 >= 11 && mod100 <= 13
      ? 'th'
      : (['th', 'st', 'nd', 'rd'][rounded % 10] ?? 'th')
  return `${rounded}${suffix}`
}

function keysOf(raw: unknown): string[] {
  if (typeof raw === 'string') return [raw]
  if (Array.isArray(raw)) return raw.filter((k): k is string => typeof k === 'string')
  return []
}

/**
 * "B · Encrypted vault" - the option letter, then what it said. The table
 * never lists the options themselves, so a bare letter would be unreadable;
 * every key carries its text, including each key of a multi.
 */
function labelForKeys(question: ResultQuestion, keys: string[]): string {
  return keys
    .map((key) => {
      const option = (question.options ?? []).find((o) => o.key === key)
      const letter = key.toUpperCase()
      return option ? `${letter} · ${option.text}` : letter
    })
    .join(', ')
}

/** What the student put down, in the shape their question type takes. */
function yourAnswer(question: ResultQuestion): string {
  switch (question.type) {
    case 'single':
    case 'multi': {
      const keys = keysOf(question.response)
      return keys.length ? labelForKeys(question, keys) : ''
    }
    case 'truefalse':
      return typeof question.response === 'boolean'
        ? question.response
          ? 'True'
          : 'False'
        : ''
    case 'short':
      return typeof question.response === 'string' ? question.response.trim() : ''
  }
}

/** The key, shown only where the student did not already find it. */
function correctAnswer(question: ResultQuestion): string {
  switch (question.type) {
    case 'single':
    case 'multi':
      return labelForKeys(question, keysOf(question.correct))
    case 'truefalse':
      return question.correct === true
        ? 'True'
        : question.correct === false
          ? 'False'
          : ''
    case 'short': {
      const accepted = (question.correct as { accepted?: unknown })?.accepted
      return keysOf(accepted).join(', ')
    }
  }
}

function AnswerRow({ question }: { question: ResultQuestion }) {
  const answered = yourAnswer(question)
  const verdict =
    question.is_correct === null
      ? { tone: 'skipped', label: 'Not answered' }
      : question.is_correct
        ? { tone: 'correct', label: 'Correct' }
        : { tone: 'incorrect', label: 'Incorrect' }
  const key = question.is_correct ? '' : correctAnswer(question)
  const keyLabel =
    question.type === 'short'
      ? 'Accepted'
      : question.type === 'multi'
        ? 'Correct answers'
        : 'Correct answer'

  return (
    <div className="answer-row">
      <span className="answer-row-num tabular">
        {String(question.position).padStart(2, '0')}
      </span>
      <div className="answer-row-question">
        <span className="answer-row-text">{question.body.text}</span>
        {key && (
          <span className="answer-row-key">
            {keyLabel}: {key}
          </span>
        )}
      </div>
      <span className="answer-row-response">
        {answered || <span className="answer-row-blank">—</span>}
      </span>
      <span className={`answer-verdict answer-verdict-${verdict.tone}`}>
        <span className="answer-verdict-dot" aria-hidden="true" />
        {verdict.label}
      </span>
    </div>
  )
}
