import { useState } from 'react'
import { useAuth, type SessionUser } from '../auth/context'
import UsersPanel from './UsersPanel'
import GroupsPanel from './GroupsPanel'
import OrgStatsPanel from './OrgStatsPanel'
import AuditPanel from './AuditPanel'
import '../authoring/authoring.css'
import './admin.css'

type View = 'overview' | 'users' | 'groups' | 'audit'

function initials(fullName: string): string {
  return fullName
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((part) => part[0]!.toUpperCase())
    .join('')
}

/**
 * The signed-in admin shell: a fixed sidebar rail matching the teacher and
 * student workspaces, switching between the org overview, account
 * provisioning, cohort management, and the append-only audit log. Read-only
 * analytics, no authoring (docs/11 section 6).
 */
export default function AdminWorkspace({ user }: { user: SessionUser }) {
  const { logout } = useAuth()
  const [view, setView] = useState<View>('overview')
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
            className={`rail-item${view === 'overview' ? ' rail-item-active' : ''}`}
            type="button"
            onClick={() => setView('overview')}
          >
            <span className="rail-dot" aria-hidden="true" />
            Overview
          </button>
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
          <button
            className={`rail-item${view === 'audit' ? ' rail-item-active' : ''}`}
            type="button"
            onClick={() => setView('audit')}
          >
            <span className="rail-dot" aria-hidden="true" />
            Audit log
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
        {view === 'overview' ? (
          <OrgStatsPanel />
        ) : view === 'users' ? (
          <UsersPanel />
        ) : view === 'groups' ? (
          <GroupsPanel />
        ) : (
          <AuditPanel />
        )}
      </main>
    </div>
  )
}
