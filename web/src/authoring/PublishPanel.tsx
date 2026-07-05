import { useEffect, useMemo, useState } from 'react'
import { api } from '../api/client'
import type { components } from '../api/schema'
import type { Quiz } from './model'

type AssignedStudent = components['schemas']['AssignedStudent']
type Group = components['schemas']['Group']
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

/**
 * Audience and scheduling for one quiz: pick students (directly or by
 * cohort), then publish with a window, a per-attempt duration, and the
 * guardrail ladder. Shown while the quiz is draft or scheduled; a scheduled
 * quiz can be republished, which reschedules and bumps the version.
 */
export default function PublishPanel({
  quiz,
  questionCount,
  onPublished,
}: {
  quiz: Quiz
  questionCount: number
  onPublished: (quiz: Quiz) => void
}) {
  const [students, setStudents] = useState<AssignedStudent[] | null>(null)
  const [groups, setGroups] = useState<Group[]>([])
  const [loadError, setLoadError] = useState<string | null>(null)

  // The audience being edited vs. the audience the server has.
  const [checked, setChecked] = useState<Set<string>>(new Set())
  const [savedAudience, setSavedAudience] = useState<Set<string>>(new Set())
  const [pickedGroups, setPickedGroups] = useState<Set<string>>(new Set())
  const [savingAudience, setSavingAudience] = useState(false)
  const [audienceError, setAudienceError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      const [directory, assignments] = await Promise.all([
        api.GET('/api/v1/directory').catch(() => null),
        api
          .GET('/api/v1/quizzes/{id}/assignments', {
            params: { path: { id: quiz.id } },
          })
          .catch(() => null),
      ])
      if (cancelled) return
      if (!directory?.data || !assignments?.data) {
        setLoadError('Could not load students and groups. Reload to retry.')
        return
      }
      setStudents(directory.data.students)
      setGroups(directory.data.groups)
      const assigned = new Set(assignments.data.students.map((s) => s.id))
      setChecked(assigned)
      setSavedAudience(assigned)
    })()
    return () => {
      cancelled = true
    }
  }, [quiz.id])

  const audienceDirty = useMemo(() => {
    if (pickedGroups.size > 0) return true
    if (checked.size !== savedAudience.size) return true
    return [...checked].some((id) => !savedAudience.has(id))
  }, [checked, savedAudience, pickedGroups])

  const saveAudience = async () => {
    setSavingAudience(true)
    setAudienceError(null)
    const result = await api
      .PUT('/api/v1/quizzes/{id}/assignments', {
        params: { path: { id: quiz.id } },
        body: {
          student_ids: [...checked],
          group_ids: [...pickedGroups],
        },
      })
      .catch(() => null)
    setSavingAudience(false)
    if (!result?.data) {
      setAudienceError(
        result?.error?.message ?? 'Could not save the audience.',
      )
      return
    }
    // The server answers with the group-expanded audience; that is the truth.
    const assigned = new Set(result.data.students.map((s) => s.id))
    setChecked(assigned)
    setSavedAudience(assigned)
    setPickedGroups(new Set())
  }

  const toggleStudent = (id: string) => {
    setChecked((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  const toggleGroup = (id: string) => {
    setPickedGroups((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  if (loadError) return <p className="form-error">{loadError}</p>
  if (!students) {
    return (
      <p className="boot-note" role="status">
        Loading audience…
      </p>
    )
  }

  return (
    <>
      <section className="panel publish-panel" aria-label="Audience">
        <h2 className="card-title">Audience</h2>
        <p className="field-hint audience-count">
          {savedAudience.size === 0
            ? 'No students assigned yet. Publishing needs at least one.'
            : `${savedAudience.size} student${savedAudience.size === 1 ? '' : 's'} assigned.`}
        </p>

        {students.length === 0 ? (
          <p className="hint">
            There are no student accounts yet. Ask an administrator to
            provision your students first.
          </p>
        ) : (
          <div className="audience-list" role="group" aria-label="Students">
            {students.map((student) => (
              <label key={student.id} className="audience-row">
                <input
                  type="checkbox"
                  checked={checked.has(student.id)}
                  onChange={() => toggleStudent(student.id)}
                />
                <span className="audience-name">{student.full_name}</span>
                <span className="audience-email">{student.email}</span>
              </label>
            ))}
          </div>
        )}

        {groups.length > 0 && (
          <div className="audience-groups">
            <span className="field-label">
              Add a whole cohort{' '}
              <span className="field-label-note">
                - members are added when you save
              </span>
            </span>
            <div className="group-chip-row">
              {groups.map((group) => (
                <button
                  key={group.id}
                  type="button"
                  className={`button button-small group-chip${
                    pickedGroups.has(group.id) ? ' group-chip-on' : ''
                  }`}
                  aria-pressed={pickedGroups.has(group.id)}
                  onClick={() => toggleGroup(group.id)}
                >
                  {group.name}
                  <span className="group-chip-count tabular">
                    {group.member_count}
                  </span>
                </button>
              ))}
            </div>
          </div>
        )}

        <div className="audience-actions">
          <button
            className="button button-primary"
            type="button"
            disabled={savingAudience || !audienceDirty}
            onClick={() => void saveAudience()}
          >
            {savingAudience ? 'Saving…' : 'Save audience'}
          </button>
          {audienceDirty && !savingAudience && (
            <span className="field-hint">Audience changes are not saved yet.</span>
          )}
        </div>
        {audienceError && <p className="form-error">{audienceError}</p>}
      </section>

      <ScheduleSection
        quiz={quiz}
        questionCount={questionCount}
        audienceCount={savedAudience.size}
        onPublished={onPublished}
      />
    </>
  )
}

interface ScheduleDraft {
  startsAt: string // datetime-local value
  endsAt: string
  durationMin: number
  guardrails: Guardrails
}

function ScheduleSection({
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
    guardrails: { ...DEFAULT_GUARDRAILS },
  }))
  const [publishing, setPublishing] = useState(false)
  const [fields, setFields] = useState<Record<string, string>>({})
  const [publishError, setPublishError] = useState<string | null>(null)

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

  // Precondition errors that have no input of their own read as sentences.
  const generalErrors = Object.entries(fields).filter(
    ([key]) =>
      !['starts_at', 'ends_at', 'duration_sec'].includes(key) &&
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
      </div>
    </section>
  )
}
