import { useState } from 'react'
import { useAuth, type SessionUser } from '../auth/context'
import { useNotifySocket } from '../lib/useNotifySocket'
import QuizEditor from './QuizEditor'
import QuizList from './QuizList'
import TeacherAnalyticsPanel from './TeacherAnalyticsPanel'
import SdcTeamPanel from '../components/SdcTeamPanel'
import { VIOLATION_LABEL, type ViolationTally } from './model'
import './authoring.css'

type View =
  | { kind: 'list' }
  | { kind: 'editor'; quizId: string }
  | { kind: 'analytics' }
  | { kind: 'team' }

// The attempt.violation_alert payload (docs/05 section 3): the violation
// ladder's notify action, addressed to this quiz's owner.
interface ViolationAlert {
  attempt_id: string
  quiz_title: string
  student_name: string
  violation_type: ViolationTally['type']
  violation_count: number
}

function alertText(a: ViolationAlert): string {
  const what = VIOLATION_LABEL[a.violation_type] ?? a.violation_type
  return `${a.student_name}: ${what.toLowerCase()} - violation ${a.violation_count} on "${a.quiz_title}".`
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
 * The signed-in teacher shell: a fixed sidebar rail (docs/11) with the one
 * authoring destination for now, and the quiz list / editor as the content.
 */
export default function AuthoringWorkspace({ user }: { user: SessionUser }) {
  const { logout } = useAuth()
  const [view, setView] = useState<View>({ kind: 'list' })
  const [signingOut, setSigningOut] = useState(false)
  // The violation ladder's notify action (docs/06 section 3), keyed by attempt:
  // the server re-alerts on every counted violation past the threshold, and a
  // stack of "violation 3", "violation 4" for one student would bury the other
  // students. One banner per attempt, showing that student's latest count.
  const [alerts, setAlerts] = useState<ViolationAlert[]>([])

  // The teacher's own user:{id}:notify channel, held open across the whole
  // workspace rather than in the live monitor: a guardrail trips while the
  // teacher is writing next week's quiz, which is exactly when they most need
  // to be told (docs/05 section 3).
  useNotifySocket(user.id, (msg) => {
    if (msg.type !== 'attempt.violation_alert') return
    const alert = msg.payload as ViolationAlert
    setAlerts((prev) => [
      ...prev.filter((a) => a.attempt_id !== alert.attempt_id),
      alert,
    ])
  })

  const dismissAlert = (attemptId: string) => {
    setAlerts((prev) => prev.filter((a) => a.attempt_id !== attemptId))
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
        {alerts.length > 0 && (
          <div className="workspace-notices">
            {alerts.map((alert) => (
              <p className="notify-banner" key={alert.attempt_id} role="status">
                {alertText(alert)}
                <button
                  className="notify-banner-dismiss"
                  type="button"
                  onClick={() => dismissAlert(alert.attempt_id)}
                  aria-label="Dismiss"
                >
                  ×
                </button>
              </p>
            ))}
          </div>
        )}
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
