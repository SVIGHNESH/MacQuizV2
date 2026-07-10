import { useEffect, useRef, useState, type ChangeEvent } from 'react'
import { api } from '../api/client'
import { downloadCsv } from '../lib/csv'
import type { Import, TeacherQuestion } from './model'

const POLL_MS = 1200

/** The "downloadable error report" of docs/07 section 2. */
function downloadErrorReport(imp: Import) {
  downloadCsv(`import-errors-${imp.id}.csv`, [
    ['row', 'column', 'problem'],
    ...(imp.error_report ?? []).map((r) => [r.row, r.column, r.message]),
  ])
}

type Phase =
  | { kind: 'idle' }
  | { kind: 'uploading' }
  | { kind: 'polling'; imp: Import }
  | { kind: 'ready'; imp: Import }
  | { kind: 'committing'; imp: Import }
  | { kind: 'failed'; imp: Import }

/**
 * The Milestone 7 bulk-import review UI (docs/07 section 2): upload a CSV or
 * XLSX template, poll the import until the worker resolves it to
 * ready/failed, then either commit the validated rows as questions or show
 * the row-level error report so the teacher can fix the file and try again.
 */
export default function ImportPanel({
  quizId,
  onCommitted,
}: {
  quizId: string
  onCommitted: (questions: TeacherQuestion[]) => void
}) {
  const [phase, setPhase] = useState<Phase>({ kind: 'idle' })
  const [error, setError] = useState<string | null>(null)
  const fileInput = useRef<HTMLInputElement>(null)

  useEffect(() => {
    if (phase.kind !== 'polling') return
    let cancelled = false
    const timer = setTimeout(async () => {
      const result = await api
        .GET('/api/v1/imports/{id}', {
          params: { path: { id: phase.imp.id } },
        })
        .catch(() => null)
      if (cancelled) return
      if (!result?.data) {
        setError(
          result?.error?.message ?? 'Could not check the import status.',
        )
        return
      }
      const imp = result.data.import
      if (imp.status === 'ready') setPhase({ kind: 'ready', imp })
      else if (imp.status === 'failed') setPhase({ kind: 'failed', imp })
      else setPhase({ kind: 'polling', imp })
    }, POLL_MS)
    return () => {
      cancelled = true
      clearTimeout(timer)
    }
  }, [phase])

  const upload = async (file: File) => {
    setError(null)
    setPhase({ kind: 'uploading' })
    // The server tells CSV and XLSX apart by sniffing the file's own bytes
    // (quiz.ParseImportFile), not this header - it only needs to be a
    // generic binary-safe type so no intermediary tries to re-encode the
    // upload as text.
    const contentType = file.name.toLowerCase().endsWith('.xlsx')
      ? 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet'
      : 'text/csv'
    const result = await api
      .POST('/api/v1/quizzes/{id}/imports', {
        params: { path: { id: quizId } },
        headers: { 'Content-Type': contentType },
        bodySerializer: (body) => body,
        body: file as unknown as string,
      })
      .catch(() => null)
    if (!result?.data) {
      setError(
        result?.error?.fields?.file ??
          result?.error?.message ??
          'Could not upload the file.',
      )
      setPhase({ kind: 'idle' })
      return
    }
    setPhase({ kind: 'polling', imp: result.data.import })
  }

  const onPick = (e: ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    e.target.value = ''
    if (file) void upload(file)
  }

  const commit = async (importId: string, imp: Import) => {
    setPhase({ kind: 'committing', imp })
    setError(null)
    const result = await api
      .POST('/api/v1/imports/{id}/commit', {
        params: { path: { id: importId } },
      })
      .catch(() => null)
    if (!result?.data) {
      setError(result?.error?.message ?? 'Could not commit the import.')
      setPhase({ kind: 'ready', imp })
      return
    }
    onCommitted(result.data.questions)
    setPhase({ kind: 'idle' })
  }

  const reset = () => {
    setPhase({ kind: 'idle' })
    setError(null)
  }

  return (
    <section className="panel import-panel" aria-label="Bulk import">
      <span className="field-label">Bulk import (CSV or XLSX)</span>

      {phase.kind === 'idle' && (
        <div className="import-upload-row">
          <button
            className="button button-quiet"
            type="button"
            onClick={() => fileInput.current?.click()}
          >
            Upload CSV or XLSX
          </button>
          <input
            ref={fileInput}
            type="file"
            accept=".csv,text/csv,.xlsx,application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
            hidden
            onChange={onPick}
          />
        </div>
      )}

      {phase.kind === 'uploading' && <p className="hint">Uploading…</p>}

      {phase.kind === 'polling' && (
        <p className="hint" role="status">
          Validating the file…
        </p>
      )}

      {(phase.kind === 'ready' || phase.kind === 'committing') && (
        <div className="import-result">
          <p className="hint">
            {phase.imp.row_count}{' '}
            {phase.imp.row_count === 1 ? 'question' : 'questions'} validated
            and ready to add.
          </p>
          <div className="import-actions">
            <button
              className="button button-primary"
              type="button"
              disabled={phase.kind === 'committing'}
              onClick={() => void commit(phase.imp.id, phase.imp)}
            >
              {phase.kind === 'committing'
                ? 'Adding…'
                : `Add ${phase.imp.row_count} question${phase.imp.row_count === 1 ? '' : 's'}`}
            </button>
            <button
              className="button button-quiet"
              type="button"
              disabled={phase.kind === 'committing'}
              onClick={reset}
            >
              Cancel
            </button>
          </div>
        </div>
      )}

      {phase.kind === 'failed' && (
        <div className="import-result">
          <p className="form-error">
            This file has errors and nothing was imported.
          </p>
          <div
            className="import-error-table"
            role="table"
            aria-label="Import errors"
          >
            {(phase.imp.error_report ?? []).map((err, i) => (
              <div key={i} className="import-error-row" role="row">
                <span className="import-error-row-num tabular">
                  Row {err.row}
                </span>
                <span className="import-error-col">{err.column}</span>
                <span className="import-error-msg">{err.message}</span>
              </div>
            ))}
          </div>
          <div className="import-actions">
            <button
              className="button button-quiet"
              type="button"
              disabled={(phase.imp.error_report ?? []).length === 0}
              onClick={() => downloadErrorReport(phase.imp)}
            >
              Download error report
            </button>
            <button
              className="button button-quiet import-actions-end"
              type="button"
              onClick={reset}
            >
              Try another file
            </button>
          </div>
        </div>
      )}

      {error && <p className="form-error">{error}</p>}
    </section>
  )
}
