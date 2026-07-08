import { useEffect, useState } from 'react'
import { api } from '../api/client'
import type { components } from '../api/schema'

type OrgStats = components['schemas']['OrgStats']

const WEEK_FORMAT = new Intl.DateTimeFormat(undefined, { day: 'numeric', month: 'short' })

const PCT = (fraction: number | null | undefined): string =>
  fraction === null || fraction === undefined ? '—' : `${Math.round(fraction * 100)}%`

/**
 * The admin org-wide dashboard (docs/07 section 4, FR-9): active users,
 * quizzes-created trend, platform-wide participation, and a per-cohort
 * comparison. Reuses the stats-* primitives QuizStatsPanel (Milestone 8)
 * introduced, since this is the same "read one already-computed summary"
 * shape at the org level instead of the per-quiz level.
 */
export default function OrgStatsPanel() {
  const [stats, setStats] = useState<OrgStats | null>(null)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      const result = await api.GET('/api/v1/analytics/org').catch(() => null)
      if (cancelled) return
      if (!result?.data) {
        setError(result?.error?.message ?? 'Could not load the org dashboard.')
        return
      }
      setStats(result.data)
    })()
    return () => {
      cancelled = true
    }
  }, [])

  if (error) return <p className="form-error">{error}</p>
  if (!stats) {
    return (
      <p className="boot-note" role="status">
        Loading…
      </p>
    )
  }

  const maxWeek = Math.max(1, ...stats.quizzes_per_week.map((w) => w.count))

  return (
    <div className="quiz-list">
      <div className="page-head">
        <div>
          <p className="eyebrow">Admin console</p>
          <h1 className="page-title">Overview</h1>
        </div>
      </div>

      <section className="panel stats-panel" aria-label="Org-wide analytics">
        <span className="card-title">Active users</span>
        <div className="stats-summary">
          <div className="stat-tile">
            <span className="stat-tile-label">Admins</span>
            <span className="stat-tile-value tabular">{stats.active_users.admins}</span>
          </div>
          <div className="stat-tile">
            <span className="stat-tile-label">Teachers</span>
            <span className="stat-tile-value tabular">{stats.active_users.teachers}</span>
          </div>
          <div className="stat-tile">
            <span className="stat-tile-label">Students</span>
            <span className="stat-tile-value tabular">{stats.active_users.students}</span>
          </div>
          <div className="stat-tile">
            <span className="stat-tile-label">Platform participation</span>
            <span className="stat-tile-value tabular">{PCT(stats.platform_participation)}</span>
          </div>
        </div>

        {stats.quizzes_per_week.length > 0 && (
          <>
            <span className="field-label">Quizzes created per week</span>
            <div className="stats-distribution" role="img" aria-label="Quizzes created per week">
              {stats.quizzes_per_week.map((week) => (
                <div key={week.week_start} className="stats-bar-col">
                  <div
                    className="stats-bar"
                    style={{ height: `${(week.count / maxWeek) * 100}%` }}
                    title={`Week of ${WEEK_FORMAT.format(new Date(week.week_start))}: ${week.count}`}
                  />
                  <span className="stats-bar-label tabular">
                    {WEEK_FORMAT.format(new Date(week.week_start))}
                  </span>
                </div>
              ))}
            </div>
          </>
        )}
      </section>

      {stats.cohort_comparisons.length === 0 ? (
        <section className="panel empty-state">
          <h2 className="card-title">No cohorts yet</h2>
          <p className="hint">Create a group on the Groups tab to compare cohorts here.</p>
        </section>
      ) : (
        <section className="panel table-panel">
          <div className="quiz-table admin-cohort-table" role="table" aria-label="Cohort comparisons">
            <div className="qt-head" role="row">
              <span role="columnheader">Cohort</span>
              <span role="columnheader" className="qt-num">
                Members
              </span>
              <span role="columnheader" className="qt-num">
                Avg completion
              </span>
              <span role="columnheader" className="qt-num">
                Avg accuracy
              </span>
            </div>
            {stats.cohort_comparisons.map((cohort) => (
              <div key={cohort.group_id} className="qt-row" role="row">
                <span className="qt-title">{cohort.group_name}</span>
                <span className="qt-num tabular">{cohort.member_count}</span>
                <span className="qt-num tabular">{PCT(cohort.avg_completion_rate)}</span>
                <span className="qt-num tabular">{PCT(cohort.avg_accuracy)}</span>
              </div>
            ))}
          </div>
        </section>
      )}
    </div>
  )
}
