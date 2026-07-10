import '@fontsource-variable/fraunces'
import '@fontsource-variable/fraunces/wght-italic.css'
import '@fontsource/ibm-plex-mono/400.css'
import '@fontsource/ibm-plex-mono/500.css'
import '@fontsource/ibm-plex-mono/600.css'
import { useEffect, useState, type ReactNode } from 'react'
import { LoginCard } from './LoginScreen'

/**
 * The signed-out front door, art-directed as the thing it replaces: an
 * engineering-college question paper. Fraunces carries the headlines like a
 * typeset exam booklet, IBM Plex Mono carries every piece of exam metadata
 * (course codes, the ruled header strip, a live server clock), and the hero
 * artifact is the platform mid-exam - an OMR sheet under a dark proctor
 * console, stamped and verified. Sections are lettered like a paper
 * (Section A, B, C, Appendix). The real login card renders on first paint
 * (e2e suites type into #login-email immediately); nav and CTAs scroll to it.
 */

/* The subjects marquee: the technical syllabus the platform examines. */
const TICKER = [
  'Data Structures',
  'Computer Organization & Architecture',
  'Discrete Structures & Theory of Logic',
  'Operating Systems',
  'Theory of Automata & Formal Languages',
  'Object-Oriented Programming with Java',
  'Database Management Systems',
  'Computer Networks',
  'Design & Analysis of Algorithms',
  'Software Engineering',
  'Web Technology',
  'Engineering Mathematics III',
]

const LIFECYCLE: { q: string; title: string; body: string; marks: string }[] = [
  {
    q: 'Q1.',
    marks: '[2 marks]',
    title: 'Author',
    body: 'Set the paper in a three-step wizard or import a question bank from CSV/XLSX - marking scheme, negative marks and all.',
  },
  {
    q: 'Q2.',
    marks: '[2 marks]',
    title: 'Publish',
    body: 'The paper freezes into a sealed version: questions, marks, guardrails. Students sit exactly what you signed off.',
  },
  {
    q: 'Q3.',
    marks: '[4 marks]',
    title: 'Invigilate',
    body: 'A live roster of every attempt - progress, tab-switch evidence, kick and readmit - from the invigilator’s desk.',
  },
  {
    q: 'Q4.',
    marks: '[3 marks]',
    title: 'Grade',
    body: 'Instant, negative-marking-aware results the moment attempts settle. No answer-sheet bundles, no totalling errors.',
  },
  {
    q: 'Q5.',
    marks: '[4 marks]',
    title: 'Analyse',
    body: 'Item analysis, topic trends, and cohort dashboards - all exportable, ready for course files and accreditation.',
  },
]

const ROLES: { no: string; title: string; tagline: string; items: string[] }[] = [
  {
    no: '01',
    title: 'Admins',
    tagline: 'Full command over people, batches, and insights.',
    items: [
      'Provision faculty & students',
      'Bulk onboarding via CSV / XLSX',
      'Org analytics & audit log',
      'Batch dashboards & exports',
    ],
  },
  {
    no: '02',
    title: 'Faculty',
    tagline: 'Set papers and follow every attempt live.',
    items: [
      'Three-step authoring wizard',
      'MCQ, multi-select, true/false & short answer',
      'Negative marking, global or per question',
      'Live invigilation with guardrail evidence',
    ],
  },
  {
    no: '03',
    title: 'Students',
    tagline: 'Sit, submit, and grow with honest feedback.',
    items: [
      'Autosave on every answer',
      'Server-enforced deadlines',
      'Released scores with answer key',
      'Personal accuracy & topic trends',
    ],
  },
]

const BACKEND_TEAM = ['Ritik Kumar', 'Devang Pathak', 'Vivek Sharma', 'Vighnesh Shukla']
const FRONTEND_TEAM = ['Dakshita Tiwari', 'Anjali Tiwari', 'Rohit', 'Satyam Diwaker']

/** The one clock the whole platform obeys, ticking in the header for real. */
function useClock(): string {
  const [now, setNow] = useState(() => new Date())
  useEffect(() => {
    const id = setInterval(() => setNow(new Date()), 1000)
    return () => clearInterval(id)
  }, [])
  return now.toLocaleTimeString('en-IN', { hour12: false })
}

/** Poster of an OMR answer sheet mid-attempt - the paper half of the hero
 *  artifact. Static by design; one row carries a negative-marking penalty. */
function OmrSheet() {
  const rows: { q: number; mark: number; penalty?: boolean }[] = [
    { q: 11, mark: 2 },
    { q: 12, mark: 0 },
    { q: 13, mark: 3, penalty: true },
    { q: 14, mark: 1 },
    { q: 15, mark: -1 },
  ]
  return (
    <div className="landing-omr" aria-hidden="true">
      <div className="landing-omr-head">
        <span>MACQUIZ · OMR-15</span>
        <span>ROLL NO. 2201640100147</span>
      </div>
      {rows.map((r) => (
        <div key={r.q} className="landing-omr-row">
          <span className="landing-omr-q">{r.q}</span>
          {['A', 'B', 'C', 'D'].map((letter, i) => (
            <span
              key={letter}
              className={`landing-omr-bubble${i === r.mark ? ' landing-omr-bubble-filled' : ''}`}
            >
              {letter}
            </span>
          ))}
          {r.penalty && <span className="landing-omr-penalty">−0.25</span>}
        </div>
      ))}
      <div className="landing-omr-foot">DO NOT WRITE BELOW THIS LINE</div>
    </div>
  )
}

/** The console half of the hero artifact: live invigilation mid-sessional,
 *  built from the same visual vocabulary the real monitor uses. */
function ProctorConsole() {
  return (
    <div className="landing-console" aria-hidden="true">
      <div className="landing-console-head">
        <span className="landing-console-title">BCS-401 · Operating Systems — Sessional 2</span>
        <span className="landing-console-live">
          <span className="landing-live-dot" />
          LIVE 05:42
        </span>
      </div>
      {[
        { name: 'Ritik K.', pct: 86, note: 'Q9 / 12' },
        { name: 'Dakshita T.', pct: 63, note: 'Q8 / 12' },
        { name: 'Satyam D.', pct: 41, note: '2 violations', flagged: true },
        { name: 'Vighnesh S.', pct: 100, note: 'Submitted', done: true },
      ].map((r) => (
        <div key={r.name} className="landing-console-row">
          <span className="landing-console-name">{r.name}</span>
          <span className="landing-console-track">
            <span
              className={`landing-console-fill${r.done ? ' landing-console-fill-done' : ''}`}
              style={{ width: `${r.pct}%` }}
            />
          </span>
          <span
            className={`landing-console-note${r.flagged ? ' landing-console-note-flag' : ''}${r.done ? ' landing-console-note-done' : ''}`}
          >
            {r.note}
          </span>
        </div>
      ))}
      <div className="landing-console-chart">
        <span className="landing-console-chart-title">SCORE DISTRIBUTION</span>
        <div className="landing-console-bars">
          {[12, 22, 38, 62, 84, 100, 74, 46, 28, 16].map((h, i) => (
            <span
              key={i}
              className={`landing-console-bar${h === 100 ? ' landing-console-bar-peak' : ''}`}
              style={{ height: `${h}%` }}
            />
          ))}
        </div>
        <span className="landing-console-chart-foot">mean 7.2 · median 7.5 · 96% sat the paper</span>
      </div>
    </div>
  )
}

function SectionHead({
  section,
  title,
  sub,
}: {
  section: string
  title: ReactNode
  sub?: string
}) {
  return (
    <header className="landing-section-head">
      <span className="landing-section-tag">{section}</span>
      <h2 className="landing-section-title">{title}</h2>
      {sub && <p className="landing-section-sub">{sub}</p>}
    </header>
  )
}

export default function LandingScreen() {
  const clock = useClock()

  return (
    <div className="landing">
      <div className="landing-topline">
        <span className="landing-topline-org">RBMI · Software Development Cell</span>
        <span className="landing-topline-motto">One clock. No disputes.</span>
        <span className="landing-topline-clock">
          <span className="landing-live-dot" aria-hidden="true" />
          Server time&nbsp;
          <span className="landing-topline-time">{clock}</span>
          &nbsp;IST
        </span>
      </div>

      <header className="landing-nav">
        <a className="landing-brand" href="#top">
          <span className="brand-mark brand-mark-small" aria-hidden="true">
            M
          </span>
          <span className="landing-brand-text">
            <span className="landing-brand-name">MacQuiz</span>
            <span className="landing-brand-sub">Software Development Cell</span>
          </span>
        </a>
        <nav className="landing-nav-links" aria-label="Landing sections">
          <a href="#lifecycle">Paper pattern</a>
          <a href="#roles">Roles</a>
          <a href="#team">Team</a>
        </nav>
        <a className="landing-btn landing-btn-nav" href="#signin">
          Sign in
        </a>
      </header>

      <main id="top">
        <section className="landing-hero">
          <div className="landing-hero-copy">
            <p className="landing-paper-no">
              <span>Question paper № MQ-2026</span>
              <span className="landing-paper-no-rule" aria-hidden="true" />
              <span>Set &amp; invigilated by software</span>
            </p>
            <h1 className="landing-headline">
              The exam hall,
              <br />
              <em className="landing-headline-accent">rebuilt as software.</em>
            </h1>
            <p className="landing-sub">
              MacQuiz runs your college&rsquo;s technical assessments - Data
              Structures sessionals, Operating Systems unit tests, end-semester
              DBMS papers - with live invigilation, negative marking, and item
              analytics. Deadlines are enforced by the server&rsquo;s clock, so
              no student, browser, or power cut can bend them.
            </p>
            <dl className="landing-paper-meta">
              <div>
                <dt>Time</dt>
                <dd>Server-enforced</dd>
              </div>
              <div>
                <dt>Max. marks</dt>
                <dd>Negative-aware</dd>
              </div>
              <div>
                <dt>Answers</dt>
                <dd>Autosaved</dd>
              </div>
              <div>
                <dt>Invigilation</dt>
                <dd>Live</dd>
              </div>
            </dl>
            <div className="landing-cta-row">
              <a className="landing-btn landing-btn-primary" href="#signin">
                Enter the exam hall <span aria-hidden="true">→</span>
              </a>
              <a className="landing-btn landing-btn-ghost" href="#lifecycle">
                See the paper pattern
              </a>
            </div>
          </div>

          <div className="landing-artifact" aria-hidden="true">
            <OmrSheet />
            <ProctorConsole />
            <div className="landing-stamp">
              <span className="landing-stamp-top">RBMI · SDC</span>
              <span className="landing-stamp-mid">TIME
                <br />
                AUTHORITY</span>
              <span className="landing-stamp-bot">VERIFIED</span>
            </div>
          </div>
        </section>

        <div className="landing-ticker" aria-hidden="true">
          <div className="landing-ticker-track">
            {[...TICKER, ...TICKER].map((t, i) => (
              <span key={i} className="landing-ticker-item">
                {t}
                <span className="landing-ticker-sep">•</span>
              </span>
            ))}
          </div>
        </div>

        <section id="lifecycle" className="landing-section">
          <SectionHead
            section="Section A — Paper pattern"
            title={
              <>
                One paper, five moments,{' '}
                <em className="landing-title-accent">zero ambiguity.</em>
              </>
            }
          />
          <ol className="landing-lifecycle">
            {LIFECYCLE.map((s) => (
              <li key={s.q} className="landing-moment">
                <span className="landing-moment-q">{s.q}</span>
                <div className="landing-moment-body">
                  <h3 className="landing-moment-title">{s.title}</h3>
                  <p className="landing-moment-text">{s.body}</p>
                </div>
                <span className="landing-moment-marks" aria-hidden="true">
                  {s.marks}
                </span>
              </li>
            ))}
          </ol>
        </section>

        <section id="roles" className="landing-section">
          <SectionHead
            section="Section B — Who sits where"
            title={
              <>
                Three seats,{' '}
                <em className="landing-title-accent">three workspaces.</em>
              </>
            }
            sub="One platform, tailored to the office, the staff room, and the exam hall."
          />
          <div className="landing-roles">
            {ROLES.map((role) => (
              <article key={role.title} className="landing-role">
                <div className="landing-role-head">
                  <span className="landing-role-no">{role.no}</span>
                  <h3 className="landing-role-title">{role.title}</h3>
                </div>
                <p className="landing-role-tagline">{role.tagline}</p>
                <ul className="landing-check-list">
                  {role.items.map((item) => (
                    <li key={item} className="landing-check">
                      <span className="landing-check-dot" aria-hidden="true">
                        ✓
                      </span>
                      {item}
                    </li>
                  ))}
                </ul>
              </article>
            ))}
          </div>
        </section>

        <section id="team" className="landing-section">
          <SectionHead
            section="Appendix — Prepared by"
            title={
              <>
                Set by the{' '}
                <em className="landing-title-accent">Software Development Cell.</em>
              </>
            }
            sub="Version 2 is a ground-up rebuild by a single developer, on Go, React 19, and PostgreSQL - standing on the foundation the SDC's Version 1 team laid."
          />
          <div className="landing-v2-credit">
            <span className="landing-v2-tag">Version 2 · 2026 — Current release</span>
            <a
              className="landing-v2-name"
              href="https://github.com/SVIGHNESH"
              target="_blank"
              rel="noreferrer"
            >
              Vighnesh Shukla
              <span className="landing-v2-handle">github.com/SVIGHNESH ↗</span>
            </a>
            <p className="landing-v2-note">
              Designed &amp; built end to end - backend, frontend, and
              infrastructure.
            </p>
          </div>
          <div className="landing-teams">
            <article className="landing-register">
              <h3 className="landing-register-title">Version 1 · Backend bench</h3>
              <ol className="landing-register-rows">
                {BACKEND_TEAM.map((n, i) => (
                  <li key={n}>
                    <span className="landing-register-roll">{String(i + 1).padStart(2, '0')}</span>
                    {n}
                  </li>
                ))}
              </ol>
            </article>
            <img className="landing-team-logo" src="/sdc-logo.png" alt="" aria-hidden="true" />
            <article className="landing-register">
              <h3 className="landing-register-title">Version 1 · Frontend bench</h3>
              <ol className="landing-register-rows">
                {FRONTEND_TEAM.map((n, i) => (
                  <li key={n}>
                    <span className="landing-register-roll">{String(i + 1).padStart(2, '0')}</span>
                    {n}
                  </li>
                ))}
              </ol>
            </article>
          </div>
        </section>

        <section id="signin" className="landing-signin">
          <div className="landing-band">
            <div className="landing-band-copy">
              <p className="landing-band-kicker">
                No self-serve signups — accounts are provisioned by your college
              </p>
              <h2 className="landing-band-title">
                Ready when
                <br />
                <em>the bell rings.</em>
              </h2>
              <p className="landing-band-sub">
                Sign in with the credentials your administrator issued and
                start setting, sitting, and analysing papers in minutes.
              </p>
              <p className="landing-band-clock">
                <span className="landing-live-dot" aria-hidden="true" />
                SERVER TIME {clock} IST
              </p>
            </div>
            <LoginCard autoFocus={false} />
          </div>
        </section>
      </main>

      <footer className="landing-foot">
        <span className="landing-foot-note">
          MacQuiz v2 · Built by{' '}
          <a href="https://github.com/SVIGHNESH" target="_blank" rel="noreferrer">
            Vighnesh Shukla
          </a>{' '}
          · Software Development Cell · © 2026
        </span>
        <nav className="landing-foot-links" aria-label="Footer">
          <a href="#lifecycle">Paper pattern</a>
          <a href="#roles">Roles</a>
          <a href="#team">Team</a>
          <a href="#signin">Sign in</a>
        </nav>
        <img className="landing-foot-logo" src="/sdc-logo.png" alt="Software Development Cell" />
      </footer>
    </div>
  )
}
