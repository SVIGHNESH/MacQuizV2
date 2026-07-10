import { useEffect, useState } from 'react'
import { api } from '../api/client'
import type { components } from '../api/schema'

type TeacherStats = components['schemas']['TeacherStats']

const PCT = (fraction: number | null | undefined): string =>
  fraction === null || fraction === undefined
    ? '—'
    : `${Math.round(fraction * 100)}%`

/**
 * Raw points, not a percentage: quiz_stats stores each quiz's mean in points
 * and keeps no percentage mean to average instead (see the server's
 * AvgClassScore note), so the unit has to be said out loud rather than
 * silently rendered as a share.
 */
const POINTS = (mean: number | null | undefined): string =>
  mean === null || mean === undefined ? '—' : mean.toFixed(1)

/**
 * Publish-to-results latency arrives in seconds and spans minutes to days
 * across a term. Round to the largest unit that keeps the number legible -
 * "2.4 d" reads, "207,360 s" does not.
 */
function latency(seconds: number | null | undefined): string {
  if (seconds === null || seconds === undefined) return '—'
  if (seconds < 120) return `${Math.round(seconds)} s`
  const minutes = seconds / 60
  if (minutes < 90) return `${minutes.toFixed(0)} min`
  const hours = minutes / 60
  if (hours < 48) return `${hours.toFixed(1)} h`
  return `${(hours / 24).toFixed(1)} d`
}

/**
 * The per-teacher drill-down (docs/07 section 4, "Per teacher: quizzes
 * created/conducted, total student attempts, average participation, average
 * class score, publish-to-results latency - Admin"; FR-9's "all teachers").
 *
 * The admin band is read-only analytics, so this is a dismissible overlay on
 * the Users table rather than a fifth navigable view: it drills into one row
 * of a list the admin is already looking at, and there is nothing to author.
 * A teacher who has created nothing is a legitimate 200 with zero counts and
 * null averages, which is why every average renders an em dash rather than a
 * zero when the server has nothing to average.
 */
export default function TeacherStatsModal({
  teacherID,
  fullName,
  onDismiss,
}: {
  teacherID: string
  fullName: string
  onDismiss: () => void
}) {
  const [stats, setStats] = useState<TeacherStats | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      const result = await api
        .GET('/api/v1/analytics/teachers/{id}', {
          params: { path: { id: teacherID } },
        })
        .catch(() => null)
      if (cancelled) return
      if (!result?.data) {
        setError(result?.error?.message ?? "Could not load this teacher's activity.")
        return
      }
      setStats(result.data)
    })()
    return () => {
      cancelled = true
    }
  }, [teacherID])

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onDismiss()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onDismiss])

  return (
    <div className="modal-overlay" role="presentation" onClick={onDismiss}>
      <div
        className="modal-panel admin-teacher-stats"
        role="dialog"
        aria-modal="true"
        aria-label={`Activity for ${fullName}`}
        onClick={(event) => event.stopPropagation()}
      >
        <p className="eyebrow">Teacher activity</p>
        <h2 className="modal-title">{fullName}</h2>
        <p className="modal-subtitle">Read-only · aggregated live</p>

        {error && <p className="form-error">{error}</p>}
        {!stats && !error && (
          <p className="boot-note" role="status">
            Loading…
          </p>
        )}

        {stats && (
          <>
            <div className="stat-cards">
              <div className="stat-card">
                <span className="stat-card-value tabular">{stats.quizzes_created}</span>
                <span className="stat-card-label">Quizzes created</span>
              </div>
              <div className="stat-card">
                <span className="stat-card-value tabular">{stats.quizzes_conducted}</span>
                <span className="stat-card-label">Quizzes conducted</span>
              </div>
              <div className="stat-card">
                <span className="stat-card-value tabular">
                  {stats.total_attempts.toLocaleString()}
                </span>
                <span className="stat-card-label">Student attempts</span>
              </div>
            </div>

            <dl className="teacher-stat-rows">
              <div className="teacher-stat-row">
                <dt>Avg participation</dt>
                <dd className="tabular">{PCT(stats.avg_participation)}</dd>
              </div>
              <div className="teacher-stat-row">
                <dt>
                  Avg class score
                  <span className="teacher-stat-unit">points</span>
                </dt>
                <dd className="tabular">{POINTS(stats.avg_class_score)}</dd>
              </div>
              <div className="teacher-stat-row">
                <dt>Publish → results</dt>
                <dd className="tabular">{latency(stats.avg_publish_to_results_sec)}</dd>
              </div>
            </dl>

            <p className="hint">
              Averages cover only quizzes whose statistics have been rolled up; a
              teacher with none reads as an em dash, not a zero.
            </p>
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
