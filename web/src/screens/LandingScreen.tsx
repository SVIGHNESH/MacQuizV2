import '@fontsource-variable/fraunces'
import '@fontsource-variable/fraunces/wght-italic.css'
import '@fontsource/ibm-plex-mono/400.css'
import '@fontsource/ibm-plex-mono/500.css'
import '@fontsource/ibm-plex-mono/600.css'
import '../styles/landing.css'
import { useEffect, useRef, useState, type ReactNode } from 'react'
import { LoginCard } from './LoginScreen'

/**
 * The signed-out front door, art-directed as a monograph: a case-bound
 * volume about the exam hall, read cover to colophon. A running head tracks
 * the page number as you scroll, roman-numeral dividers open each part, the
 * five moments of a paper are laid out as chapter spreads, the three roles
 * are loose specimen cards on a desk, and the design tokens themselves are
 * shown as a film strip. Fraunces sets the display type, IBM Plex Mono every
 * piece of metadata, and every colour is a slate/primary token from
 * tokens.css (see docs/13-landing-design-system.md). The real login card
 * renders on first paint inside the colophon (e2e suites type into
 * #login-email immediately); nav and CTAs scroll to it.
 */

const reducedMotion = () =>
  typeof window !== 'undefined' &&
  window.matchMedia('(prefers-reduced-motion: reduce)').matches

const coarsePointer = () =>
  typeof window !== 'undefined' && window.matchMedia('(pointer: coarse)').matches

/** The one clock the whole platform obeys, ticking in the running head. */
function useClock(): string {
  const [now, setNow] = useState(() => new Date())
  useEffect(() => {
    const id = setInterval(() => setNow(new Date()), 1000)
    return () => clearInterval(id)
  }, [])
  return now.toLocaleTimeString('en-IN', { hour12: false })
}

/** Reveal-on-scroll for `.reveal` blocks and the chapter spreads. */
function useScrollReveal(root: React.RefObject<HTMLElement | null>) {
  useEffect(() => {
    const el = root.current
    if (!el) return
    const reveals = el.querySelectorAll<HTMLElement>('.reveal')
    const spreads = el.querySelectorAll<HTMLElement>('.spread')
    if (reducedMotion()) {
      reveals.forEach((n) => n.classList.add('is-in'))
      spreads.forEach((n) => n.classList.add('is-in'))
      return
    }
    const revealObs = new IntersectionObserver(
      (entries) => {
        entries.forEach((e) => {
          if (e.isIntersecting) {
            e.target.classList.add('is-in')
            revealObs.unobserve(e.target)
          }
        })
      },
      { threshold: 0.12, rootMargin: '0px 0px -8% 0px' },
    )
    const spreadObs = new IntersectionObserver(
      (entries) => {
        entries.forEach((e) => {
          const t = e.target
          if (e.isIntersecting && e.intersectionRatio > 0.12) {
            t.classList.add('is-in')
            t.classList.remove('is-past')
          } else if (!e.isIntersecting) {
            // Scrolled past: dim. Scrolled back above: reset to unrevealed.
            t.classList.toggle('is-past', e.boundingClientRect.top < 0)
            if (e.boundingClientRect.top >= 0) t.classList.remove('is-in')
          }
        })
      },
      { threshold: [0, 0.12, 0.4], rootMargin: '-5% 0px -5% 0px' },
    )
    reveals.forEach((n) => revealObs.observe(n))
    spreads.forEach((n) => spreadObs.observe(n))
    return () => {
      revealObs.disconnect()
      spreadObs.disconnect()
    }
  }, [root])
}

/** A trail of small dots in the primary colour, drawn on a fixed canvas. */
function CursorTrail() {
  const ref = useRef<HTMLCanvasElement>(null)
  useEffect(() => {
    const canvas = ref.current
    if (!canvas || coarsePointer() || reducedMotion()) return
    const ctx = canvas.getContext('2d')
    if (!ctx) return
    const dpr = Math.min(window.devicePixelRatio || 1, 2)
    const accent = getComputedStyle(canvas).getPropertyValue('color').trim()

    const resize = () => {
      canvas.width = window.innerWidth * dpr
      canvas.height = window.innerHeight * dpr
      canvas.style.width = `${window.innerWidth}px`
      canvas.style.height = `${window.innerHeight}px`
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0)
    }
    resize()
    window.addEventListener('resize', resize)

    type Dot = { x: number; y: number; r: number; life: number; decay: number }
    const dots: Dot[] = []
    let lastX = -100
    let lastY = -100
    const onMove = (e: MouseEvent) => {
      lastX = e.clientX
      lastY = e.clientY
      const count = 2 + Math.floor(Math.random() * 2)
      for (let i = 0; i < count; i++) {
        dots.push({
          x: e.clientX + (Math.random() - 0.5) * 6,
          y: e.clientY + (Math.random() - 0.5) * 6,
          r: 0.8 + Math.random() * 2.2,
          life: 1,
          decay: 0.012 + Math.random() * 0.022,
        })
      }
      if (dots.length > 500) dots.splice(0, dots.length - 500)
    }
    window.addEventListener('mousemove', onMove)

    let raf = 0
    const tick = () => {
      ctx.clearRect(0, 0, window.innerWidth, window.innerHeight)
      ctx.fillStyle = accent
      for (let i = dots.length - 1; i >= 0; i--) {
        const d = dots[i]
        d.life -= d.decay
        if (d.life <= 0) {
          dots.splice(i, 1)
          continue
        }
        ctx.globalAlpha = d.life * 0.55
        ctx.beginPath()
        ctx.arc(d.x, d.y, d.r * Math.max(0.2, d.life), 0, Math.PI * 2)
        ctx.fill()
      }
      ctx.globalAlpha = 0.9
      ctx.beginPath()
      ctx.arc(lastX, lastY, 2, 0, Math.PI * 2)
      ctx.fill()
      ctx.globalAlpha = 1
      raf = requestAnimationFrame(tick)
    }
    raf = requestAnimationFrame(tick)
    return () => {
      cancelAnimationFrame(raf)
      window.removeEventListener('resize', resize)
      window.removeEventListener('mousemove', onMove)
    }
  }, [])
  return <canvas ref={ref} className="cursor-canvas" aria-hidden="true" />
}

/* ------------------------------------------------------------------ data */

const STEPS: {
  no: string
  id: string
  title: string
  cat: string
  head: string
  sub: string
  meta: [string, string][]
  body: ReactNode[]
  quote: string
}[] = [
  {
    no: '01',
    id: 'author',
    title: 'Author',
    cat: 'Set the paper',
    head: 'Set a paper in three steps, or import one.',
    sub: 'Details, questions, and marking scheme in a wizard - or a question bank imported from CSV / XLSX with negative marks included.',
    meta: [
      ['Question types', 'MCQ · Multi-select · True/False · Short answer'],
      ['Marking', 'Global or per question, negative marks supported'],
      ['Import', 'CSV / XLSX question banks'],
      ['Drafts', 'Autosaved as you type'],
    ],
    body: [
      <>
        A teacher opens a <em>three-step wizard</em>: paper details, questions, marking
        scheme. Every edit is autosaved, so a draft started on one machine can be picked
        up on another.
      </>,
      <>
        Question banks that already live in a spreadsheet are imported directly. Rows are
        validated one by one, and any row that fails says exactly why.
      </>,
    ],
    quote: 'Four question types, per-question marks, and negative marking - set once by the person who teaches the course.',
  },
  {
    no: '02',
    id: 'publish',
    title: 'Publish',
    cat: 'Seal the version',
    head: 'What you sign off is exactly what students sit.',
    sub: 'Publishing freezes the paper into a sealed version - questions, marks, guardrails, schedule. Every attempt is bound to one version.',
    meta: [
      ['Version', 'Immutable once published'],
      ['Schedule', 'Opens and closes on the server clock'],
      ['Guardrails', 'Tab-switch and focus rules declared up front'],
      ['Audit log', 'Append-only record of every action'],
    ],
    body: [
      <>
        A published paper cannot drift. Its questions and marks are <em>frozen</em>, and
        its window opens and closes on the server's clock whether or not anyone is
        watching.
      </>,
      <>
        Guardrails - how many tab switches are tolerated, what happens after - are part
        of the sealed version, so students and teachers see the same rules.
      </>,
    ],
    quote: 'Server time is the only clock. The deadline is the single timing authority.',
  },
  {
    no: '03',
    id: 'invigilate',
    title: 'Invigilate',
    cat: 'Watch the hall live',
    head: 'Every attempt on one screen, in real time.',
    sub: 'A live roster of the hall: progress per student, tab-switch evidence, kick and readmit, extend or close - from one desk.',
    meta: [
      ['Roster', 'Live updates over WebSocket'],
      ['Evidence', 'Tab switches and focus loss, per attempt'],
      ['Actions', 'Kick · Readmit · Extend · Close'],
      ['Enforcement', 'Server-side only'],
    ],
    body: [
      <>
        Each row is a student, a progress bar, and a note. When a browser loses focus the
        attempt reports it and the row shows the <em>evidence</em>; the guardrail ladder
        set at publish time decides what happens next.
      </>,
      <>
        The browser only reports. Deadlines, auto-submits, and kicks are enforced on the
        server, so a power cut or a closed tab changes nothing about the deadline.
      </>,
    ],
    quote: 'Client signals are evidence. Only the server enforces.',
  },
  {
    no: '04',
    id: 'grade',
    title: 'Grade',
    cat: 'Settle the marks',
    head: 'Marks are ready the moment attempts end.',
    sub: 'Automatic, negative-marking-aware grading. No answer-sheet bundles, no totalling errors, no disputes about who submitted when.',
    meta: [
      ['Grading', 'Automatic on submit'],
      ['Negative marks', 'Applied per question'],
      ['Submission', 'Manual, timer, auto-submit, or kick - graded once'],
      ['Release', 'Scores held until the teacher releases them'],
    ],
    body: [
      <>
        Manual submit, timer expiry, a forced close, a kick: every way an attempt can end
        goes through <em>one path</em>, so each attempt is graded exactly once and the
        reason it ended is recorded.
      </>,
      <>
        Scores are held until the teacher releases them. Students then see their score
        with the answer key beside their sheet, penalties included.
      </>,
    ],
    quote: 'A −0.25 is written down next to the bubble, where the student can read it.',
  },
  {
    no: '05',
    id: 'analyse',
    title: 'Analyse',
    cat: 'After the hall empties',
    head: 'See what the paper taught you about the batch.',
    sub: 'Item analysis, topic trends, and cohort dashboards - computed in the background and exportable as CSV for course files and accreditation.',
    meta: [
      ['Item analysis', 'Difficulty and discrimination per question'],
      ['Trends', 'Per topic and per student'],
      ['Dashboards', 'Batch and organisation'],
      ['Export', 'CSV'],
    ],
    body: [
      <>
        Which question did the whole hall miss? Which topic does a batch keep tripping
        on? The rollups answer in <em>seconds</em>, from the same attempt data the hall
        produced.
      </>,
      <>
        Everything exports. Course files, accreditation binders, and end-of-semester
        reviews are one download each.
      </>,
    ],
    quote: 'Score distributions, per-question difficulty, and topic trends - no spreadsheets built by hand.',
  },
]

const ROLES: {
  no: string
  kind: string
  title: string
  glyph: [string, string]
  items: string[]
  guide?: { href: string; label: string }
  pos: { left: string; top: string; rot: string }
}[] = [
  {
    no: '01',
    kind: 'OFFICE',
    title: 'Admins',
    glyph: ['A', 'd'],
    items: [
      'Provision faculty & students',
      'Bulk onboarding via CSV / XLSX',
      'Org analytics & audit log',
      'Batch dashboards & exports',
    ],
    pos: { left: '6%', top: '10%', rot: '-2.4deg' },
  },
  {
    no: '02',
    kind: 'STAFF ROOM',
    title: 'Faculty',
    glyph: ['F', 'a'],
    items: [
      'Three-step authoring wizard',
      'MCQ, multi-select, true/false & short answer',
      'Negative marking, global or per question',
      'Live invigilation with guardrail evidence',
    ],
    guide: { href: '/guides/teacher.html', label: 'WALKTHROUGH' },
    pos: { left: '36%', top: '22%', rot: '1.6deg' },
  },
  {
    no: '03',
    kind: 'EXAM HALL',
    title: 'Students',
    glyph: ['S', 't'],
    items: [
      'Autosave on every answer',
      'Server-enforced deadlines',
      'Released scores with answer key',
      'Personal accuracy & topic trends',
    ],
    guide: { href: '/guides/student.html', label: 'WALKTHROUGH' },
    pos: { left: '66%', top: '8%', rot: '-1.2deg' },
  },
]

const SECTIONS: { id: string; title: string; cat: string }[] = [
  ...['author', 'publish', 'invigilate', 'grade', 'analyse'].map((id, i) => ({
    id,
    title: ['Author', 'Publish', 'Invigilate', 'Grade', 'Analyse'][i],
    cat: ['Set the paper', 'Seal the version', 'Watch the hall live', 'Settle the marks', 'After the hall empties'][i],
  })),
  { id: 'roles', title: 'Roles', cat: 'Admins · Faculty · Students' },
  { id: 'guides', title: 'Walkthroughs', cat: 'Teacher and student guides' },
  { id: 'signin', title: 'Sign in', cat: 'Team and credentials' },
]

const BACKEND_TEAM = ['Ritik Kumar', 'Devang Pathak', 'Vivek Sharma', 'Vighnesh Shukla']
const FRONTEND_TEAM = ['Dakshita Tiwari', 'Anjali Tiwari', 'Rohit', 'Satyam Diwaker']

/* ------------------------------------------------------------- visuals */

/** Plate 01: the authoring wizard, mid-step. */
function WizardPlate() {
  return (
    <div className="plate plate-wizard" aria-hidden="true">
      <span className="plate-corner plate-corner-tl">FIG. 01</span>
      <span className="plate-corner plate-corner-tr">AUTHORING WIZARD</span>
      <ol className="wizard-steps">
        <li className="is-done">
          <span>1</span>Details
        </li>
        <li className="is-active">
          <span>2</span>Questions
        </li>
        <li>
          <span>3</span>Marking
        </li>
      </ol>
      <div className="wizard-q">
        <div className="wizard-q-head">
          <span>Q7 · MCQ</span>
          <span>+4 / −1</span>
        </div>
        <p className="wizard-q-text">
          Which scheduling policy can starve a long CPU-bound process indefinitely?
        </p>
        {['FCFS', 'Round robin', 'Shortest job first', 'Multilevel feedback'].map((o, i) => (
          <div key={o} className={`wizard-opt${i === 2 ? ' is-correct' : ''}`}>
            <span className="wizard-opt-key">{String.fromCharCode(65 + i)}</span>
            {o}
          </div>
        ))}
      </div>
      <span className="plate-corner plate-corner-bl">DRAFT · AUTOSAVED 14:02:11</span>
      <span className="plate-corner plate-corner-br">12 / 20 Q</span>
    </div>
  )
}

/** Plate 02: the sealed version. */
function SealPlate() {
  return (
    <div className="plate plate-seal" aria-hidden="true">
      <span className="plate-corner plate-corner-tl">FIG. 02</span>
      <span className="plate-corner plate-corner-tr">VERSION RECORD</span>
      <div className="seal-stamp">
        <span className="seal-stamp-top">RBMI · SDC</span>
        <span className="seal-stamp-mid">
          TIME
          <br />
          AUTHORITY
        </span>
        <span className="seal-stamp-bot">VERIFIED</span>
      </div>
      <dl className="seal-meta">
        <div>
          <dt>Paper</dt>
          <dd>BCS-401 · Operating Systems · Sessional 2</dd>
        </div>
        <div>
          <dt>Version</dt>
          <dd>v3 · sealed</dd>
        </div>
        <div>
          <dt>Window</dt>
          <dd>09:30 → 10:30 IST</dd>
        </div>
        <div>
          <dt>Guardrails</dt>
          <dd>Tab switch ×3 → auto-submit</dd>
        </div>
      </dl>
      <span className="plate-corner plate-corner-bl">PUBLISHED BY DR. A. VERMA</span>
      <span className="plate-corner plate-corner-br">AUDIT #4812</span>
    </div>
  )
}

/** Plate 03: live invigilation, built from the real monitor's vocabulary. */
function ConsolePlate() {
  return (
    <div className="plate plate-console" aria-hidden="true">
      <div className="console-head">
        <span className="plate-corner-inline">FIG. 03 · LIVE ROSTER</span>
        <span className="console-live">
          <span className="live-dot" />
          LIVE 05:42
        </span>
      </div>
      {[
        { name: 'Ritik K.', pct: 86, note: 'Q9 / 12' },
        { name: 'Dakshita T.', pct: 63, note: 'Q8 / 12' },
        { name: 'Satyam D.', pct: 41, note: '2 violations', flagged: true },
        { name: 'Vighnesh S.', pct: 100, note: 'Submitted', done: true },
      ].map((r) => (
        <div key={r.name} className="console-row">
          <span className="console-name">{r.name}</span>
          <span className="console-track">
            <span
              className={`console-fill${r.done ? ' is-done' : ''}`}
              style={{ width: `${r.pct}%` }}
            />
          </span>
          <span
            className={`console-note${r.flagged ? ' is-flag' : ''}${r.done ? ' is-done' : ''}`}
          >
            {r.note}
          </span>
        </div>
      ))}
      <div className="console-foot">
        <span>38 SEATED · 1 SUBMITTED</span>
        <span>KICK · READMIT · EXTEND</span>
      </div>
    </div>
  )
}

/** Plate 04: an OMR sheet, one row carrying a penalty. */
function OmrPlate() {
  const rows: { q: number; mark: number; penalty?: boolean }[] = [
    { q: 11, mark: 2 },
    { q: 12, mark: 0 },
    { q: 13, mark: 3, penalty: true },
    { q: 14, mark: 1 },
    { q: 15, mark: -1 },
  ]
  return (
    <div className="plate plate-omr" aria-hidden="true">
      <div className="omr-head">
        <span>MACQUIZ · OMR-15</span>
        <span>ROLL NO. 2201640100147</span>
      </div>
      {rows.map((r) => (
        <div key={r.q} className="omr-row">
          <span className="omr-q">{r.q}</span>
          {['A', 'B', 'C', 'D'].map((letter, i) => (
            <span key={letter} className={`omr-bubble${i === r.mark ? ' is-filled' : ''}`}>
              {letter}
            </span>
          ))}
          {r.penalty && <span className="omr-penalty">−0.25</span>}
        </div>
      ))}
      <div className="omr-foot">DO NOT WRITE BELOW THIS LINE</div>
    </div>
  )
}

/** Plate 05: the score distribution. */
function ChartPlate() {
  return (
    <div className="plate plate-chart" aria-hidden="true">
      <span className="plate-corner plate-corner-tl">FIG. 05</span>
      <span className="plate-corner plate-corner-tr">SCORE DISTRIBUTION</span>
      <div className="chart-bars">
        {[12, 22, 38, 62, 84, 100, 74, 46, 28, 16].map((h, i) => (
          <span key={i} className={`chart-bar${h === 100 ? ' is-peak' : ''}`} style={{ height: `${h}%` }} />
        ))}
      </div>
      <div className="chart-axis">
        <span>0</span>
        <span>5</span>
        <span>10</span>
        <span>15</span>
        <span>20</span>
      </div>
      <div className="chart-foot">
        <span>MEAN 7.2 · MEDIAN 7.5</span>
        <span>96% SAT THE PAPER</span>
      </div>
    </div>
  )
}

const PLATES = [WizardPlate, SealPlate, ConsolePlate, OmrPlate, ChartPlate]

/* -------------------------------------------------------- sections */

function Divider({ roman, label }: { roman: string; label: string }) {
  return (
    <div className="divider" aria-hidden="true">
      <span className="divider-roman">{roman}</span>
      <span>- {label} -</span>
    </div>
  )
}

/** The three roles as loose specimen cards on a desk: drag to rearrange. */
function SpecimenDesk() {
  const ref = useRef<HTMLDivElement>(null)
  useEffect(() => {
    const desk = ref.current
    if (!desk) return
    let z = 5
    const cleanups: (() => void)[] = []
    desk.querySelectorAll<HTMLElement>('.specimen').forEach((card) => {
      let x = 0
      let y = 0
      let startX = 0
      let startY = 0
      let active: number | null = null
      const rot = card.style.getPropertyValue('--rot') || '0deg'
      const apply = () => {
        card.style.transform = `translate(${x}px, ${y}px) rotate(${rot})`
      }
      const down = (e: PointerEvent) => {
        if ((e.target as HTMLElement).closest('a')) return
        if (e.pointerType === 'mouse' && e.button !== 0) return
        active = e.pointerId
        startX = e.clientX
        startY = e.clientY
        card.classList.add('is-dragging')
        card.style.zIndex = String(++z)
        card.setPointerCapture(e.pointerId)
      }
      const move = (e: PointerEvent) => {
        if (active !== e.pointerId) return
        x += e.clientX - startX
        y += e.clientY - startY
        startX = e.clientX
        startY = e.clientY
        apply()
      }
      const up = (e: PointerEvent) => {
        if (active !== e.pointerId) return
        active = null
        card.classList.remove('is-dragging')
        try {
          card.releasePointerCapture(e.pointerId)
        } catch {
          /* already released */
        }
      }
      card.addEventListener('pointerdown', down)
      card.addEventListener('pointermove', move)
      card.addEventListener('pointerup', up)
      card.addEventListener('pointercancel', up)
      cleanups.push(() => {
        card.removeEventListener('pointerdown', down)
        card.removeEventListener('pointermove', move)
        card.removeEventListener('pointerup', up)
        card.removeEventListener('pointercancel', up)
      })
    })
    return () => cleanups.forEach((fn) => fn())
  }, [])

  return (
    <div className="specimens-desk" ref={ref}>
      <span className="specimens-hint" aria-hidden="true">
        <span>DRAG</span> TO REARRANGE
      </span>
      {ROLES.map((role) => (
        <article
          key={role.no}
          className="specimen"
          style={
            {
              left: role.pos.left,
              top: role.pos.top,
              '--rot': role.pos.rot,
              transform: `rotate(${role.pos.rot})`,
            } as React.CSSProperties
          }
        >
          <div className="specimen-head">
            <span>
              NO. {role.no} / {role.kind}
            </span>
            <span>{role.title.toUpperCase()}</span>
          </div>
          <div className="specimen-glyph">
            {role.glyph[0]}
            <em>{role.glyph[1]}</em>
          </div>
          <ul className="specimen-list">
            {role.items.map((it) => (
              <li key={it}>{it}</li>
            ))}
          </ul>
          <div className="specimen-foot">
            <span>MACQUIZ · 2026</span>
            {role.guide ? (
              <a className="specimen-link" href={role.guide.href}>
                {role.guide.label}
              </a>
            ) : (
              <span>PROVISIONED</span>
            )}
          </div>
        </article>
      ))}
    </div>
  )
}

/* ------------------------------------------------------------- screen */

export default function LandingScreen() {
  const clock = useClock()
  const root = useRef<HTMLDivElement>(null)
  useScrollReveal(root)

  return (
    <div className="landing" ref={root}>
      <CursorTrail />

      <header className="running-head" aria-hidden="true">
        <span>MACQUIZ</span>
        <span>RBMI · SOFTWARE DEVELOPMENT CELL</span>
        <span>
          <span className="live-dot" />
          <span className="running-head-page">{clock}</span> IST
        </span>
      </header>

      <nav className="landing-nav" aria-label="Landing sections">
        <a href="#author">How it works</a>
        <a href="#roles">Roles</a>
        <a href="#guides">Guides</a>
        <a href="#team">Team</a>
        <a className="landing-nav-cta" href="#signin">
          Sign in
        </a>
      </nav>

      <main>
        {/* ---------------------------------------------------- cover */}
        <section className="hero" id="top">
          <div className="hero-meta">
            <span>QUIZ &amp; EXAM PLATFORM</span>
            <span>SESSIONALS · UNIT TESTS · END-SEMESTER PAPERS</span>
            <span>VERSION 2 · 2026</span>
          </div>

          <div className="hero-title">
            <h1 className="hero-name">
              <span className="hero-line hero-line-1" aria-label="Mac">
                {['M', 'a', 'c'].map((c, i) => (
                  <span key={i} style={{ '--i': i } as React.CSSProperties}>
                    {c}
                  </span>
                ))}
              </span>
              <span className="hero-line hero-line-2" aria-label="Quiz">
                {['Q', 'u', 'i', 'z'].map((c, i) => (
                  <span key={i} style={{ '--i': i + 3 } as React.CSSProperties}>
                    {c}
                  </span>
                ))}
              </span>
            </h1>
            <div className="hero-byline">
              <span className="hero-rule" aria-hidden="true" />
              <span>Author · Publish · Invigilate · Grade · Analyse</span>
              <span className="hero-rule" aria-hidden="true" />
            </div>
            <p className="hero-sub">
              The exam hall, rebuilt as software. MacQuiz runs your college&rsquo;s
              technical assessments - sessionals, unit tests, end-semester papers - with
              live invigilation, negative marking, and item analytics. Deadlines are kept
              by the server&rsquo;s clock, so no browser or power cut can bend them.
            </p>
            <div className="hero-cta">
              <a className="btn btn-primary" href="#signin">
                Enter the exam hall <span aria-hidden="true">→</span>
              </a>
              <a className="btn btn-ghost" href="#author">
                See how it works
              </a>
            </div>
          </div>

          <div className="hero-meta">
            <span className="hero-clock">
              <span className="live-dot" aria-hidden="true" />
              SERVER TIME <span className="hero-clock-time">{clock}</span> IST
            </span>
            <span className="hero-scroll-cue">↓ SCROLL</span>
            <span>ACCOUNTS ISSUED BY YOUR COLLEGE</span>
          </div>
        </section>

        {/* -------------------------------------------------- overview */}
        <Divider roman="I" label="Overview" />
        <section className="manifesto reveal" id="overview">
          <div className="manifesto-header">
            <span className="eyebrow">WHAT MACQUIZ IS</span>
            <h2 className="manifesto-title">
              One clock. <em>No disputes.</em>
            </h2>
          </div>
          <div className="manifesto-body">
            <p>
              Every exam hall runs on three arguments: what time it is, what the paper
              said, and who did what. MacQuiz settles all three. The server keeps the only
              clock, a published paper is sealed and cannot change, and every action - a
              tab switch, a submission, a forced close - is recorded once and never edited.
            </p>
            <p>
              Teachers set papers in a wizard or import them from a spreadsheet, watch
              every attempt live, and release results when they are ready. Students get
              autosaved answers, a timer they can trust, and their score with the answer
              key. Admins provision accounts in bulk and get batch and organisation
              dashboards with CSV export.
            </p>
            <p>
              Version 2 is a ground-up rebuild on Go, React, and PostgreSQL, built by the
              Software Development Cell at RBMI for its own halls.
            </p>
          </div>
        </section>

        {/* ------------------------------------------------- contents */}
        <Divider roman="II" label="On this page" />
        <section className="index reveal" id="contents">
          <div className="index-header">
            <span className="eyebrow">FIVE STEPS, ONE PAPER</span>
            <h2 className="index-title">Contents</h2>
          </div>
          <ol className="index-list">
            {SECTIONS.map((c, i) => (
              <li key={c.id}>
                <a className="index-entry" href={`#${c.id}`}>
                  <span className="index-num">{String(i + 1).padStart(2, '0')} --</span>
                  <span className="index-name">{c.title}</span>
                  <span className="index-cat">{c.cat}</span>
                  <span className="index-page" aria-hidden="true">
                    →
                  </span>
                </a>
              </li>
            ))}
          </ol>
        </section>

        {/* ---------------------------------------------------- steps */}
        <Divider roman="III" label="How it works" />
        {STEPS.map((c, i) => {
          const Plate = PLATES[i]
          return (
            <section key={c.no} className={`spread spread-${c.id}`} id={c.id}>
              <div className="spread-inner">
                <div className="spread-head">
                  <span>HOW IT WORKS</span>
                  <span>
                    STEP {c.no} - {c.title.toUpperCase()}
                  </span>
                  <span>{c.no} / 05</span>
                </div>
                <div className="spread-chapter-wrap">
                  <div className="spread-chapter">{c.no}</div>
                  <div className="spread-chapter-label">
                    {c.title}
                    <small>{c.cat}</small>
                  </div>
                </div>
                <h3 className="spread-title">{c.head}</h3>
                <p className="spread-sub">{c.sub}</p>
                <dl className="spread-meta">
                  {c.meta.map(([k, v]) => (
                    <div key={k}>
                      <dt>{k}</dt>
                      <dd>{v}</dd>
                    </div>
                  ))}
                </dl>
                <div className={`spread-layout${i % 2 === 1 ? ' is-flipped' : ''}`}>
                  <div className="spread-body">
                    {c.body.map((b, j) => (
                      <p key={j}>{b}</p>
                    ))}
                    <blockquote className="spread-pullquote">{c.quote}</blockquote>
                  </div>
                  <div className="spread-visual">
                    <Plate />
                    <div className="spread-caption">
                      <span>
                        Fig. {c.no} - {c.cat}
                      </span>
                      <span>Illustrative data</span>
                    </div>
                  </div>
                </div>
              </div>
            </section>
          )
        })}

        {/* ------------------------------------------------------ roles */}
        <Divider roman="IV" label="Roles" />
        <div className="section-head reveal" id="roles">
          <h2 className="section-head-title">
            Three seats, <em>three workspaces.</em>
          </h2>
          <p className="section-head-sub">
            The office, the staff room, and the exam hall each get their own workspace.
            Drag the cards around; the walkthroughs open without signing in.
          </p>
        </div>
        <section className="specimens">
          <SpecimenDesk />
        </section>

        {/* --------------------------------------------------- guides */}
        <Divider roman="V" label="Walkthroughs" />
        <div className="section-head reveal" id="guides">
          <h2 className="section-head-title">
            Read the paper <em>before you sit it.</em>
          </h2>
          <p className="section-head-sub">
            Two short walkthroughs, one per side of the desk. No sign-in needed, and both
            print cleanly.
          </p>
        </div>
        <section className="guides reveal">
          <a className="guide" href="/guides/teacher.html">
            <span className="guide-no">MQ-T1</span>
            <h3 className="guide-title">
              The <em>invigilator&rsquo;s</em> walkthrough.
            </h3>
            <p className="guide-sub">
              Setting the paper, choosing the hall, invigilating live, and settling the
              marks afterwards.
            </p>
            <span className="guide-cta">FOR TEACHERS →</span>
          </a>
          <a className="guide" href="/guides/student.html">
            <span className="guide-no">MQ-S1</span>
            <h3 className="guide-title">
              The <em>candidate&rsquo;s</em> walkthrough.
            </h3>
            <p className="guide-sub">
              Signing in, what the timer really means, what happens when the WiFi drops, and
              where your result appears.
            </p>
            <span className="guide-cta">FOR STUDENTS →</span>
          </a>
        </section>

        {/* -------------------------------------------------- sign in */}
        <Divider roman="VI" label="Sign in" />
        <section className="colophon" id="signin">
          <div className="colophon-grid">
            <div className="colophon-copy reveal" id="team">
              <p className="colophon-pre">- No self-serve signups - accounts are issued by your college -</p>
              <h2 className="colophon-name">
                Ready when <em>the bell rings.</em>
              </h2>
              <p className="colophon-sub">
                Sign in with the credentials your administrator issued and start setting,
                sitting, and analysing papers.
              </p>
              <p className="colophon-clock">
                <span className="live-dot" aria-hidden="true" />
                SERVER TIME {clock} IST
              </p>

              <div className="credits">
                <div className="credit credit-v2">
                  <span className="label">Version 2 · 2026 · Current release</span>
                  <a
                    className="credit-name"
                    href="https://github.com/SVIGHNESH"
                    target="_blank"
                    rel="noreferrer"
                  >
                    Vighnesh Shukla
                  </a>
                  <span className="value">
                    Designed &amp; built end to end - backend, frontend, infrastructure.{' '}
                    <a
                      className="credit-handle"
                      href="https://github.com/SVIGHNESH"
                      target="_blank"
                      rel="noreferrer"
                    >
                      github.com/SVIGHNESH ↗
                    </a>
                  </span>
                </div>
                <div className="credit">
                  <span className="label">Version 1 · Backend bench</span>
                  <ol className="credit-list">
                    {BACKEND_TEAM.map((n, i) => (
                      <li key={n}>
                        <span>{String(i + 1).padStart(2, '0')}</span>
                        {n}
                      </li>
                    ))}
                  </ol>
                </div>
                <div className="credit">
                  <span className="label">Version 1 · Frontend bench</span>
                  <ol className="credit-list">
                    {FRONTEND_TEAM.map((n, i) => (
                      <li key={n}>
                        <span>{String(i + 1).padStart(2, '0')}</span>
                        {n}
                      </li>
                    ))}
                  </ol>
                </div>
              </div>
            </div>
            <div className="colophon-login">
              <LoginCard autoFocus={false} />
            </div>
          </div>

          <div className="colophon-note">
            <div className="colophon-sig">
              <nav className="foot-links" aria-label="Footer">
                <a href="#author">How it works</a>
                <a href="#roles">Roles</a>
                <a href="/guides/teacher.html">Teacher guide</a>
                <a href="/guides/student.html">Student guide</a>
                <a href="#signin">Sign in</a>
              </nav>
              <span>MACQUIZ V2 · SOFTWARE DEVELOPMENT CELL · © 2026</span>
              <img src="/sdc-logo.png" alt="Software Development Cell" className="colophon-logo" />
            </div>
          </div>
        </section>
      </main>
    </div>
  )
}
