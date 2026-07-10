import { useState } from 'react'
import { useAuth, type SessionUser } from '../auth/context'
import QuizEditor from './QuizEditor'
import QuizList from './QuizList'
import TeacherAnalyticsPanel from './TeacherAnalyticsPanel'
import SdcTeamPanel from '../components/SdcTeamPanel'
import './authoring.css'

type View =
  | { kind: 'list' }
  | { kind: 'editor'; quizId: string }
  | { kind: 'analytics' }
  | { kind: 'team' }

function initials(fullName: string): string {
  return fullName
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((part) => part[0]!.toUpperCase())
    .join('')
}

/**
 * The signed-in teacher shell: a fixed sidebar rail (docs/11) with the one
 * authoring destination for now, and the quiz list / editor as the content.
 */
export default function AuthoringWorkspace({ user }: { user: SessionUser }) {
  const { logout } = useAuth()
  const [view, setView] = useState<View>({ kind: 'list' })
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
            className={`rail-item${view.kind !== 'analytics' && view.kind !== 'team' ? ' rail-item-active' : ''}`}
            type="button"
            onClick={() => setView({ kind: 'list' })}
          >
            <span className="rail-dot" aria-hidden="true" />
            Quizzes
          </button>
          <button
            className={`rail-item${view.kind === 'analytics' ? ' rail-item-active' : ''}`}
            type="button"
            onClick={() => setView({ kind: 'analytics' })}
          >
            <span className="rail-dot" aria-hidden="true" />
            Analytics
          </button>
          <button
            className={`rail-item${view.kind === 'team' ? ' rail-item-active' : ''}`}
            type="button"
            onClick={() => setView({ kind: 'team' })}
          >
            <span className="rail-dot" aria-hidden="true" />
            SDC Team
          </button>
        </nav>

        <div className="rail-user">
          <div className="rail-identity">
            <span className="avatar avatar-small" aria-hidden="true">
              {initials(user.full_name)}
            </span>
            <span className="rail-identity-text">
              <span className="rail-user-name">{user.full_name}</span>
              <span className="chip chip-role">Teacher</span>
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
        {view.kind === 'list' ? (
          <QuizList onOpen={(quizId) => setView({ kind: 'editor', quizId })} />
        ) : view.kind === 'analytics' ? (
          <TeacherAnalyticsPanel teacherId={user.id} />
        ) : view.kind === 'team' ? (
          <SdcTeamPanel eyebrow="Teacher workspace" />
        ) : (
          <QuizEditor
            quizId={view.quizId}
            onBack={() => setView({ kind: 'list' })}
          />
        )}
      </main>
    </div>
  )
}
