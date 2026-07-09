import { useCallback, useEffect, useState } from 'react'
import { api } from '../api/client'
import DestructiveConfirmModal from '../components/DestructiveConfirmModal'
import type { components } from '../api/schema'
import type { QuizStats, TeacherQuestion } from './model'

type ResultRow = components['schemas']['ResultRow']

type Phase =
  | { kind: 'loading' }
  | { kind: 'unavailable' }
  | { kind: 'error'; message: string }
  | { kind: 'loaded'; stats: QuizStats }

const PCT = (fraction: number | null | undefined): string =>
  fraction === null || fraction === undefined
    ? '—'
    : `${Math.round(fraction * 100)}%`

/**
 * Milestone 8's teacher dashboard (docs/04 "GET /analytics/quizzes/:id"):
 * the score distribution, mean/median/participation, per-question item
 * analysis, and integrity summary RollupDue froze at close. One read of the
 * already-computed rollup, never a live recompute.
 */
export default function QuizStatsPanel({
  quizId,
  quizTitle,
  questions,
}: {
  quizId: string
  quizTitle: string
  questions: TeacherQuestion[]
}) {
  const [phase, setPhase] = useState<Phase>({ kind: 'loading' })
  const [kicked, setKicked] = useState<ResultRow[] | null>(null)
  const [busyAttemptId, setBusyAttemptId] = useState<string | null>(null)
  const [overrideError, setOverrideError] = useState<string | null>(null)
  const [pendingOverride, setPendingOverride] = useState<{
    attemptId: string
    studentName: string
  } | null>(null)

  useEffect(() => {
    let cancelled = false
    setPhase({ kind: 'loading' })
    ;(async () => {
      const result = await api
        .GET('/api/v1/analytics/quizzes/{id}', {
          params: { path: { id: quizId } },
        })
        .catch(() => null)
      if (cancelled) return
      if (!result?.data) {
        if (result?.response.status === 404) {
          setPhase({ kind: 'unavailable' })
          return
        }
        setPhase({
          kind: 'error',
          message: result?.error?.message ?? 'Could not load quiz analytics.',
        })
        return
      }
      setPhase({ kind: 'loaded', stats: result.data })
    })()
    return () => {
      cancelled = true
    }
  }, [quizId])

  // The kicked-attempts table (docs/06 line 80's score-override control) only
  // makes sense once there is at least one kicked attempt to show, so it
  // reads GET /quizzes/:id/results itself rather than growing the analytics
  // rollup endpoint with per-attempt detail it doesn't otherwise carry.
  const loadKicked = useCallback(async () => {
    const result = await api
      .GET('/api/v1/quizzes/{id}/results', { params: { path: { id: quizId } } })
      .catch(() => null)
    if (result?.data) {
      setKicked(result.data.results.filter((row) => row.submit_kind === 'kicked'))
    }
  }, [quizId])

  useEffect(() => {
    if (phase.kind === 'loaded' && phase.stats.integrity.kicked_attempts > 0) {
      void loadKicked()
    }
  }, [phase, loadKicked])

  const overrideScore = async (reason: string) => {
    if (!pendingOverride) return
    const { attemptId } = pendingOverride
    setBusyAttemptId(attemptId)
    setOverrideError(null)
    const result = await api
      .POST('/api/v1/attempts/{id}/override-score', {
        params: { path: { id: attemptId } },
        body: { reason },
      })
      .catch(() => null)
    setBusyAttemptId(null)
    if (!result?.data) {
      setOverrideError(
        result?.error?.message ?? 'Could not override this attempt’s score.',
      )
      return
    }
    setPendingOverride(null)
    void loadKicked()
  }

  if (phase.kind === 'loading') {
    return (
      <p className="boot-note" role="status">
        Loading analytics…
      </p>
    )
  }

  if (phase.kind === 'unavailable') {
    return (
      <p className="hint">
        Analytics show up once this quiz closes and its results are rolled
        up.
      </p>
    )
  }

  if (phase.kind === 'error') {
    return <p className="form-error">{phase.message}</p>
  }

  const { stats } = phase
  const questionText = (id: string) =>
    questions.find((q) => q.id === id)?.body.text ?? id

  const maxBucket = Math.max(1, ...stats.distribution)

  return (
    <section className="panel stats-panel" aria-label="Quiz analytics">
      <div className="stats-panel-head">
        <span className="card-title">Analytics</span>
        <a
          className="button button-quiet"
          href={`/api/v1/quizzes/${quizId}/results.csv`}
          download
        >
          Download results CSV
        </a>
      </div>

      <div className="stats-summary">
        <div className="stat-tile">
          <span className="stat-tile-label">Mean score</span>
          <span className="stat-tile-value tabular">
            {stats.mean === null ? '—' : stats.mean.toFixed(1)}
          </span>
        </div>
        <div className="stat-tile">
          <span className="stat-tile-label">Median score</span>
          <span className="stat-tile-value tabular">
            {stats.median === null ? '—' : stats.median.toFixed(1)}
          </span>
        </div>
        <div className="stat-tile">
          <span className="stat-tile-label">Participation</span>
          <span className="stat-tile-value tabular">
            {PCT(stats.participation)}
          </span>
        </div>
      </div>

      <div className="stats-distribution" role="img" aria-label="Score distribution">
        {stats.distribution.map((count, i) => (
          <div key={i} className="stats-bar-col">
            <div
              className="stats-bar"
              style={{ height: `${(count / maxBucket) * 100}%` }}
              title={`${i * 10}-${(i + 1) * 10}%: ${count}`}
            />
            <span className="stats-bar-label tabular">{i * 10}</span>
          </div>
        ))}
      </div>

      {stats.item_analysis.length > 0 && (
        <div className="stats-item-table" role="table" aria-label="Item analysis">
          <div className="stats-item-head" role="row">
            <span role="columnheader">Question</span>
            <span role="columnheader" className="qt-num">
              Responses
            </span>
            <span role="columnheader" className="qt-num">
              Difficulty
            </span>
            <span role="columnheader" className="qt-num">
              Discrimination
            </span>
            <span role="columnheader" className="qt-num">
              Avg time
            </span>
          </div>
          {stats.item_analysis.map((item) => (
            <div key={item.question_id} className="stats-item-row" role="row">
              <span className="stats-item-question">
                {questionText(item.question_id)}
              </span>
              <span className="qt-num tabular">{item.responses}</span>
              <span className="qt-num tabular">{PCT(item.p_value)}</span>
              <span className="qt-num tabular">
                {item.point_biserial === null
                  ? '—'
                  : item.point_biserial.toFixed(2)}
              </span>
              <span className="qt-num tabular">
                {(item.avg_time_ms / 1000).toFixed(1)}s
              </span>
            </div>
          ))}
        </div>
      )}

      <div className="stats-integrity">
        <span className="field-label">Integrity</span>
        <div className="stats-summary">
          <div className="stat-tile">
            <span className="stat-tile-label">Kicked attempts</span>
            <span className="stat-tile-value tabular">
              {stats.integrity.kicked_attempts}
            </span>
          </div>
          <div className="stat-tile">
            <span className="stat-tile-label">Flagged students</span>
            <span className="stat-tile-value tabular">
              {stats.integrity.flagged_students}
            </span>
          </div>
          <div className="stat-tile">
            <span className="stat-tile-label">Total violations</span>
            <span className="stat-tile-value tabular">
              {stats.integrity.total_violations}
            </span>
          </div>
        </div>
      </div>

      {stats.integrity.kicked_attempts > 0 && (
        <div
          className="stats-item-table kicked-attempts-table"
          role="table"
          aria-label="Kicked attempts"
        >
          <span className="field-label">Kicked attempts</span>
          {overrideError && <p className="form-error">{overrideError}</p>}
          <div className="stats-item-head" role="row">
            <span role="columnheader">Student</span>
            <span role="columnheader" className="qt-num">
              Score
            </span>
            <span role="columnheader"></span>
          </div>
          {(kicked ?? []).map((row) => (
            <div key={row.attempt_id} className="stats-item-row" role="row">
              <span className="stats-item-question">{row.full_name}</span>
              <span className="qt-num tabular">
                {row.score === null ? '—' : row.score}
              </span>
              <span className="qt-num">
                {row.score_overridden ? (
                  <span className="hint">Score overridden to 0</span>
                ) : row.status !== 'graded' ? (
                  <span className="hint">Grading pending</span>
                ) : (
                  <button
                    className="button button-quiet-danger button-small"
                    type="button"
                    disabled={busyAttemptId === row.attempt_id}
                    onClick={() =>
                      setPendingOverride({
                        attemptId: row.attempt_id!,
                        studentName: row.full_name,
                      })
                    }
                  >
                    Override to zero
                  </button>
                )}
              </span>
            </div>
          ))}
        </div>
      )}

      {pendingOverride && (
        <DestructiveConfirmModal
          title="Confirm score override"
          subtitle={`${pendingOverride.studentName} · ${quizTitle}`}
          reasonLabel="Reason for zeroing this attempt's score"
          consequence="This sets the attempt's score to zero and cannot be undone."
          confirmLabel="Override to zero"
          busy={busyAttemptId === pendingOverride.attemptId}
          error={overrideError}
          onCancel={() => setPendingOverride(null)}
          onConfirm={(reason) => void overrideScore(reason)}
        />
      )}
    </section>
  )
}
