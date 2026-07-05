import { useEffect, useState, type FormEvent } from 'react'
import { api } from '../api/client'
import { STATUS_LABEL, type Quiz } from './model'

const DATE_FORMAT = new Intl.DateTimeFormat(undefined, {
  day: 'numeric',
  month: 'short',
  year: 'numeric',
})

export default function QuizList({
  onOpen,
}: {
  onOpen: (quizId: string) => void
}) {
  const [quizzes, setQuizzes] = useState<Quiz[] | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [creating, setCreating] = useState(false)
  const [newTitle, setNewTitle] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [createError, setCreateError] = useState<string | null>(null)
  const [confirmingDelete, setConfirmingDelete] = useState<string | null>(null)
  const [actionError, setActionError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      const result = await api.GET('/api/v1/quizzes').catch(() => null)
      if (cancelled) return
      if (!result?.data) {
        setLoadError(
          result?.error?.message ?? 'Could not load your quizzes. Reload to retry.',
        )
        return
      }
      setQuizzes(result.data.quizzes)
    })()
    return () => {
      cancelled = true
    }
  }, [])

  const createQuiz = async (event: FormEvent) => {
    event.preventDefault()
    const title = newTitle.trim()
    if (title === '') {
      setCreateError('Give the quiz a title first.')
      return
    }
    setSubmitting(true)
    setCreateError(null)
    const result = await api
      .POST('/api/v1/quizzes', { body: { title } })
      .catch(() => null)
    setSubmitting(false)
    if (!result?.data) {
      setCreateError(result?.error?.message ?? 'Could not create the quiz.')
      return
    }
    onOpen(result.data.quiz.id)
  }

  const deleteQuiz = async (id: string) => {
    setActionError(null)
    const result = await api
      .DELETE('/api/v1/quizzes/{id}', { params: { path: { id } } })
      .catch(() => null)
    setConfirmingDelete(null)
    if (result?.response.status !== 204) {
      setActionError(result?.error?.message ?? 'Could not delete the quiz.')
      return
    }
    setQuizzes((prev) => (prev ?? []).filter((q) => q.id !== id))
  }

  if (loadError) return <p className="form-error">{loadError}</p>
  if (!quizzes) {
    return (
      <p className="boot-note" role="status">
        Loading…
      </p>
    )
  }

  return (
    <div className="quiz-list">
      <div className="page-head">
        <div>
          <p className="eyebrow">Teacher workspace</p>
          <h1 className="page-title">Quizzes</h1>
        </div>
        {!creating && (
          <button
            className="button button-primary"
            type="button"
            onClick={() => setCreating(true)}
          >
            New quiz
          </button>
        )}
      </div>

      {creating && (
        <form className="panel create-form" onSubmit={createQuiz}>
          <label className="field create-field">
            <span className="field-label">Quiz title</span>
            <input
              id="new-quiz-title"
              className="input"
              type="text"
              placeholder="For example: Photosynthesis basics"
              value={newTitle}
              autoFocus
              onChange={(e) => setNewTitle(e.target.value)}
            />
          </label>
          <div className="create-actions">
            <button
              className="button button-primary"
              type="submit"
              disabled={submitting}
            >
              {submitting ? 'Creating…' : 'Create draft'}
            </button>
            <button
              className="button button-quiet"
              type="button"
              disabled={submitting}
              onClick={() => {
                setCreating(false)
                setNewTitle('')
                setCreateError(null)
              }}
            >
              Cancel
            </button>
          </div>
          {createError && <p className="field-error">{createError}</p>}
        </form>
      )}

      {actionError && <p className="form-error">{actionError}</p>}

      {quizzes.length === 0 ? (
        <section className="panel empty-state">
          <h2 className="card-title">No quizzes yet</h2>
          <p className="hint">
            Create your first quiz and start adding questions. Drafts stay
            private until you publish them.
          </p>
        </section>
      ) : (
        <section className="panel table-panel">
          <div className="quiz-table" role="table" aria-label="Your quizzes">
            <div className="qt-head" role="row">
              <span role="columnheader">Title</span>
              <span role="columnheader">Status</span>
              <span role="columnheader" className="qt-num">
                Questions
              </span>
              <span role="columnheader">Created</span>
              <span role="columnheader" aria-label="Actions" />
            </div>
            {quizzes.map((quiz) => (
              <div key={quiz.id} className="qt-row" role="row">
                <button
                  className="qt-title"
                  type="button"
                  onClick={() => onOpen(quiz.id)}
                >
                  {quiz.title}
                </button>
                <span>
                  <span className={`chip chip-status chip-status-${quiz.status}`}>
                    {STATUS_LABEL[quiz.status]}
                  </span>
                </span>
                <span className="qt-num tabular">{quiz.question_count}</span>
                <span className="qt-date">
                  {DATE_FORMAT.format(new Date(quiz.created_at))}
                </span>
                <span className="qt-actions">
                  {quiz.status === 'draft' &&
                    (confirmingDelete === quiz.id ? (
                      <>
                        <button
                          className="button button-small button-danger"
                          type="button"
                          onClick={() => void deleteQuiz(quiz.id)}
                        >
                          Delete draft
                        </button>
                        <button
                          className="button button-small button-quiet"
                          type="button"
                          onClick={() => setConfirmingDelete(null)}
                        >
                          Keep
                        </button>
                      </>
                    ) : (
                      <button
                        className="button button-small button-quiet-danger"
                        type="button"
                        onClick={() => setConfirmingDelete(quiz.id)}
                      >
                        Delete
                      </button>
                    ))}
                </span>
              </div>
            ))}
          </div>
        </section>
      )}
    </div>
  )
}
