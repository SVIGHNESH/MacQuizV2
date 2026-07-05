import { useCallback, useEffect, useMemo, useState } from 'react'
import { api } from '../api/client'
import {
  STATUS_LABEL,
  TYPE_LABEL,
  newQuestionInput,
  type Quiz,
  type QuestionType,
  type TeacherQuestion,
} from './model'
import QuestionCard from './QuestionCard'
import { useAutosave, type SaveResult, type SaveState } from './useAutosave'

const QUESTION_TYPES: QuestionType[] = ['single', 'multi', 'truefalse', 'short']

export default function QuizEditor({
  quizId,
  onBack,
}: {
  quizId: string
  onBack: () => void
}) {
  const [loaded, setLoaded] = useState<{
    quiz: Quiz
    questions: TeacherQuestion[]
  } | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      const result = await api
        .GET('/api/v1/quizzes/{id}', { params: { path: { id: quizId } } })
        .catch(() => null)
      if (cancelled) return
      if (!result?.data) {
        setLoadError(
          result?.error?.message ?? 'Could not load this quiz. Try again.',
        )
        return
      }
      setLoaded({ quiz: result.data.quiz, questions: result.data.questions })
    })()
    return () => {
      cancelled = true
    }
  }, [quizId])

  if (loadError) {
    return (
      <div className="editor">
        <button className="button button-quiet back-button" onClick={onBack}>
          ← All quizzes
        </button>
        <p className="form-error">{loadError}</p>
      </div>
    )
  }

  if (!loaded) {
    return (
      <p className="boot-note" role="status">
        Loading…
      </p>
    )
  }

  return (
    <LoadedEditor
      quiz={loaded.quiz}
      initialQuestions={loaded.questions}
      onBack={onBack}
    />
  )
}

interface QuizSettingsDraft {
  title: string
  maxAttempts: number
  shuffleQuestions: boolean
}

function LoadedEditor({
  quiz,
  initialQuestions,
  onBack,
}: {
  quiz: Quiz
  initialQuestions: TeacherQuestion[]
  onBack: () => void
}) {
  const [questions, setQuestions] = useState(initialQuestions)
  const [settings, setSettings] = useState<QuizSettingsDraft>({
    title: quiz.title,
    maxAttempts: quiz.max_attempts,
    shuffleQuestions: quiz.shuffle_questions,
  })
  const [actionError, setActionError] = useState<string | null>(null)
  const [adding, setAdding] = useState(false)
  const [questionStates, setQuestionStates] = useState<
    Record<string, SaveState>
  >({})

  const saveSettings = useCallback(
    async (value: QuizSettingsDraft): Promise<SaveResult> => {
      if (value.title.trim() === '') {
        return {
          ok: false,
          message: 'The quiz needs a title.',
          fields: { title: 'The quiz needs a title.' },
        }
      }
      if (
        !Number.isInteger(value.maxAttempts) ||
        value.maxAttempts < 1 ||
        value.maxAttempts > 10
      ) {
        return {
          ok: false,
          message: 'Attempts allowed must be between 1 and 10.',
          fields: { max_attempts: 'Must be between 1 and 10.' },
        }
      }
      const result = await api.PATCH('/api/v1/quizzes/{id}', {
        params: { path: { id: quiz.id } },
        body: {
          title: value.title.trim(),
          max_attempts: value.maxAttempts,
          shuffle_questions: value.shuffleQuestions,
        },
      })
      if (result.data) return { ok: true }
      return {
        ok: false,
        message: result.error?.message ?? 'Saving failed.',
        fields: result.error?.fields,
      }
    },
    [quiz.id],
  )

  const settingsState = useAutosave(settings, saveSettings)

  const onQuestionSaveState = useCallback((id: string, state: SaveState) => {
    setQuestionStates((prev) => ({ ...prev, [id]: state }))
  }, [])

  const aggregate = useMemo((): { text: string; tone: string } => {
    const states = [
      settingsState,
      ...questions
        .map((q) => questionStates[q.id])
        .filter((s): s is SaveState => Boolean(s)),
    ]
    if (states.some((s) => s.phase === 'error')) {
      return { text: 'Some changes are not saved', tone: 'bad' }
    }
    if (states.some((s) => s.phase === 'pending' || s.phase === 'saving')) {
      return { text: 'Saving…', tone: 'busy' }
    }
    return { text: 'All changes saved', tone: 'ok' }
  }, [settingsState, questionStates, questions])

  const addQuestion = async (type: QuestionType) => {
    setAdding(true)
    setActionError(null)
    const result = await api
      .POST('/api/v1/quizzes/{id}/questions', {
        params: { path: { id: quiz.id } },
        body: newQuestionInput(type),
      })
      .catch(() => null)
    setAdding(false)
    if (!result?.data) {
      setActionError(result?.error?.message ?? 'Could not add the question.')
      return
    }
    const added = result.data.question
    setQuestions((prev) => [...prev, added])
  }

  const moveQuestion = async (id: string, direction: -1 | 1) => {
    const from = questions.findIndex((q) => q.id === id)
    const to = from + direction
    if (from < 0 || to < 0 || to >= questions.length) return
    const next = [...questions]
    const moved = next[from]!
    next[from] = next[to]!
    next[to] = moved
    setQuestions(next)
    setActionError(null)
    const result = await api
      .PUT('/api/v1/quizzes/{id}/questions/order', {
        params: { path: { id: quiz.id } },
        body: { question_ids: next.map((q) => q.id) },
      })
      .catch(() => null)
    if (!result?.data) {
      // The server order is the truth; put the row back rather than lying.
      setQuestions(questions)
      setActionError(result?.error?.message ?? 'Could not reorder.')
    }
  }

  const deleteQuestion = async (id: string) => {
    const result = await api
      .DELETE('/api/v1/questions/{id}', { params: { path: { id } } })
      .catch(() => null)
    if (result?.response.status !== 204) {
      setActionError(result?.error?.message ?? 'Could not delete the question.')
      return
    }
    setQuestions((prev) => prev.filter((q) => q.id !== id))
    setQuestionStates((prev) => {
      const next = { ...prev }
      delete next[id]
      return next
    })
  }

  const editable = quiz.status === 'draft'
  const settingsFields =
    settingsState.phase === 'error' ? (settingsState.fields ?? {}) : {}

  return (
    <div className="editor">
      <div className="editor-topline">
        <button className="button button-quiet back-button" onClick={onBack}>
          ← All quizzes
        </button>
        <span
          className={`save-badge save-badge-${aggregate.tone} save-indicator`}
          role="status"
        >
          {aggregate.text}
        </span>
      </div>

      <section className="panel">
        <div className="editor-title-row">
          <label className="field editor-title-field">
            <span className="field-label">Quiz title</span>
            <input
              id="quiz-title"
              className="input editor-title-input"
              type="text"
              value={settings.title}
              disabled={!editable}
              onChange={(e) =>
                setSettings({ ...settings, title: e.target.value })
              }
            />
            {settingsFields.title && (
              <p className="field-error">{settingsFields.title}</p>
            )}
          </label>
          <span className={`chip chip-status chip-status-${quiz.status}`}>
            {STATUS_LABEL[quiz.status]}
          </span>
        </div>

        <div className="editor-settings">
          <label className="field">
            <span className="field-label">Attempts allowed</span>
            <input
              id="quiz-max-attempts"
              className="input input-points tabular"
              type="number"
              min={1}
              max={10}
              step={1}
              value={
                Number.isFinite(settings.maxAttempts) ? settings.maxAttempts : ''
              }
              disabled={!editable}
              onChange={(e) =>
                setSettings({
                  ...settings,
                  maxAttempts: e.target.valueAsNumber,
                })
              }
            />
            {settingsFields.max_attempts && (
              <p className="field-error">{settingsFields.max_attempts}</p>
            )}
          </label>
          <label className="field checkbox-field">
            <span className="field-label">Question order</span>
            <span className="checkbox-row">
              <input
                id="quiz-shuffle"
                type="checkbox"
                checked={settings.shuffleQuestions}
                disabled={!editable}
                onChange={(e) =>
                  setSettings({
                    ...settings,
                    shuffleQuestions: e.target.checked,
                  })
                }
              />
              Shuffle questions per student
            </span>
          </label>
        </div>
      </section>

      {!editable && (
        <p className="hint">
          This quiz is {STATUS_LABEL[quiz.status].toLowerCase()}, so its
          content is frozen.
        </p>
      )}

      {actionError && <p className="form-error">{actionError}</p>}

      <div className="question-list">
        {questions.map((question, index) => (
          <QuestionCard
            key={question.id}
            question={question}
            index={index}
            count={questions.length}
            onMove={moveQuestion}
            onDelete={deleteQuestion}
            onSaveState={onQuestionSaveState}
          />
        ))}
      </div>

      {questions.length === 0 && (
        <p className="hint">No questions yet. Add the first one below.</p>
      )}

      {editable && (
        <section className="panel add-question-panel">
          <span className="field-label">Add a question</span>
          <div className="add-question-buttons">
            {QUESTION_TYPES.map((type) => (
              <button
                key={type}
                className="button button-quiet"
                type="button"
                disabled={adding}
                onClick={() => void addQuestion(type)}
              >
                {TYPE_LABEL[type]}
              </button>
            ))}
          </div>
        </section>
      )}
    </div>
  )
}
