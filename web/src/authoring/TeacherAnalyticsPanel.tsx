import { useEffect, useMemo, useState } from 'react'
import { api } from '../api/client'
import type { components } from '../api/schema'
import { downloadCsv } from '../lib/csv'

type TeacherStats = components['schemas']['TeacherStats']
type StudentPerformance = components['schemas']['TeacherStudentPerformance']

type Sort = 'name' | 'score' | 'violations' | 'recent'

const SUBMITTED_LABEL = new Intl.DateTimeFormat(undefined, {
  day: 'numeric',
  month: 'short',
  year: 'numeric',
})

const PCT = (fraction: number | null | undefined): string =>
  fraction === null || fraction === undefined
    ? '—'
    : `${Math.round(fraction * 100)}%`

/** Score percentages arrive already 0-100, unlike the 0-1 fractions PCT takes. */
const SCORE = (percentValue: number | null | undefined): string =>
  percentValue === null || percentValue === undefined
    ? '—'
    : `${Math.round(percentValue)}%`

function lastActivity(at: string | null | undefined): string {
  return at ? SUBMITTED_LABEL.format(new Date(at)) : '—'
}

/** Sort null scores last so students with no graded work never lead. */
function byNumber(left: number | null | undefined, right: number | null | undefined): number {
  if (left === null || left === undefined) return right === null || right === undefined ? 0 : 1
  if (right === null || right === undefined) return -1
  return right - left
}

const STATUS_LABEL: Record<StudentPerformance['quizzes'][number]['status'], string> = {
  draft: 'Draft',
  scheduled: 'Scheduled',
  live: 'Live',
  closed: 'Closed',
  archived: 'Archived',
}

/**
 * The teacher analytics tab (docs/07 section 3): the teacher's own
 * activity-and-outcomes summary plus every student assigned to their
 * quizzes, scored on those quizzes only - the server enforces the scoping,
 * this panel just renders it. Rows expand into the per-quiz breakdown so a
 * surprising average is one click from its evidence.
 */
export default function TeacherAnalyticsPanel({ teacherId }: { teacherId: string }) {
  const [stats, setStats] = useState<TeacherStats | null>(null)
  const [students, setStudents] = useState<StudentPerformance[] | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)

  const [search, setSearch] = useState('')
  const [sort, setSort] = useState<Sort>('name')
  const [expanded, setExpanded] = useState<Set<string>>(new Set())

  useEffect(() => {
    let cancelled = false
    const load = async () => {
      const [statsResult, rosterResult] = await Promise.all([
        api
          .GET('/api/v1/analytics/teachers/{id}', { params: { path: { id: teacherId } } })
          .catch(() => null),
        api
          .GET('/api/v1/analytics/teachers/{id}/students', {
            params: { path: { id: teacherId } },
          })
          .catch(() => null),
      ])
      if (cancelled) return
      if (!statsResult?.data || !rosterResult?.data) {
        setLoadError('Could not load analytics. Reload to retry.')
        return
      }
      setStats(statsResult.data)
      setStudents(rosterResult.data.students)
    }
    void load()
    return () => {
      cancelled = true
    }
  }, [teacherId])

  const visible = useMemo(() => {
    const needle = search.trim().toLowerCase()
    const rows = (students ?? []).filter(
      (s) =>
        !needle ||
        s.full_name.toLowerCase().includes(needle) ||
        s.email.toLowerCase().includes(needle),
    )
    const sorted = [...rows]
    switch (sort) {
      case 'score':
        sorted.sort((a, b) => byNumber(a.avg_score_percent, b.avg_score_percent))
        break
      case 'violations':
        sorted.sort((a, b) => b.total_violations - a.total_violations)
        break
      case 'recent':
        sorted.sort((a, b) =>
          (b.last_submitted_at ?? '').localeCompare(a.last_submitted_at ?? ''),
        )
        break
      default: // name: the server already sorted by name
    }
    return sorted
  }, [students, search, sort])

  const toggle = (id: string) =>
    setExpanded((prev) => {
      const next = new Set(prev)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })

  const exportCsv = () =>
    downloadCsv('student-performance.csv', [
      ['student', 'email', 'quiz', 'quiz_status', 'score_percent', 'submitted_at'],
      ...visible.flatMap((s) =>
        s.quizzes.map((q) => [
          s.full_name, s.email, q.title, q.status,
          q.score_percent === null || q.score_percent === undefined
            ? ''
            : Math.round(q.score_percent),
          q.submitted_at ?? '',
        ]),
      ),
    ])

  if (loadError) return <p className="form-error">{loadError}</p>
  if (!stats || !students) {
    return (
      <p className="boot-note" role="status">
        Loading…
      </p>
    )
  }

  return (
    <div className="quiz-list">
      <div className="page-head">
        <div>
          <p className="eyebrow">Teacher workspace</p>
          <h1 className="page-title">Analytics</h1>
        </div>
        <button className="button button-quiet" type="button" onClick={exportCsv}>
          Export CSV
        </button>
      </div>

      <div className="stat-cards">
        <div className="stat-card">
          <span className="stat-card-value tabular">{stats.quizzes_conducted}</span>
          <span className="stat-card-label">Quizzes conducted</span>
        </div>
        <div className="stat-card">
          <span className="stat-card-value tabular">{stats.total_attempts.toLocaleString()}</span>
          <span className="stat-card-label">Student attempts</span>
        </div>
        <div className="stat-card">
          <span className="stat-card-value tabular">{PCT(stats.avg_participation)}</span>
          <span className="stat-card-label">Avg participation</span>
        </div>
        <div className="stat-card">
          <span className="stat-card-value tabular">{students.length}</span>
          <span className="stat-card-label">Students assigned</span>
        </div>
      </div>

      <div className="admin-filter-row">
        <input
          className="input admin-search"
          type="search"
          placeholder="Search name or email"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          aria-label="Search students"
        />
        <select
          className="input"
          value={sort}
          onChange={(e) => setSort(e.target.value as Sort)}
          aria-label="Sort students"
        >
          <option value="name">By name</option>
          <option value="score">Highest score</option>
          <option value="violations">Most violations</option>
          <option value="recent">Recently active</option>
        </select>
      </div>

      {visible.length === 0 ? (
        <section className="panel empty-state">
          <h2 className="card-title">
            {students.length === 0 ? 'No students yet' : 'Nothing matches'}
          </h2>
          <p className="hint">
            {students.length === 0
              ? 'Students appear here once they are assigned to one of your quizzes.'
              : 'Adjust the search or the sort.'}
          </p>
        </section>
      ) : (
        <section className="panel table-panel">
          <div className="quiz-table teacher-analytics-table" role="table" aria-label="Student performance">
            <div className="qt-head" role="row">
              <span role="columnheader">Student</span>
              <span role="columnheader" className="qt-num">Assigned</span>
              <span role="columnheader" className="qt-num">Completed</span>
              <span role="columnheader" className="qt-num">Avg score</span>
              <span role="columnheader" className="qt-num">Violations</span>
              <span role="columnheader">Last activity</span>
              <span role="columnheader" aria-label="Actions" />
            </div>
            {visible.map((s) => (
              <div key={s.student_id} className="teacher-analytics-group">
                <div className="qt-row" role="row">
                  <span className="admin-user-identity">
                    <span className="admin-user-name" title={s.full_name}>{s.full_name}</span>
                    <span className="admin-user-email" title={s.email}>{s.email}</span>
                  </span>
                  <span className="qt-num tabular">{s.assigned_quizzes}</span>
                  <span className="qt-num tabular">{s.completed_quizzes}</span>
                  <span className="qt-num tabular">{SCORE(s.avg_score_percent)}</span>
                  <span
                    className={`qt-num tabular${s.total_violations > 0 ? ' teacher-analytics-violations' : ''}`}
                  >
                    {s.total_violations}
                  </span>
                  <span className="teacher-analytics-activity">{lastActivity(s.last_submitted_at)}</span>
                  <span className="qt-actions">
                    <button
                      className="button button-small button-quiet"
                      type="button"
                      aria-expanded={expanded.has(s.student_id)}
                      onClick={() => toggle(s.student_id)}
                    >
                      {expanded.has(s.student_id) ? 'Hide' : 'Quizzes'}
                    </button>
                  </span>
                </div>
                {expanded.has(s.student_id) && (
                  <div className="teacher-analytics-breakdown" role="rowgroup">
                    {s.quizzes.map((q) => (
                      <div key={q.quiz_id} className="teacher-analytics-breakdown-row" role="row">
                        <span className="qt-title" title={q.title}>{q.title}</span>
                        <span className={`chip chip-lifecycle chip-lifecycle-${q.status}`}>
                          {STATUS_LABEL[q.status]}
                        </span>
                        <span className="qt-num tabular">{SCORE(q.score_percent)}</span>
                        <span className="teacher-analytics-activity">
                          {lastActivity(q.submitted_at)}
                        </span>
                      </div>
                    ))}
                  </div>
                )}
              </div>
            ))}
          </div>
        </section>
      )}
    </div>
  )
}
