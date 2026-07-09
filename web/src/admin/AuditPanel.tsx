import { useEffect, useState } from 'react'
import { api } from '../api/client'
import type { components } from '../api/schema'

type AuditEntry = components['schemas']['AuditEntry']

const PAGE_SIZE = 50

const STAMP = new Intl.DateTimeFormat(undefined, {
  day: 'numeric',
  month: 'short',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hour12: false,
})

/**
 * Actions are written as `noun.verb` (quizzes.published, attempt.kicked). The
 * noun is already carried by resource_type, so the row shows the verb and
 * lets the resource column say what it acted on.
 */
function verbOf(action: string): string {
  const verb = action.slice(action.indexOf('.') + 1)
  return verb.replace(/_/g, ' ')
}

// Red is the human decision and its consequences (docs/11 section 5) - the
// irreversible verbs an admin scans this log for, not every mutation.
// Disabling an account is not here: the server writes it as `users.updated`,
// so there is no `disabled` verb to match.
const CONSEQUENTIAL = new Set([
  'kicked',
  'deleted',
  'unassigned',
  'score overridden',
  'session invalidated',
])

/** A uuid is unreadable in full; its first block identifies the row. */
function shortId(id: string): string {
  return id.split('-')[0] ?? id
}

/**
 * The append-only audit trail (docs/04). Keyset-paginated newest first on the
 * entry id, exactly as the server returns it - this screen never sorts or
 * filters client-side, because a log you can silently reorder is not evidence.
 */
export default function AuditPanel() {
  const [entries, setEntries] = useState<AuditEntry[] | null>(null)
  const [actors, setActors] = useState<Map<string, string>>(new Map())
  const [cursor, setCursor] = useState<number | null>(null)
  const [loadingMore, setLoadingMore] = useState(false)
  const [error, setError] = useState<string | null>(null)

  useEffect(() => {
    let cancelled = false
    ;(async () => {
      // The log stores actor_id only; names come from the accounts list the
      // admin can already read.
      const [page, users] = await Promise.all([
        api
          .GET('/api/v1/audit', { params: { query: { limit: PAGE_SIZE } } })
          .catch(() => null),
        api.GET('/api/v1/users').catch(() => null),
      ])
      if (cancelled) return
      if (!page?.data) {
        setError(page?.error?.message ?? 'Could not load the audit log.')
        return
      }
      if (users?.data) {
        setActors(new Map(users.data.users.map((u) => [u.id, u.full_name])))
      }
      setEntries(page.data.entries)
      setCursor(page.data.next_cursor)
    })()
    return () => {
      cancelled = true
    }
  }, [])

  const loadMore = async () => {
    if (cursor === null) return
    setLoadingMore(true)
    const result = await api
      .GET('/api/v1/audit', {
        params: { query: { before: cursor, limit: PAGE_SIZE } },
      })
      .catch(() => null)
    setLoadingMore(false)
    if (!result?.data) {
      setError(result?.error?.message ?? 'Could not load more entries.')
      return
    }
    setEntries((prev) => [...(prev ?? []), ...result.data.entries])
    setCursor(result.data.next_cursor)
  }

  if (error) return <p className="form-error">{error}</p>
  if (!entries) {
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
          <p className="eyebrow">Admin console</p>
          <h1 className="page-title">Audit log</h1>
          <p className="admin-subtitle">
            Append-only · newest first · every actor, action, and resource
          </p>
        </div>
      </div>

      {entries.length === 0 ? (
        <section className="panel empty-state">
          <h2 className="card-title">Nothing recorded yet</h2>
          <p className="hint">
            Provisioning an account, publishing a quiz, or removing a student
            all write an entry here.
          </p>
        </section>
      ) : (
        <>
          <section className="panel table-panel admin-audit-panel">
            <div className="audit-table" role="table" aria-label="Audit log">
              <div className="audit-head" role="row">
                <span role="columnheader">Time</span>
                <span role="columnheader">Actor · action</span>
                <span role="columnheader">Resource</span>
              </div>
              {entries.map((entry) => {
                const verb = verbOf(entry.action)
                const actor = entry.actor_id
                  ? (actors.get(entry.actor_id) ?? shortId(entry.actor_id))
                  : 'System'
                return (
                  <div className="audit-row" role="row" key={entry.id}>
                    <span className="audit-time tabular">
                      {STAMP.format(new Date(entry.at))}
                    </span>
                    <span className="audit-actor">
                      {actor} ·{' '}
                      <span
                        className={
                          CONSEQUENTIAL.has(verb)
                            ? 'audit-verb audit-verb-consequential'
                            : 'audit-verb'
                        }
                      >
                        {verb}
                      </span>
                    </span>
                    <span className="audit-resource">
                      {entry.resource_type}
                      {entry.resource_id && (
                        <span className="audit-resource-id tabular">
                          {' '}
                          · {shortId(entry.resource_id)}
                        </span>
                      )}
                    </span>
                  </div>
                )
              })}
            </div>
          </section>

          {cursor !== null && (
            <button
              className="button button-quiet audit-more"
              type="button"
              disabled={loadingMore}
              onClick={() => void loadMore()}
            >
              {loadingMore ? 'Loading…' : 'Load older entries'}
            </button>
          )}
        </>
      )}
    </div>
  )
}
