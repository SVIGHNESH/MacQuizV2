import { useState } from 'react'
import { useAuth, type SessionUser } from '../auth/context'
import UsersPanel from './UsersPanel'
import GroupsPanel from './GroupsPanel'
import '../authoring/authoring.css'
import './admin.css'

type View = 'users' | 'groups'

function initials(fullName: string): string {
  return fullName
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((part) => part[0]!.toUpperCase())
    .join('')
}

/**
 * The signed-in admin shell (Milestone 1: user and group management): a
 * fixed sidebar rail matching the teacher/student workspaces, switching
 * between account provisioning and cohort management.
 */
export default function AdminWorkspace({ user }: { user: SessionUser }) {
  const { logout } = useAuth()
  const [view, setView] = useState<View>('users')
  const [signingOut, setSigningOut] = useState(false)

  return (
    <div className="workspace">
      <aside className="rail">
        <div className="rail-brand">
          <span className="brand-mark brand-mark-small" aria-hidden="true">
            M
          </span>
          <span className="rail-brand-name">MacQuiz</span>
        </div>

        <nav className="rail-nav" aria-label="Workspace">
          <button
            className={`rail-item${view === 'users' ? ' rail-item-active' : ''}`}
            type="button"
            onClick={() => setView('users')}
          >
            <span className="rail-dot" aria-hidden="true" />
            Users
          </button>
          <button
            className={`rail-item${view === 'groups' ? ' rail-item-active' : ''}`}
            type="button"
            onClick={() => setView('groups')}
          >
            <span className="rail-dot" aria-hidden="true" />
            Groups
          </button>
        </nav>

        <div className="rail-user">
          <div className="rail-identity">
            <span className="avatar avatar-small" aria-hidden="true">
              {initials(user.full_name)}
            </span>
            <span className="rail-identity-text">
              <span className="rail-user-name">{user.full_name}</span>
              <span className="chip chip-role">Admin</span>
            </span>
          </div>
          <button
            className="button button-quiet rail-signout"
            type="button"
            disabled={signingOut}
            onClick={() => {
              setSigningOut(true)
              void logout()
            }}
          >
            {signingOut ? 'Signing out…' : 'Sign out'}
          </button>
        </div>
      </aside>

      <main className="workspace-main">
        {view === 'users' ? <UsersPanel /> : <GroupsPanel />}
      </main>
    </div>
  )
}
