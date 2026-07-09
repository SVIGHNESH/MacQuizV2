import { useState } from 'react'
import { api } from '../api/client'
import type { Quiz } from './model'

const RELEASED_FORMAT = new Intl.DateTimeFormat(undefined, {
  day: 'numeric',
  month: 'short',
  hour: '2-digit',
  minute: '2-digit',
})

/**
 * The two lifecycle actions a closed quiz still has (docs/06 section 1):
 * releasing results - the one moment a score may leave the server (docs/08
 * section 3) - and archiving, the terminal read-only retirement path.
 *
 * Deliberately a sibling of QuizStatsPanel rather than a section inside it:
 * that panel renders nothing until the worker's rollup lands, and a teacher
 * must be able to release results before then. A quiz published with the
 * manual policy is otherwise unreachable - its students never see a score.
 */
export default function ResultsReleasePanel({
  quiz,
  onUpdated,
}: {
  quiz: Quiz
  onUpdated: (quiz: Quiz) => void
}) {
  const [busy, setBusy] = useState<'release' | 'archive' | null>(null)
  const [confirmingArchive, setConfirmingArchive] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const released = quiz.results_released_at !== null

  const releaseResults = async () => {
    setBusy('release')
    setError(null)
    const result = await api
      .POST('/api/v1/quizzes/{id}/release-results', {
        params: { path: { id: quiz.id } },
      })
      .catch(() => null)
    setBusy(null)
    if (!result?.data) {
      setError(result?.error?.message ?? 'Could not release the results.')
      return
    }
    onUpdated(result.data.quiz)
  }

  const archiveQuiz = async () => {
    setBusy('archive')
    setError(null)
    const result = await api
      .POST('/api/v1/quizzes/{id}/archive', {
        params: { path: { id: quiz.id } },
      })
      .catch(() => null)
    setBusy(null)
    setConfirmingArchive(false)
    if (!result?.data) {
      setError(result?.error?.message ?? 'Could not archive the quiz.')
      return
    }
    onUpdated(result.data.quiz)
  }

  return (
    <section className="panel release-panel" aria-label="Results and retirement">
      <h2 className="card-title">Results</h2>

      {released ? (
        <p className="field-hint" id="release-state">
          Results released{' '}
          {RELEASED_FORMAT.format(new Date(quiz.results_released_at as string))}.
          Assigned students can now see their score and the answer key.
        </p>
      ) : (
        <p className="field-hint" id="release-state">
          {quiz.release_policy === 'auto'
            ? 'Scores release on their own once grading finishes. Release now to publish them without waiting.'
            : 'Scores stay withheld until you release them. Until then students see their attempt as submitted, with no score.'}
        </p>
      )}

      {error && <p className="form-error">{error}</p>}

      <div className="publish-actions">
        {!released && (
          <button
            id="release-results-button"
            className="button button-primary"
            type="button"
            disabled={busy !== null}
            onClick={() => void releaseResults()}
          >
            {busy === 'release' ? 'Releasing…' : 'Release results'}
          </button>
        )}

        {quiz.status === 'closed' &&
          (confirmingArchive ? (
            <>
              <button
                id="archive-confirm-button"
                className="button button-danger"
                type="button"
                disabled={busy !== null}
                onClick={() => void archiveQuiz()}
              >
                {busy === 'archive' ? 'Archiving…' : 'Archive quiz'}
              </button>
              <button
                className="button button-quiet"
                type="button"
                disabled={busy !== null}
                onClick={() => setConfirmingArchive(false)}
              >
                Keep it open
              </button>
            </>
          ) : (
            <button
              id="archive-button"
              className="button button-quiet-danger"
              type="button"
              disabled={busy !== null}
              onClick={() => setConfirmingArchive(true)}
            >
              Archive
            </button>
          ))}

        {quiz.status === 'archived' && (
          <span className="field-hint">
            Archived - read-only. Analytics are retained.
          </span>
        )}
      </div>

      {confirmingArchive && (
        <p className="field-hint">
          Archiving is permanent. The quiz becomes read-only; its analytics and
          every attempt are kept.
        </p>
      )}
    </section>
  )
}
