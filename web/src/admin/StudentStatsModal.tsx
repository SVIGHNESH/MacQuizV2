import { useEffect, useState } from 'react'
import { api } from '../api/client'
import type { components } from '../api/schema'

type StudentStats = components['schemas']['StudentStats']

// The rollup lands async on quiz close; a student with no terminal quiz yet
// has no row at all, which the server reports as 404 (see MyAnalytics).
const NO_ROLLUP = Symbol('no-rollup')

const TREND_LIMIT = 12

const TREND_LABEL = new Intl.DateTimeFormat(undefined, {
  day: 'numeric',
  month: 'short',
})

const PCT = (fraction: number | null | undefined): string =>
  fraction === null || fraction === undefined
    ? '—'
    : `${Math.round(fraction * 100)}%`

/** The rollup stores milliseconds per question (see MyAnalytics.formatPace). */
function pace(milliseconds: number | null | undefined): string {
  if (milliseconds === null || milliseconds === undefined) return '—'
  if (milliseconds < 1000) return '<1s'
  const seconds = Math.round(milliseconds / 1000)
  if (seconds < 60) return `${seconds}s`
  return `${Math.floor(seconds / 60)}m ${seconds % 60}s`
}

/** Weakest topic first - the one to act on - with ties held stable by tag. */
function weakestFirst(topics: Record<string, number>): [string, number][] {
  return Object.entries(topics).sort(
    ([leftTopic, left], [rightTopic, right]) =>
      left - right || leftTopic.localeCompare(rightTopic),
  )
}

/**
 * The per-student drill-down on the admin analytics tab: the same
 * student_stats rollup the student's own "My analytics" screen reads,
 * rendered with the admin console's chart primitives (week-chart bars for
 * the accuracy trend, role-bars for topic strengths) so nothing here can
 * disagree with what the student sees.
 */
export default function StudentStatsModal({
  studentID,
  fullName,
  onDismiss,
}: {
  studentID: string
  fullName: string
  onDismiss: () => void
}) {
  const [stats, setStats] = useState<StudentStats | typeof NO_ROLLUP | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      const result = await api
        .GET('/api/v1/analytics/students/{id}', {
          params: { path: { id: studentID } },
        })
        .catch(() => null)
      if (cancelled) return
      if (result?.response.status === 404) {
        setStats(NO_ROLLUP)
        return
      }
      if (!result?.data) {
        setError(result?.error?.message ?? "Could not load this student's analytics.")
        return
      }
      setStats(result.data)
    })()
    return () => {
      cancelled = true
    }
  }, [studentID])

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onDismiss()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onDismiss])

  const trend =
    stats && stats !== NO_ROLLUP ? stats.accuracy_trend.slice(-TREND_LIMIT) : []
  const topics =
    stats && stats !== NO_ROLLUP ? weakestFirst(stats.topic_strengths) : []

  return (
    <div className="modal-overlay" role="presentation" onClick={onDismiss}>
      <div
        className="modal-panel admin-teacher-stats"
        role="dialog"
        aria-modal="true"
        aria-label={`Analytics for ${fullName}`}
        onClick={(event) => event.stopPropagation()}
      >
        <p className="eyebrow">Student analytics</p>
        <h2 className="modal-title">{fullName}</h2>
        <p className="modal-subtitle">Read-only · rolled up on quiz close</p>

        {error && <p className="form-error">{error}</p>}
        {!stats && !error && (
          <p className="boot-note" role="status">
            Loading…
          </p>
        )}

        {stats === NO_ROLLUP && (
          <p className="hint">
            No analytics yet - this student has no closed quiz behind them, so
            there is nothing to roll up.
          </p>
        )}

        {stats && stats !== NO_ROLLUP && (
          <>
            <div className="stat-cards">
              <div className="stat-card">
                <span className="stat-card-value tabular">{stats.accuracy_trend.length}</span>
                <span className="stat-card-label">Quizzes taken</span>
              </div>
              <div className="stat-card">
                <span className="stat-card-value tabular">{PCT(stats.completion_rate)}</span>
                <span className="stat-card-label">Completion</span>
              </div>
              <div className="stat-card">
                <span className="stat-card-value tabular">{pace(stats.avg_time_per_question)}</span>
                <span className="stat-card-label">Avg pace / question</span>
              </div>
            </div>

            {trend.length > 0 && (
              <section aria-label="Accuracy trend">
                <span className="field-label">Accuracy trend</span>
                <ul className="week-chart admin-student-trend">
                  {trend.map((point, i) => (
                    <li key={`${point.quiz_id}-${i}`} className="week-bar-slot">
                      <span
                        className={`week-bar${i === trend.length - 1 ? ' week-bar-latest' : ''}`}
                        style={{ height: `${Math.round((point.accuracy ?? 0) * 100)}%` }}
                        title={PCT(point.accuracy)}
                      />
                      <span className="week-label">
                        {TREND_LABEL.format(new Date(point.submitted_at))}
                      </span>
                    </li>
                  ))}
                </ul>
              </section>
            )}

            {topics.length > 0 && (
              <section aria-label="Topic strengths">
                <span className="field-label">Topic strengths (weakest first)</span>
                <div className="role-bars admin-student-topics">
                  {topics.map(([topic, accuracy]) => (
                    <div key={topic} className="role-bar">
                      <div className="role-bar-head">
                        <span className="role-bar-label">{topic}</span>
                        <span className="role-bar-count tabular">{PCT(accuracy)}</span>
                      </div>
                      <div className="role-bar-track">
                        <span
                          className="role-bar-fill"
                          style={{
                            width: `${Math.round(accuracy * 100)}%`,
                            background: 'var(--color-primary)',
                          }}
                        />
                      </div>
                    </div>
                  ))}
                </div>
              </section>
            )}
          </>
        )}

        <div className="modal-actions">
          <button className="button button-quiet" type="button" onClick={onDismiss}>
            Close
          </button>
        </div>
      </div>
    </div>
  )
}
