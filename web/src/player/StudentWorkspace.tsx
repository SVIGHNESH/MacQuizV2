import { useState } from 'react'
import { useAuth, type SessionUser } from '../auth/context'
import AssignedList from './AssignedList'
import AttemptPlayer, { type PlayerEntry } from './AttemptPlayer'
import ResultReview from './ResultReview'
import '../authoring/authoring.css'
import './player.css'

type View =
  | { kind: 'list' }
  | { kind: 'player'; entry: PlayerEntry }
  | { kind: 'result'; attemptId: string }

function initials(fullName: string): string {
  return fullName
    .split(/\s+/)
    .filter(Boolean)
    .slice(0, 2)
    .map((part) => part[0]!.toUpperCase())
    .join('')
}

/**
 * The signed-in student shell: the same fixed rail as the teacher workspace
 * (docs/11) with the one student destination, and the assigned list, the
 * attempt player, or the released review as the content.
 */
export default function StudentWorkspace({ user }: { user: SessionUser }) {
  const { logout } = useAuth()
  const [view, setView] = useState<View>({ kind: 'list' })
  const [signingOut, setSigningOut] = useState(false)
  // Remounts the list on every return to it, so it always refetches: an
  // attempt just submitted or results just released must show immediately.
  const [listEpoch, setListEpoch] = useState(0)

  const toList = () => {
    setListEpoch((n) => n + 1)
    setView({ kind: 'list' })
  }

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
            className="rail-item rail-item-active"
            type="button"
            onClick={toList}
          >
            <span className="rail-dot" aria-hidden="true" />
            My quizzes
          </button>
        </nav>

        <div className="rail-user">
          <div className="rail-identity">
            <span className="avatar avatar-small" aria-hidden="true">
              {initials(user.full_name)}
            </span>
            <span className="rail-identity-text">
              <span className="rail-user-name">{user.full_name}</span>
              <span className="chip chip-role">Student</span>
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
        {view.kind === 'list' && (
          <AssignedList
            key={listEpoch}
            onStart={(quizId) =>
              setView({ kind: 'player', entry: { kind: 'start', quizId } })
            }
            onResume={(attemptId) =>
              setView({ kind: 'player', entry: { kind: 'resume', attemptId } })
            }
            onReview={(attemptId) => setView({ kind: 'result', attemptId })}
          />
        )}
        {view.kind === 'player' && (
          <AttemptPlayer entry={view.entry} onExit={toList} />
        )}
        {view.kind === 'result' && (
          <ResultReview attemptId={view.attemptId} onBack={toList} />
        )}
      </main>
    </div>
  )
}
