import { useEffect, useRef, useState } from 'react'
import { useAuth, type SessionUser } from '../auth/context'
import { useNotifySocket } from '../lib/useNotifySocket'
import AssignedList from './AssignedList'
import AttemptPlayer, { type PlayerEntry } from './AttemptPlayer'
import ResultReview from './ResultReview'
import MyAnalytics from './MyAnalytics'
import SdcTeamPanel from '../components/SdcTeamPanel'
import '../authoring/authoring.css'
import './player.css'

type View =
  | { kind: 'list' }
  | { kind: 'player'; entry: PlayerEntry }
  | { kind: 'result'; attemptId: string }
  | { kind: 'analytics' }
  | { kind: 'team' }

interface Notice {
  id: number
  text: string
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
  const [notices, setNotices] = useState<Notice[]>([])
  const noticeSeq = useRef(0)
  const viewRef = useRef(view)
  useEffect(() => {
    viewRef.current = view
  }, [view])

  const toList = () => {
    setListEpoch((n) => n + 1)
    setView({ kind: 'list' })
  }

  // The user:{id}:notify channel (docs/05 section 3): open for the whole
  // signed-in session, not just the list view, since a teacher can assign or
  // unassign a quiz while the student is mid-attempt on something else.
  useNotifySocket(user.id, (msg) => {
    if (msg.type !== 'quiz.assigned' && msg.type !== 'quiz.unassigned') return
    const p = msg.payload as { title?: string }
    const title = p.title?.trim() || 'a quiz'
    const text =
      msg.type === 'quiz.assigned'
        ? `You've been assigned "${title}".`
        : `"${title}" is no longer assigned to you.`
    noticeSeq.current += 1
    setNotices((prev) => [...prev, { id: noticeSeq.current, text }])
    // The assigned list is stale the moment the audience changes; if it
    // is on screen right now, remount it immediately rather than waiting
    // for the student to navigate away and back.
    if (viewRef.current.kind === 'list') setListEpoch((n) => n + 1)
  })

  const dismissNotice = (id: number) => {
    setNotices((prev) => prev.filter((n) => n.id !== id))
  }

  // docs/11 section 5, "calm under pressure": during a live attempt nothing
  // competes with the timer, so the exam chrome replaces the whole shell -
  // no rail, no notices, no sign-out sitting next to a running clock.
  if (view.kind === 'player') {
    return <AttemptPlayer entry={view.entry} onExit={toList} />
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
            className={`rail-item${view.kind !== 'analytics' && view.kind !== 'team' ? ' rail-item-active' : ''}`}
            type="button"
            aria-current={view.kind !== 'analytics' && view.kind !== 'team' ? 'page' : undefined}
            onClick={toList}
          >
            <span className="rail-dot" aria-hidden="true" />
            Assigned quizzes
          </button>
          <button
            className={`rail-item${view.kind === 'analytics' ? ' rail-item-active' : ''}`}
            type="button"
            aria-current={view.kind === 'analytics' ? 'page' : undefined}
            onClick={() => setView({ kind: 'analytics' })}
          >
            <span className="rail-dot" aria-hidden="true" />
            My analytics
          </button>
          <button
            className={`rail-item${view.kind === 'team' ? ' rail-item-active' : ''}`}
            type="button"
            aria-current={view.kind === 'team' ? 'page' : undefined}
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
        {notices.length > 0 && (
          <div className="workspace-notices">
            {notices.map((notice) => (
              <p className="quiz-banner" key={notice.id}>
                {notice.text}
                <button
                  className="quiz-banner-dismiss"
                  type="button"
                  onClick={() => dismissNotice(notice.id)}
                  aria-label="Dismiss"
                >
                  ×
                </button>
              </p>
            ))}
          </div>
        )}
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
        {view.kind === 'result' && (
          <ResultReview attemptId={view.attemptId} onBack={toList} />
        )}
        {view.kind === 'analytics' && <MyAnalytics studentId={user.id} />}
        {view.kind === 'team' && <SdcTeamPanel eyebrow="Student workspace" />}
      </main>
    </div>
  )
}
