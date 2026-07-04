import { useEffect, useState } from 'react'
import { api } from './api/client'
import type { components } from './api/schema'
import './App.css'

type Health = components['schemas']['Health']

type ApiState =
  | { phase: 'checking' }
  | { phase: 'up'; health: Health }
  | { phase: 'down' }

export default function App() {
  const [state, setState] = useState<ApiState>({ phase: 'checking' })

  useEffect(() => {
    let cancelled = false
    api
      .GET('/healthz')
      .then(({ data }) => {
        if (cancelled) return
        setState(data ? { phase: 'up', health: data } : { phase: 'down' })
      })
      .catch(() => {
        if (!cancelled) setState({ phase: 'down' })
      })
    return () => {
      cancelled = true
    }
  }, [])

  return (
    <main className="shell">
      <section className="card">
        <header className="masthead">
          <span className="brand-mark" aria-hidden="true">
            M
          </span>
          <div>
            <p className="eyebrow">MacQuiz</p>
            <h1 className="page-title">System status</h1>
          </div>
        </header>

        <div className="status-row">
          <span className="status-label">API</span>
          {state.phase === 'checking' && (
            <span className="chip chip-neutral">Checking</span>
          )}
          {state.phase === 'up' && <span className="chip chip-up">Up</span>}
          {state.phase === 'down' && (
            <span className="chip chip-down">Unreachable</span>
          )}
        </div>

        {state.phase === 'up' && (
          <dl className="meta">
            <div>
              <dt>Version</dt>
              <dd className="tabular">{state.health.version}</dd>
            </div>
            <div>
              <dt>Commit</dt>
              <dd className="tabular">{state.health.commit}</dd>
            </div>
            <div>
              <dt>Server time</dt>
              <dd className="tabular">{state.health.time}</dd>
            </div>
          </dl>
        )}
        {state.phase === 'down' && (
          <p className="hint">
            Start the API with <code>make run-server</code> or{' '}
            <code>docker compose up</code>, then reload.
          </p>
        )}
      </section>
    </main>
  )
}
