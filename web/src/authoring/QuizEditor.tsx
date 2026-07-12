import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api } from '../api/client'
import {
  STATUS_LABEL,
  TYPE_LABEL,
  newQuestionInput,
  type Quiz,
  type QuestionType,
  type TeacherQuestion,
} from './model'
import AudienceEditor, { type AudienceHandle } from './AudienceEditor'
import ImportPanel from './ImportPanel'
import LiveMonitorPanel from './LiveMonitorPanel'
import PreviewModal from './PreviewModal'
import QuestionCard from './QuestionCard'
import QuizStatsPanel from './QuizStatsPanel'
import ResultsReleasePanel from './ResultsReleasePanel'
import ScheduleSection from './ScheduleSection'
import { useAutosave, type SaveResult, type SaveState } from './useAutosave'

const QUESTION_TYPES: QuestionType[] = ['single', 'multi', 'truefalse', 'short']

type WizardStep = 1 | 2 | 3
const WIZARD_STEPS: { n: WizardStep; label: string }[] = [
  { n: 1, label: 'Questions' },
  { n: 2, label: 'Audience' },
  { n: 3, label: 'Schedule' },
]

// A published quiz's page splits into views: the frozen settings/questions,
// the live scoreboard (while live), and results + analytics (once closed).
type EditorTab = 'settings' | 'monitor' | 'results'

/**
 * The authoring wizard's step header: a linear process, so each step is a
 * plain button marked aria-current="step" when active - not a tablist, which
 * would imply interchangeable panels and bring roving-focus expectations that
 * fight the Back/Next buttons. Every step is already persisted, so the header
 * is a free jump, not a gated stepper.
 */
function WizardSteps({
  step,
  onStep,
}: {
  step: WizardStep
  onStep: (n: WizardStep) => void
}) {
  return (
    <nav className="wizard-steps" aria-label="Quiz setup steps">
      {WIZARD_STEPS.map(({ n, label }) => (
        <button
          key={n}
          type="button"
          className="wizard-step-button"
          aria-current={step === n ? 'step' : undefined}
          onClick={() => onStep(n)}
        >
          <span className="wizard-step-index tabular">{n}</span>
          {label}
        </button>
      ))}
    </nav>
  )
}

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
  /** Marks a question earns when it doesn't set its own. */
  defaultPoints: number
  /** Marks a wrong answer costs quiz-wide; 0 disables negative marking. */
  defaultPenalty: number
}

function LoadedEditor({
  quiz: initialQuiz,
  initialQuestions,
  onBack,
}: {
  quiz: Quiz
  initialQuestions: TeacherQuestion[]
  onBack: () => void
}) {
  const [quiz, setQuiz] = useState(initialQuiz)
  const [questions, setQuestions] = useState(initialQuestions)
  const [settings, setSettings] = useState<QuizSettingsDraft>({
    title: quiz.title,
    maxAttempts: quiz.max_attempts,
    shuffleQuestions: quiz.shuffle_questions,
    defaultPoints: quiz.default_points,
    defaultPenalty: quiz.default_penalty,
  })
  const [actionError, setActionError] = useState<string | null>(null)
  const [adding, setAdding] = useState(false)
  const [previewing, setPreviewing] = useState(false)
  const [questionStates, setQuestionStates] = useState<
    Record<string, SaveState>
  >({})
  const [step, setStep] = useState<WizardStep>(1)
  // docs/05 section 2's window banners ("Banner to teacher and all in-progress
  // students"). They live at the shell rather than inside the live monitor that
  // raises them, so the "this quiz has been closed" line is still on screen
  // after the close swaps that tab out for results - which is the moment it
  // explains.
  const [notices, setNotices] = useState<{ id: number; text: string }[]>([])
  const noticeSeq = useRef(0)
  // Stable: it is a dependency of the monitor's socket effect, and a new
  // identity each render would tear the WebSocket down and rebuild it.
  const pushNotice = useCallback((text: string) => {
    setNotices((prev) => [...prev, { id: ++noticeSeq.current, text }])
  }, [])
  // null = "no explicit pick": the page opens on what the status makes
  // interesting (live scores while live, results once closed) and follows a
  // live->closed transition on its own until the teacher picks a tab.
  const [tab, setTab] = useState<EditorTab | null>(null)
  // Lifted out of the schedule step so the audience count survives that panel
  // being hidden, and feeds its "freezes N questions for M students" hint.
  const [audienceCount, setAudienceCount] = useState(0)
  const [audienceGateError, setAudienceGateError] = useState<string | null>(
    null,
  )
  const audienceRef = useRef<AudienceHandle>(null)

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
      if (
        !Number.isFinite(value.defaultPoints) ||
        value.defaultPoints <= 0 ||
        value.defaultPoints > 1000
      ) {
        return {
          ok: false,
          message: 'Default marks must be greater than zero.',
          fields: { default_points: 'Must be greater than zero and at most 1000.' },
        }
      }
      if (
        !Number.isFinite(value.defaultPenalty) ||
        value.defaultPenalty < 0 ||
        value.defaultPenalty > 1000
      ) {
        return {
          ok: false,
          message: 'Negative marks must be zero or more.',
          fields: { default_penalty: 'Must be between 0 and 1000.' },
        }
      }
      const result = await api.PATCH('/api/v1/quizzes/{id}', {
        params: { path: { id: quiz.id } },
        body: {
          title: value.title.trim(),
          max_attempts: value.maxAttempts,
          shuffle_questions: value.shuffleQuestions,
          default_points: value.defaultPoints,
          default_penalty: value.defaultPenalty,
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

  const onQuestionSaved = useCallback((saved: TeacherQuestion) => {
    setQuestions((prev) =>
      prev.map((q) => (q.id === saved.id ? saved : q)),
    )
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
  const wizard = quiz.status === 'draft' || quiz.status === 'scheduled'
  const terminal = quiz.status === 'closed' || quiz.status === 'archived'
  // An explicit pick can go stale (the monitor tab dies when the quiz
  // closes), so it only holds while its tab still exists.
  const defaultTab: EditorTab =
    quiz.status === 'live' ? 'monitor' : terminal ? 'results' : 'settings'
  const activeTab: EditorTab =
    tab === 'monitor' && quiz.status !== 'live' ? defaultTab : (tab ?? defaultTab)
  const settingsFields =
    settingsState.phase === 'error' ? (settingsState.fields ?? {}) : {}

  // Leaving the audience step forward saves any pending picks and refuses to
  // advance with nobody assigned; the header still allows a free jump, where
  // the schedule step's own publish precondition is the backstop.
  const goNext = async () => {
    setAudienceGateError(null)
    if (step === 2) {
      const count = await audienceRef.current?.commit()
      if (count == null) return // save failed; the panel shows why
      if (count === 0) {
        setAudienceGateError('Assign at least one student before scheduling.')
        return
      }
    }
    setStep((s) => Math.min(3, s + 1) as WizardStep)
  }

  const titleField = (
    <label className="field editor-title-field">
      <span className="field-label">Quiz title</span>
      <input
        id="quiz-title"
        className="input editor-title-input"
        type="text"
        value={settings.title}
        disabled={!editable}
        onChange={(e) => setSettings({ ...settings, title: e.target.value })}
      />
      {settingsFields.title && (
        <p className="field-error">{settingsFields.title}</p>
      )}
    </label>
  )

  const attemptsAndShuffle = (
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
            setSettings({ ...settings, maxAttempts: e.target.valueAsNumber })
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
              setSettings({ ...settings, shuffleQuestions: e.target.checked })
            }
          />
          Shuffle questions per student
        </span>
      </label>
      <label className="field">
        <span className="field-label">Marks per question</span>
        <input
          id="quiz-default-points"
          className="input input-points tabular"
          type="number"
          min={0.5}
          max={1000}
          step={0.5}
          value={
            Number.isFinite(settings.defaultPoints) ? settings.defaultPoints : ''
          }
          disabled={!editable}
          onChange={(e) =>
            setSettings({ ...settings, defaultPoints: e.target.valueAsNumber })
          }
        />
        {settingsFields.default_points ? (
          <p className="field-error">{settingsFields.default_points}</p>
        ) : (
          <p className="field-hint">
            Questions without their own marks use this.
          </p>
        )}
      </label>
      <label className="field">
        <span className="field-label">Negative marks per wrong answer</span>
        <input
          id="quiz-default-penalty"
          className="input input-points tabular"
          type="number"
          min={0}
          max={1000}
          step={0.5}
          value={
            Number.isFinite(settings.defaultPenalty) ? settings.defaultPenalty : ''
          }
          disabled={!editable}
          onChange={(e) =>
            setSettings({ ...settings, defaultPenalty: e.target.valueAsNumber })
          }
        />
        {settingsFields.default_penalty ? (
          <p className="field-error">{settingsFields.default_penalty}</p>
        ) : (
          <p className="field-hint">
            0 disables negative marking. Skipped questions are never penalized,
            and a total never drops below zero.
          </p>
        )}
      </label>
    </div>
  )

  const frozenHint = !editable && (
    <p className="hint">
      This quiz is {STATUS_LABEL[quiz.status].toLowerCase()}, so its content is
      frozen.
    </p>
  )

  const questionsBlock = (
    <>
      {actionError && <p className="form-error">{actionError}</p>}

      <div className="question-list">
        {questions.map((question, index) => (
          <QuestionCard
            key={question.id}
            question={question}
            index={index}
            count={questions.length}
            editable={editable}
            defaultPoints={settings.defaultPoints}
            defaultPenalty={settings.defaultPenalty}
            onMove={moveQuestion}
            onDelete={deleteQuestion}
            onSaveState={onQuestionSaveState}
            onSaved={onQuestionSaved}
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

      {editable && (
        <ImportPanel
          quizId={quiz.id}
          onCommitted={(added) => setQuestions((prev) => [...prev, ...added])}
        />
      )}
    </>
  )

  return (
    <div className="editor">
      <div className="editor-topline">
        <button className="button button-quiet back-button" onClick={onBack}>
          ← All quizzes
        </button>
        <div className="editor-topline-actions">
          <span className={`chip chip-status chip-status-${quiz.status}`}>
            {STATUS_LABEL[quiz.status]}
          </span>
          <button
            className="button button-quiet"
            type="button"
            disabled={questions.length === 0}
            onClick={() => setPreviewing(true)}
          >
            Preview
          </button>
          <span
            className={`save-badge save-badge-${aggregate.tone} save-indicator`}
            role="status"
          >
            {aggregate.text}
          </span>
        </div>
      </div>

      {notices.length > 0 && (
        <div className="editor-banners">
          {notices.map((notice) => (
            <p className="editor-banner" role="status" key={notice.id}>
              <span>{notice.text}</span>
              <button
                className="editor-banner-dismiss"
                type="button"
                aria-label="Dismiss"
                onClick={() =>
                  setNotices((prev) => prev.filter((n) => n.id !== notice.id))
                }
              >
                ×
              </button>
            </p>
          ))}
        </div>
      )}

      {previewing && (
        <PreviewModal
          quizTitle={settings.title}
          shuffled={settings.shuffleQuestions}
          questions={questions}
          onDismiss={() => setPreviewing(false)}
        />
      )}

      {wizard ? (
        <>
          <WizardSteps step={step} onStep={setStep} />
          <p className="visually-hidden" role="status">
            Step {step} of 3:{' '}
            {WIZARD_STEPS.find((s) => s.n === step)?.label}
          </p>

          <div className="wizard-step" hidden={step !== 1}>
            <section className="panel">
              <div className="editor-title-row">{titleField}</div>
            </section>
            {frozenHint}
            {questionsBlock}
          </div>

          <div className="wizard-step" hidden={step !== 2}>
            <AudienceEditor
              ref={audienceRef}
              quizId={quiz.id}
              wizardMode
              onAudienceChange={setAudienceCount}
            />
            {audienceGateError && (
              <p className="form-error wizard-step-error">
                {audienceGateError}
              </p>
            )}
          </div>

          <div className="wizard-step" hidden={step !== 3}>
            <section className="panel">{attemptsAndShuffle}</section>
            <ScheduleSection
              quiz={quiz}
              questionCount={questions.length}
              audienceCount={audienceCount}
              onPublished={setQuiz}
            />
          </div>

          <div className="wizard-nav-actions">
            {step > 1 && (
              <button
                className="button button-quiet"
                type="button"
                onClick={() => setStep((s) => Math.max(1, s - 1) as WizardStep)}
              >
                Back
              </button>
            )}
            {step < 3 && (
              <button
                className="button button-primary wizard-next"
                type="button"
                onClick={() => void goNext()}
              >
                Next
              </button>
            )}
          </div>
        </>
      ) : (
        <>
          <nav className="editor-tabs" role="tablist" aria-label="Quiz views">
            <button
              className={`editor-tab${activeTab === 'settings' ? ' editor-tab-active' : ''}`}
              type="button"
              role="tab"
              aria-selected={activeTab === 'settings'}
              onClick={() => setTab('settings')}
            >
              Settings & questions
            </button>
            {quiz.status === 'live' && (
              <button
                className={`editor-tab${activeTab === 'monitor' ? ' editor-tab-active' : ''}`}
                type="button"
                role="tab"
                aria-selected={activeTab === 'monitor'}
                onClick={() => setTab('monitor')}
              >
                Live scores
              </button>
            )}
            {terminal && (
              <button
                className={`editor-tab${activeTab === 'results' ? ' editor-tab-active' : ''}`}
                type="button"
                role="tab"
                aria-selected={activeTab === 'results'}
                onClick={() => setTab('results')}
              >
                Results & analytics
              </button>
            )}
          </nav>

          <div className="editor-tab-view" hidden={activeTab !== 'settings'}>
            <section className="panel">
              <div className="editor-title-row">{titleField}</div>
              {attemptsAndShuffle}
            </section>

            {frozenHint}
            {questionsBlock}
          </div>

          {quiz.status === 'live' && (
            <div className="editor-tab-view" hidden={activeTab !== 'monitor'}>
              <LiveMonitorPanel
                quizId={quiz.id}
                quizTitle={quiz.title}
                onQuizUpdate={setQuiz}
                onNotice={pushNotice}
              />
            </div>
          )}

          {terminal && (
            <div className="editor-tab-view" hidden={activeTab !== 'results'}>
              <ResultsReleasePanel quiz={quiz} onUpdated={setQuiz} />
              <QuizStatsPanel
                quizId={quiz.id}
                quizTitle={quiz.title}
                questions={questions}
              />
            </div>
          )}
        </>
      )}
    </div>
  )
}
