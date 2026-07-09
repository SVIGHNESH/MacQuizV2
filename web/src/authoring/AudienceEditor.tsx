import { useEffect, useMemo, useState } from 'react'
import { api } from '../api/client'
import type { components } from '../api/schema'

type AssignedStudent = components['schemas']['AssignedStudent']
type Group = components['schemas']['Group']

/**
 * The audience picker (docs/04: PUT /quizzes/:id/assignments): pick students
 * directly or by cohort, then save. Used by PublishPanel while the quiz is
 * draft or scheduled, and by LiveMonitorPanel once it is live - adding a
 * student there is a late invite; removing one with an in-progress attempt
 * is refused server-side (409 ASSIGNMENT_IN_PROGRESS, docs/06 section 1) and
 * surfaces here as the save error, same as any other precondition.
 */
export default function AudienceEditor({
  quizId,
  live = false,
  onAudienceChange,
}: {
  quizId: string
  live?: boolean
  onAudienceChange?: (count: number) => void
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

  const saveAudience = async () => {
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
      return
    }
    // The server answers with the group-expanded audience; that is the truth.
    const assigned = new Set(result.data.students.map((s) => s.id))
    setChecked(assigned)
    setSavedAudience(assigned)
    setPickedGroups(new Set())
    onAudienceChange?.(assigned.size)
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
  )
}
