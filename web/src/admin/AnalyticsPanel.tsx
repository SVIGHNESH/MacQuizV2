import { useEffect, useMemo, useState } from 'react'
import { api } from '../api/client'
import type { components } from '../api/schema'
import { downloadCsv } from '../lib/csv'
import TeacherStatsModal from './TeacherStatsModal'
import StudentStatsModal from './StudentStatsModal'

type TeacherOverview = components['schemas']['TeacherOverview']
type StudentOverview = components['schemas']['StudentOverview']
type Group = components['schemas']['Group']

type Subject = 'teachers' | 'students'

type TeacherSort = 'name' | 'quizzes' | 'attempts' | 'participation' | 'score'
type StudentSort = 'name' | 'accuracy' | 'completion' | 'quizzes' | 'pace'

/** A 0-1 fraction as a whole-percent label; null reads as an em-dash-free dash. */
function percent(fraction: number | null | undefined): string {
  return fraction === null || fraction === undefined ? '—' : `${Math.round(fraction * 100)}%`
}

function points(value: number | null | undefined): string {
  return value === null || value === undefined ? '—' : value.toFixed(1)
}

/** The rollup stores milliseconds per question (see MyAnalytics.formatPace). */
function pace(milliseconds: number | null | undefined): string {
  if (milliseconds === null || milliseconds === undefined) return '—'
  if (milliseconds < 1000) return '<1s'
  const seconds = Math.round(milliseconds / 1000)
  if (seconds < 60) return `${seconds}s`
  return `${Math.floor(seconds / 60)}m ${seconds % 60}s`
}

/** Sort null averages last regardless of direction, so idle rows never lead. */
function byNumber(left: number | null | undefined, right: number | null | undefined): number {
  if (left === null || left === undefined) return right === null || right === undefined ? 0 : 1
  if (right === null || right === undefined) return -1
  return right - left
}

/**
 * The admin analytics tab (docs/07 section 4, docs/11 section 6
 * "Teacher/admin analytics"): every teacher's activity-and-outcomes row and
 * every student's cross-quiz profile row, filterable client-side (the lists
 * are role-bounded, not unbounded event data) and exportable as CSV. Each
 * row drills into the same per-teacher/per-student stats the rest of the
 * console uses, so this tab can never disagree with them.
 */
export default function AnalyticsPanel() {
  const [subject, setSubject] = useState<Subject>('teachers')
  const [teachers, setTeachers] = useState<TeacherOverview[] | null>(null)
  const [students, setStudents] = useState<StudentOverview[] | null>(null)
  const [groups, setGroups] = useState<Group[]>([])
  const [loadError, setLoadError] = useState<string | null>(null)

  const [search, setSearch] = useState('')
  const [cohortFilter, setCohortFilter] = useState('')
  const [teacherSort, setTeacherSort] = useState<TeacherSort>('name')
  const [studentSort, setStudentSort] = useState<StudentSort>('name')

  const [teacherDetail, setTeacherDetail] = useState<TeacherOverview | null>(null)
  const [studentDetail, setStudentDetail] = useState<StudentOverview | null>(null)

  useEffect(() => {
    let cancelled = false
    const load = async () => {
      const [teacherResult, studentResult, groupResult] = await Promise.all([
        api.GET('/api/v1/analytics/teachers').catch(() => null),
        api.GET('/api/v1/analytics/students').catch(() => null),
        api.GET('/api/v1/groups').catch(() => null),
      ])
      if (cancelled) return
      if (!teacherResult?.data || !studentResult?.data) {
        setLoadError('Could not load analytics. Reload to retry.')
        return
      }
      setTeachers(teacherResult.data.teachers)
      setStudents(studentResult.data.students)
      setGroups(groupResult?.data?.groups ?? [])
    }
    void load()
    return () => {
      cancelled = true
    }
  }, [])

  const needle = search.trim().toLowerCase()
  const matches = (fullName: string, email: string) =>
    !needle || fullName.toLowerCase().includes(needle) || email.toLowerCase().includes(needle)

  const visibleTeachers = useMemo(() => {
    const rows = (teachers ?? []).filter((t) => matches(t.full_name, t.email))
    const sorted = [...rows]
    switch (teacherSort) {
      case 'quizzes':
        sorted.sort((a, b) => b.quizzes_created - a.quizzes_created)
        break
      case 'attempts':
        sorted.sort((a, b) => b.total_attempts - a.total_attempts)
        break
      case 'participation':
        sorted.sort((a, b) => byNumber(a.avg_participation, b.avg_participation))
        break
      case 'score':
        sorted.sort((a, b) => byNumber(a.avg_class_score, b.avg_class_score))
        break
      default: // name: the server already sorted by name
    }
    return sorted
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [teachers, needle, teacherSort])

  const visibleStudents = useMemo(() => {
    const rows = (students ?? []).filter(
      (s) =>
        matches(s.full_name, s.email) &&
        (!cohortFilter || s.group_ids.includes(cohortFilter)),
    )
    const sorted = [...rows]
    switch (studentSort) {
      case 'accuracy':
        sorted.sort((a, b) => byNumber(a.avg_accuracy, b.avg_accuracy))
        break
      case 'completion':
        sorted.sort((a, b) => byNumber(a.completion_rate, b.completion_rate))
        break
      case 'quizzes':
        sorted.sort((a, b) => b.quizzes_taken - a.quizzes_taken)
        break
      case 'pace':
        // Fastest first; null (no data) last, matching byNumber's convention.
        sorted.sort((a, b) => -byNumber(a.avg_time_per_question, b.avg_time_per_question))
        break
      default:
    }
    return sorted
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [students, needle, cohortFilter, studentSort])

  const groupName = (id: string) => groups.find((g) => g.id === id)?.name ?? '—'

  const exportCsv = () => {
    if (subject === 'teachers') {
      downloadCsv('teacher-analytics.csv', [
        ['full_name', 'email', 'status', 'quizzes_created', 'quizzes_conducted',
          'total_attempts', 'avg_participation', 'avg_class_score'],
        ...visibleTeachers.map((t) => [
          t.full_name, t.email, t.status, t.quizzes_created, t.quizzes_conducted,
          t.total_attempts, t.avg_participation ?? '', t.avg_class_score ?? '',
        ]),
      ])
      return
    }
    downloadCsv('student-analytics.csv', [
      ['full_name', 'email', 'status', 'cohorts', 'quizzes_taken',
        'avg_accuracy', 'completion_rate', 'avg_time_per_question_ms'],
      ...visibleStudents.map((s) => [
        s.full_name, s.email, s.status,
        s.group_ids.map(groupName).join('; '),
        s.quizzes_taken, s.avg_accuracy ?? '', s.completion_rate ?? '',
        s.avg_time_per_question ?? '',
      ]),
    ])
  }

  if (loadError) return <p className="form-error">{loadError}</p>
  if (!teachers || !students) {
    return (
      <p className="boot-note" role="status">
        Loading…
      </p>
    )
  }

  const visibleCount = subject === 'teachers' ? visibleTeachers.length : visibleStudents.length

  return (
    <div className="quiz-list">
      <div className="page-head">
        <div>
          <p className="eyebrow">Admin console</p>
          <h1 className="page-title">Analytics</h1>
        </div>
        <button className="button button-quiet" type="button" onClick={exportCsv}>
          Export CSV
        </button>
      </div>

      <div className="admin-analytics-toggle" role="tablist" aria-label="Analytics subject">
        <button
          className={`admin-toggle-item${subject === 'teachers' ? ' admin-toggle-item-active' : ''}`}
          type="button"
          role="tab"
          aria-selected={subject === 'teachers'}
          onClick={() => setSubject('teachers')}
        >
          Teachers ({teachers.length})
        </button>
        <button
          className={`admin-toggle-item${subject === 'students' ? ' admin-toggle-item-active' : ''}`}
          type="button"
          role="tab"
          aria-selected={subject === 'students'}
          onClick={() => setSubject('students')}
        >
          Students ({students.length})
        </button>
      </div>

      <div className="admin-filter-row">
        <input
          className="input admin-search"
          type="search"
          placeholder="Search name or email"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          aria-label="Search analytics"
        />
        {subject === 'students' && (
          <select
            className="input"
            value={cohortFilter}
            onChange={(e) => setCohortFilter(e.target.value)}
            aria-label="Filter by cohort"
          >
            <option value="">All cohorts</option>
            {groups.map((g) => (
              <option key={g.id} value={g.id}>
                {g.name}
              </option>
            ))}
          </select>
        )}
        {subject === 'teachers' ? (
          <select
            className="input"
            value={teacherSort}
            onChange={(e) => setTeacherSort(e.target.value as TeacherSort)}
            aria-label="Sort teachers"
          >
            <option value="name">By name</option>
            <option value="quizzes">Most quizzes</option>
            <option value="attempts">Most attempts</option>
            <option value="participation">Highest participation</option>
            <option value="score">Highest class score</option>
          </select>
        ) : (
          <select
            className="input"
            value={studentSort}
            onChange={(e) => setStudentSort(e.target.value as StudentSort)}
            aria-label="Sort students"
          >
            <option value="name">By name</option>
            <option value="accuracy">Highest accuracy</option>
            <option value="completion">Highest completion</option>
            <option value="quizzes">Most quizzes</option>
            <option value="pace">Fastest pace</option>
          </select>
        )}
      </div>

      {teacherDetail && (
        <TeacherStatsModal
          teacherID={teacherDetail.teacher_id}
          fullName={teacherDetail.full_name}
          onDismiss={() => setTeacherDetail(null)}
        />
      )}
      {studentDetail && (
        <StudentStatsModal
          studentID={studentDetail.student_id}
          fullName={studentDetail.full_name}
          onDismiss={() => setStudentDetail(null)}
        />
      )}

      {visibleCount === 0 ? (
        <section className="panel empty-state">
          <h2 className="card-title">Nothing matches</h2>
          <p className="hint">Adjust the search or filters.</p>
        </section>
      ) : subject === 'teachers' ? (
        <section className="panel table-panel">
          <div className="quiz-table admin-analytics-teacher-table" role="table" aria-label="Teacher analytics">
            <div className="qt-head" role="row">
              <span role="columnheader">Teacher</span>
              <span role="columnheader" className="qt-num">Quizzes</span>
              <span role="columnheader" className="qt-num">Conducted</span>
              <span role="columnheader" className="qt-num">Attempts</span>
              <span role="columnheader" className="qt-num">Participation</span>
              <span role="columnheader" className="qt-num">Class score</span>
              <span role="columnheader" aria-label="Actions" />
            </div>
            {visibleTeachers.map((t) => (
              <div
                key={t.teacher_id}
                className={`qt-row${t.status === 'disabled' ? ' admin-user-row-disabled' : ''}`}
                role="row"
              >
                <span className="admin-user-identity">
                  <span className="admin-user-name" title={t.full_name}>{t.full_name}</span>
                  <span className="admin-user-email" title={t.email}>{t.email}</span>
                </span>
                <span className="qt-num tabular">{t.quizzes_created}</span>
                <span className="qt-num tabular">{t.quizzes_conducted}</span>
                <span className="qt-num tabular">{t.total_attempts}</span>
                <span className="qt-num tabular">{percent(t.avg_participation)}</span>
                <span className="qt-num tabular">{points(t.avg_class_score)}</span>
                <span className="qt-actions">
                  <button
                    className="button button-small button-quiet"
                    type="button"
                    onClick={() => setTeacherDetail(t)}
                  >
                    Details
                  </button>
                </span>
              </div>
            ))}
          </div>
        </section>
      ) : (
        <section className="panel table-panel">
          <div className="quiz-table admin-analytics-student-table" role="table" aria-label="Student analytics">
            <div className="qt-head" role="row">
              <span role="columnheader">Student</span>
              <span role="columnheader">Cohorts</span>
              <span role="columnheader" className="qt-num">Quizzes</span>
              <span role="columnheader" className="qt-num">Accuracy</span>
              <span role="columnheader" className="qt-num">Completion</span>
              <span role="columnheader" className="qt-num">Pace</span>
              <span role="columnheader" aria-label="Actions" />
            </div>
            {visibleStudents.map((s) => (
              <div
                key={s.student_id}
                className={`qt-row${s.status === 'disabled' ? ' admin-user-row-disabled' : ''}`}
                role="row"
              >
                <span className="admin-user-identity">
                  <span className="admin-user-name" title={s.full_name}>{s.full_name}</span>
                  <span className="admin-user-email" title={s.email}>{s.email}</span>
                </span>
                <span className="admin-analytics-cohorts">
                  {s.group_ids.length === 0 ? '—' : s.group_ids.map(groupName).join(', ')}
                </span>
                <span className="qt-num tabular">{s.quizzes_taken}</span>
                <span className="qt-num tabular">{percent(s.avg_accuracy)}</span>
                <span className="qt-num tabular">{percent(s.completion_rate)}</span>
                <span className="qt-num tabular">{pace(s.avg_time_per_question)}</span>
                <span className="qt-actions">
                  <button
                    className="button button-small button-quiet"
                    type="button"
                    onClick={() => setStudentDetail(s)}
                  >
                    Details
                  </button>
                </span>
              </div>
            ))}
          </div>
        </section>
      )}
    </div>
  )
}
