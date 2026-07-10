/**
 * The SDC credits page: who built MacQuiz v2, on what stack, and how the
 * pieces fit. Purely informational - it calls no API and renders from
 * constants, so it can never break another view.
 */

interface Member {
  name: string
  /** GitHub username; the name renders as a profile link when set. */
  github?: string
}

const BACKEND_TEAM: Member[] = [{ name: 'Vighnesh Shukla', github: 'SVIGHNESH' }]
// const BACKEND_TEAM: Member[] = [
//   { name: 'Vighnesh Shukla', github: 'SVIGHNESH' },
//   { name: 'Ritik Kumar', github: '' },
//   { name: 'Devang Pathak', github: '' },
//   { name: 'Vivek Sharma', github: '' },
// ]
// const FRONTEND_TEAM: Member[] = [
//   { name: 'Dakshita Tiwari', github: '' },
//   { name: 'Satyam Diwaker', github: '' },
//   { name: 'Anjali Tiwari', github: '' },
//   { name: 'Rohit', github: '' },
// ]

const STACK: { area: string; tech: string; detail: string }[] = [
  {
    area: 'Backend',
    tech: 'Go 1.25',
    detail: 'Modular monolith · chi router · River job queue',
  },
  {
    area: 'Frontend',
    tech: 'React 19 + TypeScript',
    detail: 'Vite · typed openapi-fetch client · CSS design tokens',
  },
  {
    area: 'Database',
    tech: 'PostgreSQL',
    detail: 'Single source of truth - data, job queue, analytics rollups',
  },
  {
    area: 'Realtime',
    tech: 'WebSockets + Redis',
    detail: 'Best-effort pub/sub fan-out; Redis loss never stalls writes',
  },
  {
    area: 'API contract',
    tech: 'OpenAPI 3.0',
    detail: 'Contract-first: Go types and TS client generated, drift-checked in CI',
  },
  {
    area: 'Delivery',
    tech: 'Docker Compose + Caddy',
    detail: 'Same-origin SPA + API behind one proxy · Puppeteer e2e in CI',
  },
]

function TeamCard({ title, members }: { title: string; members: Member[] }) {
  return (
    <section className="panel sdc-team-card">
      <h2 className="card-title">{title}</h2>
      <ul className="sdc-team-list">
        {members.map((m) => (
          <li key={m.name} className="sdc-team-member">
            {m.github ? (
              <a
                className="sdc-team-link"
                href={`https://github.com/${m.github}`}
                target="_blank"
                rel="noreferrer"
              >
                {m.name}
                <span className="sdc-team-handle">github.com/{m.github} ↗</span>
              </a>
            ) : (
              m.name
            )}
          </li>
        ))}
      </ul>
    </section>
  )
}

function ArchNode({ title, sub, wide }: { title: string; sub: string; wide?: boolean }) {
  return (
    <div className={`sdc-arch-node${wide ? ' sdc-arch-node-wide' : ''}`}>
      <span className="sdc-arch-node-title">{title}</span>
      <span className="sdc-arch-node-sub">{sub}</span>
    </div>
  )
}

function ArchArrow({ label }: { label?: string }) {
  return (
    <div className="sdc-arch-arrow" aria-hidden="true">
      <span className="sdc-arch-arrow-line" />
      {label && <span className="sdc-arch-arrow-label">{label}</span>}
    </div>
  )
}

export default function SdcTeamPanel({ eyebrow }: { eyebrow: string }) {
  return (
    <div className="quiz-list">
      <div className="page-head">
        <div>
          <p className="eyebrow">{eyebrow}</p>
          <h1 className="page-title">SDC Team</h1>
        </div>
      </div>

      <section className="panel sdc-intro">
        <div className="sdc-intro-text">
          <h2 className="card-title">Software Development Cell (SDC)</h2>
          <p className="body-copy">
            MacQuiz v2 development team - student contributors.
          </p>
        </div>
        <img className="sdc-intro-logo" src="/sdc-logo.png" alt="" aria-hidden="true" />
      </section>

      <div className="sdc-team-grid">
        <TeamCard title="Developer" members={BACKEND_TEAM} />
      </div>

      <section className="panel">
        <h2 className="card-title">Tech stack - v2</h2>
        <div className="sdc-stack-grid">
          {STACK.map((s) => (
            <div key={s.area} className="sdc-stack-card">
              <span className="eyebrow">{s.area}</span>
              <span className="sdc-stack-tech">{s.tech}</span>
              <span className="sdc-stack-detail">{s.detail}</span>
            </div>
          ))}
        </div>
      </section>

      <section className="panel" aria-label="System architecture">
        <h2 className="card-title">Architecture</h2>
        <p className="hint">
          One Go binary in two roles behind a same-origin proxy; Postgres is
          the only source of truth, Redis is best-effort fan-out.
        </p>
        <div className="sdc-arch">
          <ArchNode title="React SPA" sub="Vite · typed client · httpOnly cookies, no CORS" wide />
          <ArchArrow label="same-origin  /api · /ws" />
          <ArchNode title="Caddy reverse proxy" sub="static SPA + API behind one origin" wide />
          <ArchArrow />
          <div className="sdc-arch-row">
            <ArchNode title="serve" sub="REST API + realtime WS gateway" />
            <ArchNode title="worker" sub="River jobs: open/close, deadlines, grading, rollups" />
          </div>
          <ArchArrow />
          <div className="sdc-arch-row">
            <ArchNode title="PostgreSQL" sub="data · job queue · frozen snapshots · analytics rollups" />
            <ArchNode title="Redis" sub="pub/sub fan-out · sessions · cache (degrades, never blocks)" />
          </div>
        </div>
      </section>

      <section className="panel" aria-label="Event flow">
        <h2 className="card-title">Live-event flow - persist first, publish second</h2>
        <div className="sdc-arch">
          <ArchNode title="Student action" sub="autosave / submit / guardrail report" wide />
          <ArchArrow label="idempotent submit funnel" />
          <ArchNode title="attempt_events (Postgres)" sub="append-only - the durable record" wide />
          <ArchArrow label="only after commit" />
          <ArchNode title="Redis pub/sub → WS gateway" sub="live monitor deltas; counters recomputed in SQL, never accumulated" wide />
        </div>
      </section>
    </div>
  )
}
