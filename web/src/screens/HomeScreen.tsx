import { useState } from 'react'
import { useAuth, type SessionUser } from '../auth/context'

const ROLE_LABEL: Record<SessionUser['role'], string> = {
  admin: 'Administrator',
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

export default function HomeScreen({ user }: { user: SessionUser }) {
  const { logout } = useAuth()
  const [signingOut, setSigningOut] = useState(false)

  return (
    <main className="shell">
      <section className="card auth-card">
        <header className="masthead">
          <span className="brand-mark" aria-hidden="true">
            M
          </span>
          <div>
            <p className="eyebrow">MacQuiz</p>
            <h1 className="page-title">Home</h1>
          </div>
        </header>

        <div className="identity-row">
          <span className="avatar" aria-hidden="true">
            {initials(user.full_name)}
          </span>
          <div className="identity-text">
            <p className="identity-name">{user.full_name}</p>
            <p className="identity-email">{user.email}</p>
          </div>
          <span className="chip chip-role">{ROLE_LABEL[user.role]}</span>
        </div>

        <p className="hint">
          You are signed in. The {ROLE_LABEL[user.role].toLowerCase()} workspace
          arrives with a later milestone
          {user.role === 'admin' && ' (user and group management)'}.
        </p>

        <button
          className="button button-quiet"
          type="button"
          disabled={signingOut}
          onClick={() => {
            setSigningOut(true)
            void logout()
          }}
        >
          {signingOut ? 'Signing out…' : 'Sign out'}
        </button>
      </section>
    </main>
  )
}
