import { useEffect, useMemo, useState, type FormEvent } from 'react'
import { api } from '../api/client'
import type { components } from '../api/schema'

type Group = components['schemas']['Group']
type AssignedStudent = components['schemas']['AssignedStudent']

const DATE_FORMAT = new Intl.DateTimeFormat(undefined, {
  day: 'numeric',
  month: 'short',
  year: 'numeric',
})

/**
 * Admin cohort management (Milestone 1, FR-1): create groups and edit their
 * membership. Groups are what a teacher's audience picker (PublishPanel)
 * assigns quizzes to, so this is the only place a cohort's roster is set.
 */
export default function GroupsPanel() {
  const [groups, setGroups] = useState<Group[] | null>(null)
  const [students, setStudents] = useState<AssignedStudent[]>([])
  const [loadError, setLoadError] = useState<string | null>(null)

  const [creating, setCreating] = useState(false)
  const [newName, setNewName] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [createError, setCreateError] = useState<string | null>(null)

  const [editingGroup, setEditingGroup] = useState<Group | null>(null)
  const [checked, setChecked] = useState<Set<string>>(new Set())
  const [savingMembers, setSavingMembers] = useState(false)
  const [membersError, setMembersError] = useState<string | null>(null)

  const load = async () => {
    const [groupsResult, directoryResult] = await Promise.all([
      api.GET('/api/v1/groups').catch(() => null),
      api.GET('/api/v1/directory').catch(() => null),
    ])
    if (!groupsResult?.data || !directoryResult?.data) {
      setLoadError(groupsResult?.error?.message ?? 'Could not load groups. Reload to retry.')
      return
    }
    setLoadError(null)
    setGroups(groupsResult.data.groups)
    setStudents(directoryResult.data.students)
  }

  useEffect(() => {
    void load()
  }, [])

  const createGroup = async (event: FormEvent) => {
    event.preventDefault()
    const name = newName.trim()
    if (name === '') {
      setCreateError('Give the cohort a name first.')
      return
    }
    setSubmitting(true)
    setCreateError(null)
    const result = await api.POST('/api/v1/groups', { body: { name } }).catch(() => null)
    setSubmitting(false)
    if (!result?.data) {
      setCreateError(result?.error?.message ?? 'Could not create the cohort.')
      return
    }
    setGroups((prev) => (prev ? [result.data.group, ...prev] : [result.data.group]))
    setCreating(false)
    setNewName('')
  }

  const openMembers = (group: Group) => {
    setEditingGroup(group)
    setChecked(new Set())
    setMembersError(null)
    ;(async () => {
      const result = await api
        .GET('/api/v1/groups/{id}/members', { params: { path: { id: group.id } } })
        .catch(() => null)
      if (!result?.data) return
      setChecked(new Set(result.data.students.map((s) => s.id)))
    })()
  }

  const toggleMember = (id: string) => {
    setChecked((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  const saveMembers = async () => {
    if (!editingGroup) return
    setSavingMembers(true)
    setMembersError(null)
    const result = await api
      .PUT('/api/v1/groups/{id}/members', {
        params: { path: { id: editingGroup.id } },
        body: { student_ids: [...checked] },
      })
      .catch(() => null)
    setSavingMembers(false)
    if (!result?.data) {
      setMembersError(result?.error?.message ?? 'Could not save membership.')
      return
    }
    setGroups((prev) => (prev ?? []).map((g) => (g.id === result.data.group.id ? result.data.group : g)))
    setEditingGroup(null)
  }

  const editingCount = useMemo(() => checked.size, [checked])

  if (loadError) return <p className="form-error">{loadError}</p>
  if (!groups) {
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
          <p className="eyebrow">Admin console</p>
          <h1 className="page-title">Groups</h1>
        </div>
        {!creating && (
          <button className="button button-primary" type="button" onClick={() => setCreating(true)}>
            New group
          </button>
        )}
      </div>

      {creating && (
        <form className="panel create-form" onSubmit={createGroup}>
          <label className="field create-field">
            <span className="field-label">Cohort name</span>
            <input
              className="input"
              type="text"
              placeholder="For example: Grade 10 - Section A"
              value={newName}
              autoFocus
              onChange={(e) => setNewName(e.target.value)}
            />
          </label>
          <div className="create-actions">
            <button className="button button-primary" type="submit" disabled={submitting}>
              {submitting ? 'Creating…' : 'Create cohort'}
            </button>
            <button
              className="button button-quiet"
              type="button"
              disabled={submitting}
              onClick={() => {
                setCreating(false)
                setNewName('')
                setCreateError(null)
              }}
            >
              Cancel
            </button>
          </div>
          {createError && <p className="field-error">{createError}</p>}
        </form>
      )}

      {editingGroup && (
        <section className="panel admin-members-panel" aria-label="Edit membership">
          <h2 className="card-title">Members of {editingGroup.name}</h2>
          <p className="field-hint">{editingCount} student{editingCount === 1 ? '' : 's'} selected.</p>
          {students.length === 0 ? (
            <p className="hint">There are no student accounts yet.</p>
          ) : (
            <div className="audience-list" role="group" aria-label="Students">
              {students.map((student) => (
                <label key={student.id} className="audience-row">
                  <input
                    type="checkbox"
                    checked={checked.has(student.id)}
                    onChange={() => toggleMember(student.id)}
                  />
                  <span className="audience-name">{student.full_name}</span>
                  <span className="audience-email">{student.email}</span>
                </label>
              ))}
            </div>
          )}
          <div className="audience-actions">
            <button
              className="button button-primary"
              type="button"
              disabled={savingMembers}
              onClick={() => void saveMembers()}
            >
              {savingMembers ? 'Saving…' : 'Save members'}
            </button>
            <button
              className="button button-quiet"
              type="button"
              disabled={savingMembers}
              onClick={() => setEditingGroup(null)}
            >
              Cancel
            </button>
          </div>
          {membersError && <p className="form-error">{membersError}</p>}
        </section>
      )}

      {groups.length === 0 ? (
        <section className="panel empty-state">
          <h2 className="card-title">No cohorts yet</h2>
          <p className="hint">
            Create a group so teachers can assign quizzes to a whole cohort at once.
          </p>
        </section>
      ) : (
        <div className="admin-group-table" aria-label="Groups">
          {groups.map((group) => (
            <section
              key={group.id}
              className={`group-card${group.member_count === 0 ? ' group-card-empty' : ''}`}
            >
              <header className="group-card-head">
                <h2 className="group-card-name">{group.name}</h2>
                <span className="group-card-count tabular">
                  {group.member_count} member{group.member_count === 1 ? '' : 's'}
                </span>
              </header>
              <div className="group-card-foot">
                {group.member_count === 0 ? (
                  <span className="group-card-hint">
                    Empty cohort · add members to use it as an audience
                  </span>
                ) : (
                  <span className="group-card-hint">
                    Created {DATE_FORMAT.format(new Date(group.created_at))}
                  </span>
                )}
                <button
                  className="button button-small button-quiet"
                  type="button"
                  onClick={() => openMembers(group)}
                >
                  Manage
                </button>
              </div>
            </section>
          ))}
        </div>
      )}
    </div>
  )
}
