import { useRef, useState, type ChangeEvent, type DragEvent } from 'react'
import { api } from '../api/client'
import type { components } from '../api/schema'
import { downloadCsv } from '../lib/csv'

type ProvisionedUser = components['schemas']['ProvisionedUser']
type RowError = NonNullable<components['schemas']['UserImportError']['row_errors']>[number]

/** Mirrors the server's MaxUserImportBytes so an oversized pick fails fast. */
const MAX_FILE_BYTES = 1 << 20

const TEMPLATE_ROWS = [
  ['role', 'email', 'full_name'],
  ['student', 'priya.sharma@school.example', 'Priya Sharma'],
  ['student', 'rahul.verma@school.example', 'Rahul Verma'],
  ['teacher', 'anita.desai@school.example', 'Anita Desai'],
]

function downloadCredentials(users: readonly ProvisionedUser[]) {
  downloadCsv('account-credentials.csv', [
    ['role', 'email', 'full_name', 'one_time_password'],
    ...users.map((u) => [u.user.role, u.user.email, u.user.full_name, u.initial_password ?? '']),
  ])
}

type Phase =
  | { kind: 'pick' }
  | { kind: 'uploading' }
  | { kind: 'done'; users: ProvisionedUser[] }

/**
 * Bulk account provisioning from a CSV/XLSX roster. The upload commits
 * all-or-nothing on the server, so this modal only ever shows one of two
 * outcomes: every row created (with the one-time credentials, shown exactly
 * once, downloadable as a CSV) or a row-level error report and nothing
 * created.
 */
export default function UserImportModal({
  onCreated,
  onDismiss,
}: {
  /** Fires once on a successful import, before the credentials are shown. */
  onCreated: (users: ProvisionedUser[]) => void
  onDismiss: () => void
}) {
  const [phase, setPhase] = useState<Phase>({ kind: 'pick' })
  const [file, setFile] = useState<File | null>(null)
  const [dragging, setDragging] = useState(false)
  const [fileError, setFileError] = useState<string | null>(null)
  const [rowErrors, setRowErrors] = useState<RowError[] | null>(null)
  const fileInput = useRef<HTMLInputElement>(null)

  const pick = (picked: File) => {
    setFileError(null)
    setRowErrors(null)
    if (!/\.(csv|xlsx)$/i.test(picked.name)) {
      setFile(null)
      setFileError('must be a .csv or .xlsx file')
      return
    }
    if (picked.size > MAX_FILE_BYTES) {
      setFile(null)
      setFileError('must be 1 MB or smaller')
      return
    }
    setFile(picked)
  }

  const onDrop = (e: DragEvent) => {
    e.preventDefault()
    setDragging(false)
    const dropped = e.dataTransfer.files?.[0]
    if (dropped) pick(dropped)
  }

  const onBrowse = (e: ChangeEvent<HTMLInputElement>) => {
    const picked = e.target.files?.[0]
    e.target.value = ''
    if (picked) pick(picked)
  }

  const upload = async () => {
    if (!file) return
    setPhase({ kind: 'uploading' })
    setFileError(null)
    setRowErrors(null)
    // The server tells CSV and XLSX apart by sniffing the file's own bytes;
    // this header only needs to be a binary-safe type so no intermediary
    // tries to re-encode the upload as text.
    const contentType = file.name.toLowerCase().endsWith('.xlsx')
      ? 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet'
      : 'text/csv'
    const result = await api
      .POST('/api/v1/users/import', {
        headers: { 'Content-Type': contentType },
        bodySerializer: (body) => body,
        body: file as unknown as string,
      })
      .catch(() => null)
    if (result?.data) {
      onCreated(result.data.users)
      // The credentials exist only in this response, so the CSV is saved
      // for the admin immediately rather than trusting them to click.
      downloadCredentials(result.data.users)
      setPhase({ kind: 'done', users: result.data.users })
      return
    }
    setPhase({ kind: 'pick' })
    const error = result?.error
    // The 422 body may carry the roster's per-row report; 401/403 use the
    // plain Error envelope, hence the narrowing.
    const reported = error && 'row_errors' in error ? error.row_errors : undefined
    if (reported?.length) {
      setRowErrors(reported)
      return
    }
    setFileError(
      error?.fields?.file ?? error?.message ?? 'Could not upload the roster.',
    )
  }

  if (phase.kind === 'done') {
    return (
      <div className="modal-overlay" role="alertdialog" aria-modal="true">
        <div className="modal-panel admin-import-modal">
          <span className="chip chip-lifecycle chip-lifecycle-submitted">
            <span className="chip-dot" aria-hidden="true" />
            {phase.users.length} {phase.users.length === 1 ? 'account' : 'accounts'} created
          </span>
          <h2 className="credential-title">The roster is in</h2>
          <p className="body-copy">
            A CSV with each account's email and one-time password
            (<code>account-credentials.csv</code>) was just downloaded - keep
            it safe. The credentials are shown <strong>exactly once</strong>{' '}
            and cannot be retrieved later.
          </p>

          <div className="admin-import-credentials" role="table" aria-label="One-time credentials">
            <div className="admin-import-credential-head" role="row">
              <span role="columnheader">Account</span>
              <span role="columnheader">One-time credential</span>
            </div>
            {phase.users.map((u) => (
              <div key={u.user.id} className="admin-import-credential-row" role="row">
                <span className="admin-user-identity">
                  <span className="admin-user-name" title={u.user.full_name}>
                    {u.user.full_name}
                  </span>
                  <span className="admin-user-email" title={u.user.email}>
                    {u.user.email}
                  </span>
                </span>
                <code className="admin-credential-value">{u.initial_password}</code>
              </div>
            ))}
          </div>

          <p className="credential-notice">
            <span aria-hidden="true">!</span>
            <span>Every account must reset its password on first login.</span>
          </p>

          <div className="admin-import-actions">
            <button
              className="button button-primary"
              type="button"
              onClick={() => downloadCredentials(phase.users)}
            >
              Download the CSV again
            </button>
            <button className="button button-commit credential-done" type="button" onClick={onDismiss}>
              I've saved them - done
            </button>
          </div>
        </div>
      </div>
    )
  }

  const busy = phase.kind === 'uploading'
  return (
    <div className="modal-overlay" role="dialog" aria-modal="true" aria-label="Import users">
      <div className="modal-panel admin-import-modal">
        <h2 className="credential-title">Import users from a file</h2>
        <p className="body-copy">
          Upload a CSV or XLSX roster with the columns <code>role</code>,{' '}
          <code>email</code>, and <code>full_name</code>; role is{' '}
          <code>teacher</code> or <code>student</code>. All accounts are
          created together - if any row has a problem, nothing is created.
        </p>
        <button
          className="button button-small button-quiet admin-import-template"
          type="button"
          onClick={() => downloadCsv('user-import-template.csv', TEMPLATE_ROWS)}
        >
          Download example CSV
        </button>

        <div
          className={`admin-import-dropzone${dragging ? ' admin-import-dropzone-active' : ''}`}
          onDragOver={(e) => {
            e.preventDefault()
            setDragging(true)
          }}
          onDragLeave={() => setDragging(false)}
          onDrop={onDrop}
        >
          <p className="admin-import-dropzone-hint">
            {file ? (
              <>
                <strong>{file.name}</strong> ({Math.max(1, Math.round(file.size / 1024))} KB)
              </>
            ) : (
              'Drag and drop the roster here'
            )}
          </p>
          <button
            className="button button-small button-quiet"
            type="button"
            disabled={busy}
            onClick={() => fileInput.current?.click()}
          >
            {file ? 'Choose a different file' : 'Choose a file'}
          </button>
          <input
            ref={fileInput}
            type="file"
            accept=".csv,text/csv,.xlsx,application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
            hidden
            onChange={onBrowse}
          />
        </div>

        {fileError && <p className="form-error">{fileError}</p>}

        {rowErrors && (
          <div className="admin-import-report">
            <p className="form-error">The roster has errors and no accounts were created.</p>
            <div className="import-error-table" role="table" aria-label="Roster errors">
              {rowErrors.map((err, i) => (
                <div key={i} className="import-error-row" role="row">
                  <span className="import-error-row-num tabular">Row {err.row}</span>
                  <span className="import-error-col">{err.column}</span>
                  <span className="import-error-msg">{err.message}</span>
                </div>
              ))}
            </div>
            <button
              className="button button-small button-quiet"
              type="button"
              onClick={() =>
                downloadCsv('user-import-errors.csv', [
                  ['row', 'column', 'problem'],
                  ...rowErrors.map((r) => [r.row, r.column, r.message]),
                ])
              }
            >
              Download error report
            </button>
          </div>
        )}

        <div className="admin-import-actions">
          <button
            className="button button-primary"
            type="button"
            disabled={!file || busy}
            onClick={() => void upload()}
          >
            {busy ? 'Creating accounts…' : 'Create accounts'}
          </button>
          <button className="button button-quiet" type="button" disabled={busy} onClick={onDismiss}>
            Cancel
          </button>
        </div>
      </div>
    </div>
  )
}
