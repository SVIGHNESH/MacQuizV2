import { useEffect, useState, type FormEvent } from 'react'
import { api } from '../api/client'
import type { components } from '../api/schema'
import CredentialModal from './CredentialModal'
import TeacherStatsModal from './TeacherStatsModal'

type User = components['schemas']['User']

const ROLE_LABEL: Record<User['role'], string> = {
  admin: 'Admin',
  teacher: 'Teacher',
  student: 'Student',
}

function initials(fullName: string): string {
  return fullName
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((part) => part[0]!.toUpperCase())
    .join('')
}

/**
 * An account that still holds its admin-issued credential has been created
 * but never used - "invite pending" is the honest reading of
 * must_change_password, and it is amber because it is evidence, not fault.
 */
function statusOf(user: User): { tone: string; label: string } {
  if (user.status === 'disabled') {
    return { tone: 'inactive', label: 'Disabled' }
  }
  if (user.must_change_password) {
    return { tone: 'pending', label: 'Invite pending' }
  }
  return { tone: 'active', label: 'Active' }
}

/**
 * Admin account provisioning (Milestone 1, FR-1 / docs/11 "Users +
 * provision"): list every account, create a teacher or student with a
 * generated one-time credential, and disable/re-enable or reset a credential.
 * The generated password is shown exactly once, matching the API.
 */
export default function UsersPanel() {
  const [users, setUsers] = useState<User[] | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [roleFilter, setRoleFilter] = useState<'' | User['role']>('')
  const [statusFilter, setStatusFilter] = useState<'' | User['status']>('')
  const [search, setSearch] = useState('')

  const [creating, setCreating] = useState(false)
  const [newRole, setNewRole] = useState<'teacher' | 'student'>('student')
  const [newEmail, setNewEmail] = useState('')
  const [newFullName, setNewFullName] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [createFields, setCreateFields] = useState<Record<string, string>>({})

  const [revealed, setRevealed] = useState<{ user: User; password: string } | null>(null)
  const [statsFor, setStatsFor] = useState<User | null>(null)
  const [busyUserID, setBusyUserID] = useState<string | null>(null)
  const [rowError, setRowError] = useState<string | null>(null)

  const load = async () => {
    const result = await api
      .GET('/api/v1/users', {
        params: {
          query: {
            role: roleFilter || undefined,
            status: statusFilter || undefined,
          },
        },
      })
      .catch(() => null)
    if (!result?.data) {
      setLoadError(result?.error?.message ?? 'Could not load accounts. Reload to retry.')
      return
    }
    setLoadError(null)
    setUsers(result.data.users)
  }

  useEffect(() => {
    void load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [roleFilter, statusFilter])

  const createUser = async (event: FormEvent) => {
    event.preventDefault()
    const email = newEmail.trim()
    const fullName = newFullName.trim()
    const fields: Record<string, string> = {}
    if (email === '') fields.email = 'required'
    if (fullName === '') fields.full_name = 'required'
    if (Object.keys(fields).length > 0) {
      setCreateFields(fields)
      return
    }
    setSubmitting(true)
    setCreateFields({})
    const result = await api
      .POST('/api/v1/users', { body: { role: newRole, email, full_name: fullName } })
      .catch(() => null)
    setSubmitting(false)
    if (!result?.data) {
      setCreateFields(result?.error?.fields ?? { body: result?.error?.message ?? 'Could not create the account.' })
      return
    }
    setUsers((prev) => (prev ? [result.data.user, ...prev] : [result.data.user]))
    setRevealed({ user: result.data.user, password: result.data.initial_password ?? '' })
    setCreating(false)
    setNewEmail('')
    setNewFullName('')
    setNewRole('student')
  }

  const toggleStatus = async (user: User) => {
    setRowError(null)
    setBusyUserID(user.id)
    const nextStatus = user.status === 'active' ? 'disabled' : 'active'
    const result = await api
      .PATCH('/api/v1/users/{id}', {
        params: { path: { id: user.id } },
        body: { status: nextStatus },
      })
      .catch(() => null)
    setBusyUserID(null)
    if (!result?.data) {
      setRowError(result?.error?.message ?? 'Could not update the account.')
      return
    }
    setUsers((prev) => (prev ?? []).map((u) => (u.id === user.id ? result.data.user : u)))
  }

  const resetPassword = async (user: User) => {
    setRowError(null)
    setBusyUserID(user.id)
    const result = await api
      .PATCH('/api/v1/users/{id}', {
        params: { path: { id: user.id } },
        body: { reset_password: true },
      })
      .catch(() => null)
    setBusyUserID(null)
    if (!result?.data) {
      setRowError(result?.error?.message ?? 'Could not reset the credential.')
      return
    }
    setUsers((prev) => (prev ?? []).map((u) => (u.id === user.id ? result.data.user : u)))
    setRevealed({ user: result.data.user, password: result.data.initial_password ?? '' })
  }

  if (loadError) return <p className="form-error">{loadError}</p>
  if (!users) {
    return (
      <p className="boot-note" role="status">
        Loading…
      </p>
    )
  }

  const needle = search.trim().toLowerCase()
  const visible = needle
    ? users.filter(
        (user) =>
          user.full_name.toLowerCase().includes(needle) ||
          user.email.toLowerCase().includes(needle),
      )
    : users

  return (
    <div className="quiz-list">
      <div className="page-head">
        <div>
          <p className="eyebrow">Admin console</p>
          <h1 className="page-title">Users</h1>
        </div>
        {!creating && (
          <button className="button button-primary" type="button" onClick={() => setCreating(true)}>
            Provision user
          </button>
        )}
      </div>

      {creating && (
        <form className="panel create-form admin-user-form" onSubmit={createUser}>
          <label className="field">
            <span className="field-label">Role</span>
            <select
              className="input"
              value={newRole}
              onChange={(e) => setNewRole(e.target.value as 'teacher' | 'student')}
            >
              <option value="student">Student</option>
              <option value="teacher">Teacher</option>
            </select>
          </label>
          <label className="field">
            <span className="field-label">Full name</span>
            <input
              className="input"
              type="text"
              value={newFullName}
              onChange={(e) => setNewFullName(e.target.value)}
            />
            {createFields.full_name && <p className="field-error">{createFields.full_name}</p>}
          </label>
          <label className="field">
            <span className="field-label">Email</span>
            <input
              className="input"
              type="email"
              value={newEmail}
              onChange={(e) => setNewEmail(e.target.value)}
            />
            {createFields.email && <p className="field-error">{createFields.email}</p>}
          </label>
          <div className="create-actions">
            <button className="button button-primary" type="submit" disabled={submitting}>
              {submitting ? 'Creating…' : 'Create account'}
            </button>
            <button
              className="button button-quiet"
              type="button"
              disabled={submitting}
              onClick={() => {
                setCreating(false)
                setCreateFields({})
              }}
            >
              Cancel
            </button>
          </div>
        </form>
      )}

      {revealed && (
        <CredentialModal
          fullName={revealed.user.full_name}
          password={revealed.password}
          onDismiss={() => setRevealed(null)}
        />
      )}

      {statsFor && (
        <TeacherStatsModal
          teacherID={statsFor.id}
          fullName={statsFor.full_name}
          onDismiss={() => setStatsFor(null)}
        />
      )}

      {rowError && <p className="form-error">{rowError}</p>}

      <div className="admin-filter-row">
        <input
          className="input admin-search"
          type="search"
          placeholder="Search name or email"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          aria-label="Search accounts"
        />
        <select
          className="input"
          value={roleFilter}
          onChange={(e) => setRoleFilter(e.target.value as '' | User['role'])}
          aria-label="Filter by role"
        >
          <option value="">All roles</option>
          <option value="admin">Admin</option>
          <option value="teacher">Teacher</option>
          <option value="student">Student</option>
        </select>
        <select
          className="input"
          value={statusFilter}
          onChange={(e) => setStatusFilter(e.target.value as '' | User['status'])}
          aria-label="Filter by status"
        >
          <option value="">All statuses</option>
          <option value="active">Active</option>
          <option value="disabled">Disabled</option>
        </select>
      </div>

      {visible.length === 0 ? (
        <section className="panel empty-state">
          <h2 className="card-title">No accounts match</h2>
          <p className="hint">Adjust the search or filters, or provision the first account.</p>
        </section>
      ) : (
        <section className="panel table-panel">
          <div className="quiz-table admin-user-table" role="table" aria-label="Accounts">
            <div className="qt-head" role="row">
              <span role="columnheader">User</span>
              <span role="columnheader">Role</span>
              <span role="columnheader">Status</span>
              <span role="columnheader" aria-label="Actions" />
            </div>
            {visible.map((user) => {
              const status = statusOf(user)
              return (
                <div
                  key={user.id}
                  className={`qt-row${user.status === 'disabled' ? ' admin-user-row-disabled' : ''}`}
                  role="row"
                >
                  <span className="admin-user-cell">
                    <span className="avatar avatar-small" aria-hidden="true">
                      {initials(user.full_name)}
                    </span>
                    <span className="admin-user-identity">
                      <span className="admin-user-name" title={user.full_name}>
                        {user.full_name}
                      </span>
                      <span className="admin-user-email" title={user.email}>
                        {user.email}
                      </span>
                    </span>
                  </span>
                  <span className="admin-user-role">{ROLE_LABEL[user.role]}</span>
                  <span>
                    <span className={`status-dot status-dot-${status.tone}`}>
                      {status.label}
                    </span>
                  </span>
                  <span className="qt-actions">
                    {user.role === 'teacher' && (
                      <button
                        className="button button-small button-quiet"
                        type="button"
                        onClick={() => setStatsFor(user)}
                      >
                        Activity
                      </button>
                    )}
                    {user.role !== 'admin' && (
                      <>
                        <button
                          className="button button-small button-quiet"
                          type="button"
                          disabled={busyUserID === user.id}
                          onClick={() => void resetPassword(user)}
                        >
                          Reset password
                        </button>
                        <button
                          className={`button button-small ${
                            user.status === 'active' ? 'button-quiet-danger' : 'button-quiet'
                          }`}
                          type="button"
                          disabled={busyUserID === user.id}
                          onClick={() => void toggleStatus(user)}
                        >
                          {user.status === 'active' ? 'Disable' : 'Re-enable'}
                        </button>
                      </>
                    )}
                  </span>
                </div>
              )
            })}
          </div>
        </section>
      )}
    </div>
  )
}
