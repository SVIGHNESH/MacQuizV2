import { useEffect, useState } from 'react'
import { api } from '../api/client'
import type { components } from '../api/schema'

type StudentStats = components['schemas']['StudentStats']

// The rollup lands async (docs/07); a student with no terminal quiz yet has
// no row at all, which the server reports as 404 rather than an empty body.
const NO_ROLLUP = Symbol('no-rollup')

const TREND_LIMIT = 12

const TREND_LABEL = new Intl.DateTimeFormat(undefined, {
  day: 'numeric',
  month: 'short',
})

/** A 0-1 fraction as a whole percentage; null stays null, never zero. */
function asPercent(fraction: number | null): number | null {
  return fraction === null ? null : Math.round(fraction * 100)
}

/**
 * The rollup stores milliseconds. A fast quiz rounds to "0s", which reads as
 * "no data" rather than "very fast", so anything under a second says so.
 */
function formatPace(milliseconds: number | null): string {
  if (milliseconds === null) return '—'
  if (milliseconds < 1000) return '<1s'
  const seconds = Math.round(milliseconds / 1000)
  if (seconds < 60) return `${seconds}s`
  return `${Math.floor(seconds / 60)}m ${seconds % 60}s`
}

/**
 * St6: the student's own cross-quiz rollup. Every number here is served, not
 * derived from a guess - when the rollup has not landed the screen says so
 * rather than rendering zeroes.
 */
export default function MyAnalytics({ studentId }: { studentId: string }) {
  const [stats, setStats] = useState<StudentStats | typeof NO_ROLLUP | null>(
    null,
  )
  const [loadError, setLoadError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      const result = await api
        .GET('/api/v1/analytics/students/{id}', {
          params: { path: { id: studentId } },
        })
        .catch(() => null)
      if (cancelled) return
      if (result?.data) {
        setStats(result.data)
        return
      }
      if (result?.response.status === 404) {
        setStats(NO_ROLLUP)
        return
      }
      setLoadError(
        result?.error?.message ?? 'Could not load your analytics. Retry later.',
      )
    })()
    return () => {
      cancelled = true
    }
  }, [studentId])

  if (loadError) return <p className="form-error">{loadError}</p>
  if (stats === null) {
    return (
      <p className="boot-note" role="status">
        Loading…
      </p>
    )
  }

  if (stats === NO_ROLLUP) {
    return (
      <div className="analytics">
        <div className="page-head">
          <h1 className="page-title">My analytics</h1>
        </div>
        <section className="empty-state">
          <h2 className="card-title">Nothing to summarise yet</h2>
          <p className="hint">
            Once a quiz you attempted closes and is graded, your accuracy,
            pace, and completion appear here.
          </p>
        </section>
      </div>
    )
  }

  const trend = stats.accuracy_trend.slice(-TREND_LIMIT)
  const scored = trend.filter(
    (point): point is (typeof trend)[number] & { accuracy: number } =>
      point.accuracy !== null,
  )
  const averageAccuracy = scored.length
    ? Math.round(
        (scored.reduce((sum, point) => sum + point.accuracy, 0) /
          scored.length) *
          100,
      )
    : null
  const completion = asPercent(stats.completion_rate)

  return (
    <div className="analytics">
      <div className="page-head">
        <div>
          <h1 className="page-title">My analytics</h1>
          <p className="assigned-subtitle">
            Across {stats.accuracy_trend.length} completed quiz
            {stats.accuracy_trend.length === 1 ? '' : 'zes'}
          </p>
        </div>
      </div>

      <div className="stat-cards">
        <StatCard
          value={averageAccuracy === null ? '—' : `${averageAccuracy}%`}
          label="Average accuracy"
        />
        <StatCard
          value={formatPace(stats.avg_time_per_question)}
          label="Avg time / question"
        />
        <StatCard
          value={completion === null ? '—' : `${completion}%`}
          label="Completion rate"
        />
        <StatCard
          value={String(stats.accuracy_trend.length)}
          label="Quizzes completed"
        />
      </div>

      <section className="chart-panel">
        <h2 className="chart-title">Accuracy trend</h2>
        {scored.length === 0 ? (
          <p className="hint">
            No graded quiz has a score to plot yet.
          </p>
        ) : (
          <ol className="trend-chart">
            {trend.map((point, index) => {
              const percent = asPercent(point.accuracy)
              const latest = index === trend.length - 1
              return (
                <li className="trend-bar-slot" key={point.quiz_id}>
                  <span className="trend-value tabular">
                    {percent === null ? '—' : `${percent}%`}
                  </span>
                  <div
                    className={`trend-bar${latest ? ' trend-bar-latest' : ''}`}
                    style={{ height: `${percent ?? 0}%` }}
                    role="img"
                    aria-label={`${TREND_LABEL.format(new Date(point.submitted_at))}: ${percent === null ? 'not scored' : `${percent}%`}`}
                  />
                  <span
                    className={`trend-label${latest ? ' trend-label-latest' : ''}`}
                  >
                    {TREND_LABEL.format(new Date(point.submitted_at))}
                  </span>
                </li>
              )
            })}
          </ol>
        )}
      </section>
    </div>
  )
}

function StatCard({ value, label }: { value: string; label: string }) {
  return (
    <div className="stat-card">
      <span className="stat-card-value tabular">{value}</span>
      <span className="stat-card-label">{label}</span>
    </div>
  )
}
