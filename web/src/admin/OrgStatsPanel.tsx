import { useEffect, useState } from 'react'
import { api } from '../api/client'
import type { components } from '../api/schema'

type OrgStats = components['schemas']['OrgStats']
type Week = OrgStats['quizzes_per_week'][number]

const WEEK_FORMAT = new Intl.DateTimeFormat(undefined, {
  day: 'numeric',
  month: 'short',
})

const WEEK_MS = 7 * 24 * 60 * 60 * 1000

const PCT = (fraction: number | null | undefined): string =>
  fraction === null || fraction === undefined
    ? '—'
    : `${Math.round(fraction * 100)}%`

/**
 * The server omits a week in which no quiz was created ("a week with no
 * quizzes created is simply absent"), so plotting the array as it arrives
 * would silently close the gap and draw a flat, wrong trend. Re-expand the
 * range at a 7-day step and put the zeroes back.
 */
function fillWeeks(weeks: Week[]): Week[] {
  if (weeks.length === 0) return []
  const byStart = new Map(
    weeks.map((week) => [Date.parse(week.week_start), week.count]),
  )
  const first = Math.min(...byStart.keys())
  const last = Math.max(...byStart.keys())
  // Guard the step loop against a week_start that is off the 7-day grid: an
  // unbounded chart is worse than an unfilled one.
  if (last - first > 60 * WEEK_MS) return weeks
  const filled: Week[] = []
  for (let at = first; at <= last; at += WEEK_MS) {
    filled.push({
      week_start: new Date(at).toISOString(),
      count: byStart.get(at) ?? 0,
    })
  }
  return filled
}

/** Quizzes created in the calendar month we are currently in. */
function createdThisMonth(weeks: Week[]): number {
  const now = new Date()
  return weeks
    .filter((week) => {
      const start = new Date(week.week_start)
      return (
        start.getFullYear() === now.getFullYear() &&
        start.getMonth() === now.getMonth()
      )
    })
    .reduce((sum, week) => sum + week.count, 0)
}

/**
 * The admin org-wide dashboard (docs/07 section 4, FR-9 / docs/11 "Org
 * overview"): active users as the screen's one inverted hero, the
 * quizzes-created trend, platform-wide participation, and a per-cohort
 * comparison. Read-only - an admin never authors here.
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

  const { admins, teachers, students } = stats.active_users
  const totalUsers = admins + teachers + students
  const weeks = fillWeeks(stats.quizzes_per_week)
  const maxWeek = Math.max(1, ...weeks.map((week) => week.count))

  const roles = [
    { label: 'Students', count: students, fill: 'var(--color-primary)' },
    { label: 'Teachers', count: teachers, fill: 'var(--color-chart-tint-1)' },
    { label: 'Admins', count: admins, fill: 'var(--color-chart-tint-2)' },
  ]

  return (
    <div className="quiz-list">
      <div className="page-head">
        <div>
          <p className="eyebrow">Admin console</p>
          <h1 className="page-title">Overview</h1>
          <p className="admin-subtitle">Org-wide · read-only</p>
        </div>
      </div>

      {/* At most one inverted ink card per screen: the hero number. */}
      <div className="stat-cards">
        <div className="stat-card stat-card-hero">
          <span className="stat-card-eyebrow">Active users</span>
          <span className="stat-card-hero-value tabular">
            {totalUsers.toLocaleString()}
          </span>
          <span className="stat-card-hero-sub tabular">
            {students.toLocaleString()} students · {teachers} teachers
          </span>
        </div>
        <div className="stat-card">
          <span className="stat-card-value tabular">
            {createdThisMonth(stats.quizzes_per_week)}
          </span>
          <span className="stat-card-label">Quizzes this month</span>
        </div>
        <div className="stat-card">
          <span className="stat-card-value tabular">
            {PCT(stats.platform_participation)}
          </span>
          <span className="stat-card-label">Avg participation</span>
        </div>
        <div className="stat-card">
          <span className="stat-card-value tabular">
            {stats.cohort_comparisons.length}
          </span>
          <span className="stat-card-label">Cohorts tracked</span>
        </div>
      </div>

      <div className="admin-overview-grid">
        <section className="chart-panel">
          <span className="eyebrow">Quizzes created per week</span>
          {weeks.length === 0 ? (
            <p className="hint">No quiz has been created yet.</p>
          ) : (
            <ol
              className="week-chart"
              role="img"
              aria-label={`Quizzes created per week over the last ${weeks.length} weeks`}
            >
              {weeks.map((week, index) => (
                <li className="week-bar-slot" key={week.week_start}>
                  <div
                    className={`week-bar${index === weeks.length - 1 ? ' week-bar-latest' : ''}`}
                    style={{ height: `${(week.count / maxWeek) * 100}%` }}
                    title={`Week of ${WEEK_FORMAT.format(new Date(week.week_start))}: ${week.count}`}
                  />
                  <span className="week-label">
                    {WEEK_FORMAT.format(new Date(week.week_start))}
                  </span>
                </li>
              ))}
            </ol>
          )}
        </section>

        <section className="chart-panel">
          <span className="eyebrow">Users by role</span>
          <div className="role-bars">
            {roles.map((role) => (
              <div className="role-bar" key={role.label}>
                <div className="role-bar-head">
                  <span className="role-bar-label">{role.label}</span>
                  <span className="role-bar-count tabular">
                    {role.count.toLocaleString()}
                  </span>
                </div>
                <div className="role-bar-track">
                  <div
                    className="role-bar-fill"
                    style={{
                      width: `${totalUsers === 0 ? 0 : (role.count / totalUsers) * 100}%`,
                      background: role.fill,
                    }}
                  />
                </div>
              </div>
            ))}
          </div>
        </section>
      </div>

      {stats.cohort_comparisons.length === 0 ? (
        <section className="panel empty-state">
          <h2 className="card-title">No cohorts yet</h2>
          <p className="hint">
            Create a group on the Groups tab to compare cohorts here.
          </p>
        </section>
      ) : (
        <section className="panel table-panel">
          <div
            className="quiz-table admin-cohort-table"
            role="table"
            aria-label="Cohort comparisons"
          >
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
                <span className="qt-num tabular">
                  {PCT(cohort.avg_completion_rate)}
                </span>
                <span className="qt-num tabular">{PCT(cohort.avg_accuracy)}</span>
              </div>
            ))}
          </div>
        </section>
      )}
    </div>
  )
}
