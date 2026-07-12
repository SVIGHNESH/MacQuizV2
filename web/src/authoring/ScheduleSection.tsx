import { useState } from 'react'
import { api } from '../api/client'
import type { components } from '../api/schema'
import type { Quiz } from './model'

type Guardrails = components['schemas']['Guardrails']

const WINDOW_FORMAT = new Intl.DateTimeFormat(undefined, {
  day: 'numeric',
  month: 'short',
  hour: '2-digit',
  minute: '2-digit',
})

/** A Date as a datetime-local input value (local time, minute precision). */
function toLocalInput(iso: string): string {
  const d = new Date(iso)
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(
    d.getHours(),
  )}:${pad(d.getMinutes())}`
}

/** The documented guardrail default: nothing enforced, flag at 3. */
const DEFAULT_GUARDRAILS: Guardrails = {
  fullscreen: 'off',
  focus_tracking: 'off',
  block_clipboard: false,
  max_violations: 3,
  violation_action: 'flag',
}

interface ScheduleDraft {
  startsAt: string // datetime-local value
  endsAt: string
  durationMin: number
  guardrails: Guardrails
  releasePolicy: Quiz['release_policy']
}

/**
 * The authoring wizard's final step: schedule a window and a per-attempt
 * duration, set the guardrail ladder, and publish (docs/04: POST
 * /quizzes/:id/publish). Shown while the quiz is draft or scheduled; a
 * scheduled quiz can be republished, which reschedules and bumps the version.
 * The audience is set on the previous step; audienceCount is threaded in from
 * the wizard only to phrase the "freezes N questions for M students" hint.
 */
export default function ScheduleSection({
  quiz,
  questionCount,
  audienceCount,
  onPublished,
}: {
  quiz: Quiz
  questionCount: number
  audienceCount: number
  onPublished: (quiz: Quiz) => void
}) {
  const [draft, setDraft] = useState<ScheduleDraft>(() => ({
    startsAt: quiz.starts_at ? toLocalInput(quiz.starts_at) : '',
    endsAt: quiz.ends_at ? toLocalInput(quiz.ends_at) : '',
    durationMin: quiz.duration_sec ? Math.round(quiz.duration_sec / 60) : 20,
    // Reseed the ladder the scheduled snapshot was published with, so a
    // republish preserves the teacher's real guardrails instead of silently
    // resetting them to the defaults. Null (a never-published draft) means
    // start from the documented default.
    guardrails: quiz.guardrails
      ? { ...quiz.guardrails }
      : { ...DEFAULT_GUARDRAILS },
    // A republish keeps the policy the scheduled snapshot was published with,
    // so rescheduling a manual-release quiz can't silently turn it automatic.
    releasePolicy: quiz.release_policy,
  }))
  const [publishing, setPublishing] = useState(false)
  const [fields, setFields] = useState<Record<string, string>>({})
  const [publishError, setPublishError] = useState<string | null>(null)
  const [confirmingCancel, setConfirmingCancel] = useState(false)
  const [cancelling, setCancelling] = useState(false)

  const scheduled = quiz.status === 'scheduled'

  const setGuardrail = <K extends keyof Guardrails>(
    key: K,
    value: Guardrails[K],
  ) => {
    setDraft((prev) => ({
      ...prev,
      guardrails: { ...prev.guardrails, [key]: value },
    }))
  }

  const publish = async () => {
    // The cheap mistakes fail before the round trip, with the server's own
    // field vocabulary; the server stays the authority on everything else.
    const clientFields: Record<string, string> = {}
    if (draft.startsAt === '') clientFields.starts_at = 'required'
    else if (new Date(draft.startsAt).getTime() <= Date.now()) {
      clientFields.starts_at = 'must be in the future'
    }
    if (draft.endsAt === '') clientFields.ends_at = 'required'
    else if (
      draft.startsAt !== '' &&
      new Date(draft.endsAt).getTime() <= new Date(draft.startsAt).getTime()
    ) {
      clientFields.ends_at = 'must be after starts_at'
    }
    if (
      !Number.isFinite(draft.durationMin) ||
      draft.durationMin * 60 < 30 ||
      draft.durationMin * 60 > 86400
    ) {
      clientFields.duration_sec = 'must be between 30 seconds and 24 hours'
    }
    if (
      !Number.isInteger(draft.guardrails.max_violations) ||
      draft.guardrails.max_violations < 1 ||
      draft.guardrails.max_violations > 100
    ) {
      clientFields['guardrails.max_violations'] = 'must be between 1 and 100'
    }
    setFields(clientFields)
    setPublishError(null)
    if (Object.keys(clientFields).length > 0) return

    setPublishing(true)
    const result = await api
      .POST('/api/v1/quizzes/{id}/publish', {
        params: { path: { id: quiz.id } },
        body: {
          starts_at: new Date(draft.startsAt).toISOString(),
          ends_at: new Date(draft.endsAt).toISOString(),
          duration_sec: Math.round(draft.durationMin * 60),
          guardrails: draft.guardrails,
          release_policy: draft.releasePolicy,
        },
      })
      .catch(() => null)
    setPublishing(false)
    if (!result?.data) {
      setFields(result?.error?.fields ?? {})
      setPublishError(
        result?.error?.fields
          ? null
          : (result?.error?.message ?? 'Could not publish the quiz.'),
      )
      return
    }
    onPublished(result.data.quiz)
  }

  /**
   * Call the quiz off before it opens (docs/06 section 1: "while Scheduled:
   * reschedule and cancel are allowed"). It goes back to Draft with its window
   * cleared - reversible by republishing, which is why this asks for a plain
   * inline confirmation rather than the typed-reason modal force-close uses.
   * The returned draft flows through the same onPublished channel a publish
   * does, so the editor re-derives its editable/wizard state from the status.
   */
  const cancelSchedule = async () => {
    setCancelling(true)
    setPublishError(null)
    const result = await api
      .POST('/api/v1/quizzes/{id}/cancel', {
        params: { path: { id: quiz.id } },
      })
      .catch(() => null)
    setCancelling(false)
    if (!result?.data) {
      setPublishError(
        result?.error?.message ?? 'Could not cancel the scheduled quiz.',
      )
      return
    }
    setConfirmingCancel(false)
    onPublished(result.data.quiz)
  }

  // Precondition errors that have no input of their own read as sentences.
  const generalErrors = Object.entries(fields).filter(
    ([key]) =>
      !['starts_at', 'ends_at', 'duration_sec', 'release_policy'].includes(key) &&
      !key.startsWith('guardrails.'),
  )

  return (
    <section className="panel publish-panel" aria-label="Schedule and publish">
      <h2 className="card-title">
        {scheduled ? 'Scheduled' : 'Schedule & publish'}
      </h2>
      {scheduled && quiz.starts_at && quiz.ends_at && (
        <p className="field-hint window-summary">
          Version {quiz.version} goes live{' '}
          {WINDOW_FORMAT.format(new Date(quiz.starts_at))} and closes{' '}
          {WINDOW_FORMAT.format(new Date(quiz.ends_at))}. Republishing
          reschedules it as version {quiz.version + 1}.
        </p>
      )}
      {!scheduled && (
        <p className="field-hint">
          Publishing freezes the {questionCount} question
          {questionCount === 1 ? '' : 's'} into an immutable snapshot for{' '}
          {audienceCount} assigned student{audienceCount === 1 ? '' : 's'}.
        </p>
      )}

      <div className="schedule-grid">
        <label className="field">
          <span className="field-label">Opens</span>
          <input
            id="publish-starts-at"
            className="input"
            type="datetime-local"
            value={draft.startsAt}
            onChange={(e) => setDraft({ ...draft, startsAt: e.target.value })}
          />
          {fields.starts_at && (
            <p className="field-error">Opens {fields.starts_at}.</p>
          )}
        </label>
        <label className="field">
          <span className="field-label">Closes</span>
          <input
            id="publish-ends-at"
            className="input"
            type="datetime-local"
            value={draft.endsAt}
            onChange={(e) => setDraft({ ...draft, endsAt: e.target.value })}
          />
          {fields.ends_at && (
            <p className="field-error">Closes {fields.ends_at.replace('starts_at', 'the open time')}.</p>
          )}
        </label>
        <label className="field">
          <span className="field-label">Time limit (minutes)</span>
          <input
            id="publish-duration"
            className="input input-points tabular"
            type="number"
            min={1}
            max={1440}
            step={1}
            value={Number.isFinite(draft.durationMin) ? draft.durationMin : ''}
            onChange={(e) =>
              setDraft({ ...draft, durationMin: e.target.valueAsNumber })
            }
          />
          {fields.duration_sec && (
            <p className="field-error">Time limit {fields.duration_sec}.</p>
          )}
        </label>
        <label className="field field-release">
          <span className="field-label">Release results</span>
          <select
            id="publish-release-policy"
            className="input"
            value={draft.releasePolicy}
            onChange={(e) =>
              setDraft({
                ...draft,
                releasePolicy: e.target.value as Quiz['release_policy'],
              })
            }
          >
            <option value="auto">Automatically, once the quiz closes</option>
            <option value="manual">When I release them</option>
          </select>
          {fields.release_policy && (
            <p className="field-error">Release results {fields.release_policy}.</p>
          )}
        </label>
      </div>

      <div className="guardrail-grid">
        <span className="field-label guardrail-title">
          Guardrails{' '}
          <span className="field-label-note">
            - anti-cheat rules, frozen with the snapshot
          </span>
        </span>
        <label className="field">
          <span className="field-label">Fullscreen</span>
          <select
            id="guardrail-fullscreen"
            className="input"
            value={draft.guardrails.fullscreen}
            onChange={(e) =>
              setGuardrail(
                'fullscreen',
                e.target.value as Guardrails['fullscreen'],
              )
            }
          >
            <option value="off">Not required</option>
            <option value="warn">Required - warn on exit</option>
            <option value="count">Required - count violations</option>
          </select>
        </label>
        <label className="field">
          <span className="field-label">Tab / focus changes</span>
          <select
            id="guardrail-focus"
            className="input"
            value={draft.guardrails.focus_tracking}
            onChange={(e) =>
              setGuardrail(
                'focus_tracking',
                e.target.value as Guardrails['focus_tracking'],
              )
            }
          >
            <option value="off">Ignored</option>
            <option value="warn">Warn only</option>
            <option value="count">Count violations</option>
          </select>
        </label>
        <label className="field">
          <span className="field-label">After</span>
          <span className="guardrail-ladder">
            <input
              id="guardrail-max-violations"
              className="input input-points tabular"
              type="number"
              min={1}
              max={100}
              step={1}
              value={
                Number.isFinite(draft.guardrails.max_violations)
                  ? draft.guardrails.max_violations
                  : ''
              }
              onChange={(e) =>
                setGuardrail('max_violations', e.target.valueAsNumber)
              }
            />
            <span className="guardrail-ladder-text">violations</span>
            <select
              id="guardrail-action"
              className="input guardrail-action"
              value={draft.guardrails.violation_action}
              onChange={(e) =>
                setGuardrail(
                  'violation_action',
                  e.target.value as Guardrails['violation_action'],
                )
              }
            >
              <option value="flag">flag the attempt</option>
              <option value="auto_submit">auto-submit the attempt</option>
              <option value="notify">notify me live</option>
            </select>
          </span>
          {fields['guardrails.max_violations'] && (
            <p className="field-error">
              Violations {fields['guardrails.max_violations']}.
            </p>
          )}
        </label>
        <label className="field checkbox-field">
          <span className="field-label">Clipboard</span>
          <span className="checkbox-row">
            <input
              id="guardrail-clipboard"
              type="checkbox"
              checked={draft.guardrails.block_clipboard}
              onChange={(e) =>
                setGuardrail('block_clipboard', e.target.checked)
              }
            />
            Block copy and paste
          </span>
        </label>
      </div>

      {generalErrors.length > 0 && (
        <div className="publish-preconditions">
          {generalErrors.map(([key, message]) => (
            <p key={key} className="form-error">
              {message}
            </p>
          ))}
        </div>
      )}
      {publishError && <p className="form-error">{publishError}</p>}

      <div className="publish-actions">
        <button
          id="publish-button"
          className="button button-primary"
          type="button"
          disabled={publishing}
          onClick={() => void publish()}
        >
          {publishing
            ? 'Publishing…'
            : scheduled
              ? 'Reschedule & republish'
              : 'Publish quiz'}
        </button>
        {scheduled && (
          <span className="schedule-cancel">
            {confirmingCancel ? (
              <>
                <span className="field-hint">
                  The questions and the audience are kept.
                </span>
                <button
                  id="cancel-schedule-confirm"
                  className="button button-danger"
                  type="button"
                  disabled={cancelling}
                  onClick={() => void cancelSchedule()}
                >
                  {cancelling ? 'Cancelling…' : 'Return to draft'}
                </button>
                <button
                  className="button button-quiet"
                  type="button"
                  disabled={cancelling}
                  onClick={() => setConfirmingCancel(false)}
                >
                  Keep it scheduled
                </button>
              </>
            ) : (
              <button
                id="cancel-schedule-button"
                className="button button-quiet-danger"
                type="button"
                onClick={() => setConfirmingCancel(true)}
              >
                Cancel schedule
              </button>
            )}
          </span>
        )}
      </div>
    </section>
  )
}
