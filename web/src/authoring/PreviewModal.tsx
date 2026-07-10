import { useEffect, useMemo, useRef, useState } from 'react'
import {
  TYPE_LABEL,
  type QuestionOption,
  type QuestionType,
  type TeacherQuestion,
} from './model'

/**
 * The projection the attempt API hands a student (AttemptQuestion in docs/04):
 * no answer key, no topic tag. The preview is built by copying these fields one
 * by one rather than by spreading a TeacherQuestion and deleting `correct`, so
 * a teacher-only field added later cannot leak into the student view by
 * default - it has to be added here on purpose.
 */
interface PreviewQuestion {
  id: string
  type: QuestionType
  text: string
  options: QuestionOption[]
}

function asStudentSees(question: TeacherQuestion): PreviewQuestion {
  return {
    id: question.id,
    type: question.type,
    text: question.body.text,
    options: (question.options ?? []).map((option) => ({
      key: option.key,
      text: option.text,
    })),
  }
}

export default function PreviewModal({
  quizTitle,
  shuffled,
  questions,
  onDismiss,
}: {
  quizTitle: string
  shuffled: boolean
  questions: TeacherQuestion[]
  onDismiss: () => void
}) {
  const preview = useMemo(() => questions.map(asStudentSees), [questions])
  const [index, setIndex] = useState(0)
  const panel = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onDismiss()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onDismiss])

  useEffect(() => {
    panel.current?.focus()
  }, [])

  const current = preview[index]

  return (
    <div className="modal-overlay" role="presentation" onClick={onDismiss}>
      <div
        ref={panel}
        tabIndex={-1}
        className="modal-panel preview-panel"
        role="dialog"
        aria-modal="true"
        aria-label={`Student preview of ${quizTitle}`}
        onClick={(event) => event.stopPropagation()}
      >
        <p className="eyebrow">Student preview</p>
        <h2 className="modal-title">{quizTitle}</h2>
        <p className="modal-subtitle">
          Each question exactly as a student receives it. The answer key is not
          part of that payload, so it is not shown here.
        </p>

        {shuffled && (
          <p className="hint">
            Students each see these questions in their own random order; the
            preview follows the authoring order.
          </p>
        )}

        {!current ? (
          <p className="hint">This quiz has no questions to preview yet.</p>
        ) : (
          <div className="preview-question">
            {/* The visible eyebrow reads as a caption; the announced sentence
                carries the prompt too, because paging with Next leaves focus on
                the button and a screen reader would otherwise hear nothing. */}
            <p className="visually-hidden" role="status">
              {`Question ${index + 1} of ${preview.length}. ${current.text}`}
            </p>
            <div className="preview-question-head">
              <span className="eyebrow" aria-hidden="true">
                Question {index + 1} of {preview.length}
              </span>
              <span className="chip chip-type">{TYPE_LABEL[current.type]}</span>
            </div>
            <h3 className="preview-question-text">{current.text}</h3>

            <fieldset className="preview-fieldset" disabled>
              <legend className="visually-hidden">{current.text}</legend>

              {(current.type === 'single' || current.type === 'multi') && (
                <div className="option-list">
                  {current.options.map((option) => (
                    <label key={option.key} className="option-row">
                      <input
                        type={current.type === 'single' ? 'radio' : 'checkbox'}
                        name={`preview-${current.id}`}
                        checked={false}
                        readOnly
                      />
                      <span className="option-key">
                        {option.key.toUpperCase()}
                      </span>
                      <span className="option-static">{option.text}</span>
                    </label>
                  ))}
                </div>
              )}

              {current.type === 'truefalse' && (
                <div className="option-list">
                  {[true, false].map((bool) => (
                    <label key={String(bool)} className="option-row">
                      <input
                        type="radio"
                        name={`preview-${current.id}`}
                        checked={false}
                        readOnly
                      />
                      <span className="option-key">{bool ? 'T' : 'F'}</span>
                      <span className="option-static">
                        {bool ? 'True' : 'False'}
                      </span>
                    </label>
                  ))}
                </div>
              )}

              {current.type === 'short' && (
                <input
                  className="input"
                  type="text"
                  placeholder="Type your answer"
                  value=""
                  readOnly
                />
              )}
            </fieldset>
          </div>
        )}

        <div className="modal-actions preview-actions">
          <button
            className="button button-quiet"
            type="button"
            disabled={index === 0}
            onClick={() => setIndex((i) => i - 1)}
          >
            Previous
          </button>
          <button
            className="button button-quiet"
            type="button"
            disabled={index >= preview.length - 1}
            onClick={() => setIndex((i) => i + 1)}
          >
            Next
          </button>
          <button className="button" type="button" onClick={onDismiss}>
            Close preview
          </button>
        </div>
      </div>
    </div>
  )
}
