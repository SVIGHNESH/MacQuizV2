import {
  forwardRef,
  useEffect,
  useImperativeHandle,
  useMemo,
  useState,
} from 'react'
import { api } from '../api/client'
import type { components } from '../api/schema'
import Avatar from '../components/Avatar'

type AssignedStudent = components['schemas']['AssignedStudent']
type Group = components['schemas']['Group']

/**
 * The wizard drives the audience step through this handle: its Next button
 * calls commit() to persist any pending picks, and advances only when the
 * server-expanded audience is non-empty. commit returns that count, or null
 * when the save failed (the error is already shown in-panel).
 */
export interface AudienceHandle {
  commit: () => Promise<number | null>
}

/**
 * The audience picker (docs/04: PUT /quizzes/:id/assignments): pick students
 * directly or by cohort, then save. Used by the authoring wizard's Audience
 * step (wizardMode: no standalone Save button - the wizard's Next persists via
 * the imperative handle), and by LiveMonitorPanel once the quiz is live, where
 * it keeps its own Save button - adding a student there is a late invite;
 * removing one with an in-progress attempt is refused server-side (409
 * ASSIGNMENT_IN_PROGRESS, docs/06 section 1) and surfaces here as the save
 * error, same as any other precondition.
 */
const AudienceEditor = forwardRef<
  AudienceHandle,
  {
    quizId: string
    live?: boolean
    wizardMode?: boolean
    onAudienceChange?: (count: number) => void
  }
>(function AudienceEditor(
  { quizId, live = false, wizardMode = false, onAudienceChange },
  ref,
) {
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
            params: { path: { id: quizId } },
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
      onAudienceChange?.(assigned.size)
    })()
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [quizId])

  const audienceDirty = useMemo(() => {
    if (pickedGroups.size > 0) return true
    if (checked.size !== savedAudience.size) return true
    return [...checked].some((id) => !savedAudience.has(id))
  }, [checked, savedAudience, pickedGroups])

  const saveAudience = async (): Promise<number | null> => {
    setSavingAudience(true)
    setAudienceError(null)
    const result = await api
      .PUT('/api/v1/quizzes/{id}/assignments', {
        params: { path: { id: quizId } },
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
      return null
    }
    // The server answers with the group-expanded audience; that is the truth.
    const assigned = new Set(result.data.students.map((s) => s.id))
    setChecked(assigned)
    setSavedAudience(assigned)
    setPickedGroups(new Set())
    onAudienceChange?.(assigned.size)
    return assigned.size
  }

  // The wizard's Next persists through this: a clean audience needs no round
  // trip (and a needless PUT would reset a just-picked cohort chip), so report
  // the already-saved count; otherwise save and report the expanded count. The
  // emptiness gate reads this return value, i.e. server truth, not checked.size
  // - a cohort with no members must not let Next advance.
  useImperativeHandle(
    ref,
    () => ({
      commit: async () =>
        audienceDirty ? saveAudience() : savedAudience.size,
    }),
    // saveAudience closes over checked/pickedGroups, so the handle must be
    // rebuilt whenever a pick changes - not only when audienceDirty flips -
    // or Next would persist a stale selection.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [audienceDirty, savedAudience, checked, pickedGroups],
  )

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
    <section className="panel publish-panel" aria-label="Audience">
      <h2 className="card-title">Audience</h2>
      <p className="field-hint audience-count">
        {savedAudience.size === 0
          ? 'No students assigned yet. Publishing needs at least one.'
          : `${savedAudience.size} student${savedAudience.size === 1 ? '' : 's'} assigned.`}
      </p>
      {live && (
        <p className="field-hint">
          This quiz is live. Adding a student invites them immediately; a
          student with an in-progress attempt cannot be removed here - use
          Kick on the live roster instead.
        </p>
      )}

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
              <Avatar
                userId={student.id}
                fullName={student.full_name}
                avatar={student.avatar}
                size="small"
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

      {/* In the wizard the Next button persists the audience, so the panel
          shows no Save of its own; the standalone button stays for the live
          roster, where AudienceEditor edits assignments on its own. */}
      {!wizardMode && (
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
            <span className="field-hint">
              Audience changes are not saved yet.
            </span>
          )}
        </div>
      )}
      {audienceError && <p className="form-error">{audienceError}</p>}
    </section>
  )
})

export default AudienceEditor
