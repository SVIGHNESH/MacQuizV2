import { useCallback, useEffect, useState } from 'react'
import { api } from '../api/client'
import {
  TYPE_LABEL,
  fromQuestion,
  nextOptionKey,
  toInput,
  type QuestionDraft,
  type TeacherQuestion,
} from './model'
import { useAutosave, type SaveResult, type SaveState } from './useAutosave'

interface QuestionCardProps {
  question: TeacherQuestion
  index: number
  count: number
  /** False once the quiz left draft: the content is a frozen snapshot. */
  editable: boolean
  onMove: (id: string, direction: -1 | 1) => void
  onDelete: (id: string) => Promise<void>
  onSaveState: (id: string, state: SaveState) => void
}

function saveStateLabel(state: SaveState): { text: string; tone: string } {
  switch (state.phase) {
    case 'saved':
      return { text: 'Saved', tone: 'ok' }
    case 'pending':
    case 'saving':
      return { text: 'Saving…', tone: 'busy' }
    case 'error':
      return { text: 'Not saved', tone: 'bad' }
  }
}

export default function QuestionCard({
  question,
  index,
  count,
  editable,
  onMove,
  onDelete,
  onSaveState,
}: QuestionCardProps) {
  const [draft, setDraft] = useState<QuestionDraft>(() =>
    fromQuestion(question),
  )
  const [confirmingDelete, setConfirmingDelete] = useState(false)
  const [deleting, setDeleting] = useState(false)

  const save = useCallback(
    async (value: QuestionDraft): Promise<SaveResult> => {
      const serialized = toInput(value)
      if ('fields' in serialized) {
        return {
          ok: false,
          message: 'Complete the question to save it.',
          fields: serialized.fields,
        }
      }
      const result = await api.PATCH('/api/v1/questions/{id}', {
        params: { path: { id: question.id } },
        body: serialized.input,
      })
      if (result.data) return { ok: true }
      return {
        ok: false,
        message: result.error?.message ?? 'Saving failed.',
        fields: result.error?.fields,
      }
    },
    [question.id],
  )

  const saveState = useAutosave(draft, save)
  useEffect(() => {
    onSaveState(question.id, saveState)
  }, [question.id, saveState, onSaveState])

  const fields = saveState.phase === 'error' ? (saveState.fields ?? {}) : {}
  const badge = saveStateLabel(saveState)

  const edit = (patch: Partial<QuestionDraft>) =>
    setDraft((prev) => ({ ...prev, ...patch }))

  const setOptionText = (key: string, text: string) =>
    edit({
      options: draft.options.map((o) => (o.key === key ? { ...o, text } : o)),
    })

  const addOption = () => {
    const key = nextOptionKey(draft.options)
    if (!key) return
    edit({ options: [...draft.options, { key, text: '' }] })
  }

  const removeOption = (key: string) => {
    const options = draft.options.filter((o) => o.key !== key)
    let correctKeys = draft.correctKeys.filter((k) => k !== key)
    // A single-choice question always has exactly one marked answer, so the
    // marker moves rather than vanishing when its option is removed.
    if (draft.type === 'single' && correctKeys.length === 0 && options[0]) {
      correctKeys = [options[0].key]
    }
    edit({ options, correctKeys })
  }

  const toggleCorrect = (key: string) => {
    if (draft.type === 'single') {
      edit({ correctKeys: [key] })
      return
    }
    edit({
      correctKeys: draft.correctKeys.includes(key)
        ? draft.correctKeys.filter((k) => k !== key)
        : [...draft.correctKeys, key],
    })
  }

  const setAccepted = (i: number, text: string) =>
    edit({ accepted: draft.accepted.map((a, j) => (j === i ? text : a)) })

  return (
    <article className="question-card" data-question-id={question.id}>
      <header className="question-head">
        <span className="question-index tabular">{index + 1}</span>
        <span className="chip chip-type">{TYPE_LABEL[draft.type]}</span>
        <span className={`save-badge save-badge-${badge.tone}`} role="status">
          {badge.text}
        </span>
        <span className="question-head-spacer" />
        {editable && (
          <>
            <button
              className="icon-button"
              type="button"
              aria-label="Move question up"
              disabled={index === 0}
              onClick={() => onMove(question.id, -1)}
            >
              ↑
            </button>
            <button
              className="icon-button"
              type="button"
              aria-label="Move question down"
              disabled={index === count - 1}
              onClick={() => onMove(question.id, 1)}
            >
              ↓
            </button>
            {confirmingDelete ? (
              <>
                <button
                  className="button button-small button-danger"
                  type="button"
                  disabled={deleting}
                  onClick={() => {
                    setDeleting(true)
                    void onDelete(question.id).finally(() => {
                      setDeleting(false)
                      setConfirmingDelete(false)
                    })
                  }}
                >
                  {deleting ? 'Removing…' : 'Remove question'}
                </button>
                <button
                  className="button button-small button-quiet"
                  type="button"
                  disabled={deleting}
                  onClick={() => setConfirmingDelete(false)}
                >
                  Keep
                </button>
              </>
            ) : (
              <button
                className="button button-small button-quiet-danger"
                type="button"
                onClick={() => setConfirmingDelete(true)}
              >
                Delete
              </button>
            )}
          </>
        )}
      </header>

      <label className="field">
        <span className="field-label">Question</span>
        <textarea
          className="input question-text"
          rows={2}
          value={draft.text}
          disabled={!editable}
          onChange={(e) => edit({ text: e.target.value })}
        />
        {fields.body && <p className="field-error">{fields.body}</p>}
      </label>

      {(draft.type === 'single' || draft.type === 'multi') && (
        <div className="field">
          <span className="field-label">
            Options
            <span className="field-label-note">
              {draft.type === 'single'
                ? ' · mark the correct answer'
                : ' · mark every correct answer'}
            </span>
          </span>
          <div className="option-list">
            {draft.options.map((option) => {
              const marked = draft.correctKeys.includes(option.key)
              return (
                <div
                  key={option.key}
                  className={`option-row${marked ? ' option-row-selected' : ''}`}
                >
                  <input
                    type={draft.type === 'single' ? 'radio' : 'checkbox'}
                    name={`correct-${question.id}`}
                    aria-label={`Option ${option.key.toUpperCase()} is correct`}
                    checked={marked}
                    disabled={!editable}
                    onChange={() => toggleCorrect(option.key)}
                  />
                  <span className="option-key">{option.key.toUpperCase()}</span>
                  <input
                    className="input option-text"
                    type="text"
                    placeholder="Option text"
                    value={option.text}
                    disabled={!editable}
                    onChange={(e) => setOptionText(option.key, e.target.value)}
                  />
                  {editable && (
                    <button
                      className="icon-button"
                      type="button"
                      aria-label={`Remove option ${option.key.toUpperCase()}`}
                      disabled={draft.options.length <= 2}
                      onClick={() => removeOption(option.key)}
                    >
                      ×
                    </button>
                  )}
                </div>
              )
            })}
          </div>
          {(fields.options || fields.correct) && (
            <p className="field-error">{fields.options ?? fields.correct}</p>
          )}
          {editable && (
            <button
              className="button button-small button-quiet add-option"
              type="button"
              disabled={draft.options.length >= 8}
              onClick={addOption}
            >
              Add option
            </button>
          )}
        </div>
      )}

      {draft.type === 'truefalse' && (
        <div className="field">
          <span className="field-label">Correct answer</span>
          <div className="option-list">
            {[true, false].map((value) => (
              <label
                key={String(value)}
                className={`option-row${
                  draft.correctBool === value ? ' option-row-selected' : ''
                }`}
              >
                <input
                  type="radio"
                  name={`correct-${question.id}`}
                  checked={draft.correctBool === value}
                  disabled={!editable}
                  onChange={() => edit({ correctBool: value })}
                />
                <span className="option-key">{value ? 'T' : 'F'}</span>
                <span className="option-static">
                  {value ? 'True' : 'False'}
                </span>
              </label>
            ))}
          </div>
        </div>
      )}

      {draft.type === 'short' && (
        <div className="field">
          <span className="field-label">
            Accepted answers
            <span className="field-label-note">
              {' '}
              · any one of these counts as correct
            </span>
          </span>
          <div className="option-list">
            {draft.accepted.map((answer, i) => (
              <div key={i} className="option-row">
                <span className="option-key tabular">{i + 1}</span>
                <input
                  className="input option-text"
                  type="text"
                  placeholder="Accepted answer"
                  value={answer}
                  disabled={!editable}
                  onChange={(e) => setAccepted(i, e.target.value)}
                />
                {editable && (
                  <button
                    className="icon-button"
                    type="button"
                    aria-label={`Remove accepted answer ${i + 1}`}
                    disabled={draft.accepted.length <= 1}
                    onClick={() =>
                      edit({ accepted: draft.accepted.filter((_, j) => j !== i) })
                    }
                  >
                    ×
                  </button>
                )}
              </div>
            ))}
          </div>
          {fields.correct && <p className="field-error">{fields.correct}</p>}
          {editable && (
            <button
              className="button button-small button-quiet add-option"
              type="button"
              onClick={() => edit({ accepted: [...draft.accepted, ''] })}
            >
              Add accepted answer
            </button>
          )}
        </div>
      )}

      <div className="field-row">
        <label className="field field-points">
          <span className="field-label">Points</span>
          <input
            className="input input-points tabular"
            type="number"
            min={0.5}
            max={1000}
            step={0.5}
            value={Number.isFinite(draft.points) ? draft.points : ''}
            disabled={!editable}
            onChange={(e) => edit({ points: e.target.valueAsNumber })}
          />
          {fields.points && <p className="field-error">{fields.points}</p>}
        </label>

        <label className="field field-topic">
          <span className="field-label">Topic</span>
          <input
            className="input"
            type="text"
            maxLength={60}
            placeholder="Optional, e.g. Data privacy"
            value={draft.topic}
            disabled={!editable}
            onChange={(e) => edit({ topic: e.target.value })}
          />
          {fields.topic ? (
            <p className="field-error">{fields.topic}</p>
          ) : (
            <p className="field-hint">
              Questions sharing a topic are averaged into each student&rsquo;s
              topic strengths.
            </p>
          )}
        </label>
      </div>

      {saveState.phase === 'error' &&
        Object.keys(fields).length === 0 && (
          <p className="form-error">{saveState.message}</p>
        )}
    </article>
  )
}
