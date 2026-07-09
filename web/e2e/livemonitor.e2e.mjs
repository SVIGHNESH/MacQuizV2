// Browser end-to-end check of the Milestone 5 teacher live dashboard
// (LiveMonitorPanel) against a running stack: `docker compose up` (API on
// :8080, WebSocket gateway on the same port) plus `npm run dev` (SPA on
// :5173, proxying /ws too).
//
// Drives a real Chromium through the docs/05/docs/12 Milestone 5 exit
// criterion: a teacher watches a student progress live, then kicks them.
//   1. API-side setup: a teacher publishes a 1-question quiz to 1 student,
//      live now for a long enough window to drive the flow.
//   2. The teacher opens the quiz editor; it renders as Live with a roster
//      row for the student reading "Not started" and a "Live" socket badge.
//   3. The student (separate browser context) starts the attempt and
//      answers the question; the teacher's roster updates to "In progress"
//      and "1 / 1" over the WebSocket, with no page reload.
//   4. The teacher kicks the student through the two-step destructive-flow
//      modal (docs/11 section 4); the roster row flips to "Kicked" and a
//      Readmit control appears, and the student's next autosave is refused.
//
// Run:  node e2e/livemonitor.e2e.mjs
// Env:  E2E_BASE_URL (default http://localhost:5173)
//       E2E_CHROMIUM (default /usr/bin/chromium)
//       E2E_ADMIN_EMAIL / E2E_ADMIN_PASSWORD (default compose bootstrap creds)
// Screenshots land in /tmp/macquiz-e2e/.

import { mkdir } from 'node:fs/promises'
import puppeteer from 'puppeteer-core'

const BASE = process.env.E2E_BASE_URL ?? 'http://localhost:5173'
const CHROMIUM = process.env.E2E_CHROMIUM ?? '/usr/bin/chromium'
const ADMIN_EMAIL = process.env.E2E_ADMIN_EMAIL ?? 'admin@macquiz.local'
const ADMIN_PASSWORD = process.env.E2E_ADMIN_PASSWORD ?? 'admin-dev-password'
const SHOT_DIR = '/tmp/macquiz-e2e'

const run = process.pid
const teacherEmail = `livemon.e2e.${run}@macquiz.local`
const teacherPassword = 'live-monitor-teacher'
const studentEmail = `livemon.student.${run}@macquiz.local`
const studentPassword = 'live-monitor-student'
const QUIZ_TITLE = 'Live roster check'

// Live 3 s from publish, open for 5 minutes - long enough for the scripted
// student and the kick to run well within the window.
const LIVE_DELAY_MS = 3_000
const WINDOW_MS = 5 * 60_000
const DURATION_SEC = 240

let failures = 0
function check(ok, label) {
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${label}`)
  if (!ok) failures++
}

async function shot(page, name) {
  await new Promise((resolve) => setTimeout(resolve, 500))
  await page.screenshot({ path: `${SHOT_DIR}/${name}`, fullPage: true })
}

async function waitForText(page, selector, needle, timeout = 5000) {
  return page
    .waitForFunction(
      (sel, want) =>
        [...document.querySelectorAll(sel)].some((el) =>
          (el.textContent ?? '').includes(want),
        ),
      { timeout },
      selector,
      needle,
    )
    .then(() => true)
    .catch(() => false)
}

async function type(page, selector, value) {
  await page.waitForSelector(selector, { timeout: 5000 })
  await page.click(selector)
  await page.keyboard.down('Control')
  await page.keyboard.press('KeyA')
  await page.keyboard.up('Control')
  await page.keyboard.press('Backspace')
  await page.type(selector, value)
}

async function clickButtonWithText(page, text, scope = '') {
  const clicked = await page.evaluate(
    (want, sel) => {
      const root = sel ? document.querySelector(sel) : document
      if (!root) return false
      const button = [...root.querySelectorAll('button')].find((el) =>
        (el.textContent ?? '').trim().includes(want),
      )
      if (!button) return false
      button.click()
      return true
    },
    text,
    scope,
  )
  if (!clicked) throw new Error(`no button with text "${text}" in "${scope}"`)
}

async function pickOption(page, position, optionText) {
  const clicked = await page.evaluate(
    (pos, want) => {
      const panel = document.querySelectorAll('.player-question')[pos - 1]
      if (!panel) return false
      const row = [...panel.querySelectorAll('.option-row')].find(
        (el) =>
          (el.querySelector('.option-static')?.textContent ?? '').trim() ===
          want,
      )
      if (!row) return false
      row.querySelector('input').click()
      return true
    },
    position,
    optionText,
  )
  if (!clicked) throw new Error(`no option "${optionText}" in question ${position}`)
}

// --- API helpers -------------------------------------------------------------

function cookiesOf(response) {
  return response.headers
    .getSetCookie()
    .map((c) => c.split(';')[0])
    .join('; ')
}

// Retries on 429: the e2e suites share one hardcoded admin account
// (docs/08 section 4's 5/account/minute login limit), so running several
// suites back to back easily exhausts it. The server's Retry-After header
// says exactly how long the sliding window needs to drain.
async function login(email, password) {
  for (let attempt = 0; ; attempt++) {
    const res = await fetch(`${BASE}/api/v1/auth/login`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email, password }),
    })
    if (res.status === 429 && attempt < 5) {
      const retryAfter = Number(res.headers.get('Retry-After')) || 5
      await new Promise((resolve) => setTimeout(resolve, retryAfter * 1000))
      continue
    }
    if (!res.ok) throw new Error(`login ${email} failed: ${res.status}`)
    return { cookies: cookiesOf(res), body: await res.json() }
  }
}

async function completeReset(email, oneTime, newPassword) {
  const first = await login(email, oneTime)
  const changed = await fetch(`${BASE}/api/v1/auth/password`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Cookie: first.cookies },
    body: JSON.stringify({
      current_password: oneTime,
      new_password: newPassword,
    }),
  })
  if (changed.status !== 204) {
    throw new Error(`password change for ${email} failed: ${changed.status}`)
  }
}

async function request(cookies, method, path, body, wantStatus) {
  const res = await fetch(`${BASE}${path}`, {
    method,
    headers: { 'Content-Type': 'application/json', Cookie: cookies },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (res.status !== wantStatus) {
    throw new Error(`${method} ${path} failed: ${res.status}`)
  }
  return res.status === 204 ? null : res.json()
}

// A ready teacher and one student, and the teacher's published 1-question
// quiz assigned to the student, live right now.
async function provision() {
  const admin = await login(ADMIN_EMAIL, ADMIN_PASSWORD)
  const teacher = await request(admin.cookies, 'POST', '/api/v1/users', {
    role: 'teacher',
    email: teacherEmail,
    full_name: 'Monica Monitor',
  }, 201)
  const student = await request(admin.cookies, 'POST', '/api/v1/users', {
    role: 'student',
    email: studentEmail,
    full_name: 'Sam Student',
  }, 201)
  await fetch(`${BASE}/api/v1/auth/logout`, {
    method: 'POST',
    headers: { Cookie: admin.cookies },
  })
  await completeReset(teacherEmail, teacher.initial_password, teacherPassword)
  await completeReset(studentEmail, student.initial_password, studentPassword)

  const t = await login(teacherEmail, teacherPassword)
  const quiz = await request(t.cookies, 'POST', '/api/v1/quizzes', {
    title: QUIZ_TITLE,
  }, 201)
  const quizId = quiz.quiz.id

  await request(t.cookies, 'POST', `/api/v1/quizzes/${quizId}/questions`, {
    type: 'single',
    body: { text: 'Which planet is known as the Red Planet?' },
    options: [
      { key: 'a', text: 'Venus' },
      { key: 'b', text: 'Mars' },
    ],
    correct: 'b',
    points: 1,
  }, 201)

  await request(t.cookies, 'PUT', `/api/v1/quizzes/${quizId}/assignments`, {
    student_ids: [student.user.id],
  }, 200)

  const startsAt = new Date(Date.now() + LIVE_DELAY_MS)
  const endsAt = new Date(startsAt.getTime() + WINDOW_MS)
  await request(t.cookies, 'POST', `/api/v1/quizzes/${quizId}/publish`, {
    starts_at: startsAt.toISOString(),
    ends_at: endsAt.toISOString(),
    duration_sec: DURATION_SEC,
  }, 200)

  await new Promise((resolve) => setTimeout(resolve, LIVE_DELAY_MS + 1_000))
  return { quizId }
}

async function teacherOpensLiveMonitor(browser) {
  const context = await browser.createBrowserContext()
  const page = await context.newPage()
  await page.setViewport({ width: 1280, height: 1600 })
  await page.goto(BASE, { waitUntil: 'networkidle0' })
  await page.waitForSelector('#login-email', { timeout: 5000 })
  await type(page, '#login-email', teacherEmail)
  await type(page, '#login-password', teacherPassword)
  await page.click('button[type=submit]')
  check(
    await waitForText(page, '.qt-title', QUIZ_TITLE, 8000),
    'the quiz list shows the live quiz',
  )
  await clickButtonWithText(page, QUIZ_TITLE)
  check(
    await waitForText(page, '.chip-status', 'Live', 8000),
    'the editor reads the quiz as Live',
  )
  const gotPanel = await page
    .waitForSelector('.live-monitor-panel', { timeout: 8000 })
    .then(() => true)
    .catch(() => false)
  check(gotPanel, 'the Live roster panel renders for a live quiz')
  check(
    await waitForText(page, '.save-badge', 'Live', 8000),
    'the WebSocket connects (socket badge reads "Live")',
  )
  check(
    await waitForText(page, '.chip-roster-not_started', 'Not started', 5000),
    'the roster shows the assigned student as not started',
  )
  await shot(page, '92-live-monitor-not-started.png')
  return { context, page }
}

async function studentStartsAndAnswers(browser) {
  const context = await browser.createBrowserContext()
  const page = await context.newPage()
  await page.setViewport({ width: 1280, height: 1600 })
  await page.goto(BASE, { waitUntil: 'networkidle0' })
  await page.waitForSelector('#login-email', { timeout: 5000 })
  await type(page, '#login-email', studentEmail)
  await type(page, '#login-password', studentPassword)
  await page.click('button[type=submit]')
  await waitForText(page, '.assigned-card', QUIZ_TITLE, 8000)
  await clickButtonWithText(page, 'Start quiz')
  await waitForText(page, '.player-topbar .page-title', QUIZ_TITLE, 8000)
  await pickOption(page, 1, 'Mars')
  await waitForText(page, '.save-badge', 'All changes saved', 8000)
  return { context, page }
}

async function teacherSeesProgressThenKicks(teacherPage) {
  // The progress delta is coalesced up to 2 s per docs/05 section 5; give it
  // room to arrive over the socket.
  check(
    await waitForText(teacherPage, '.chip-roster-in_progress', 'In progress', 10000),
    'the roster flips to In progress live, over the socket',
  )
  const progressOk = await waitForText(teacherPage, '.live-roster-table', '1 / 1', 6000)
  check(progressOk, 'the progress cell reads 1 / 1 after the student answers')
  await shot(teacherPage, '93-live-monitor-in-progress.png')

  await clickButtonWithText(teacherPage, 'Kick')
  const modalOpened = await teacherPage
    .waitForSelector('.modal-panel', { timeout: 5000 })
    .then(() => true)
    .catch(() => false)
  check(modalOpened, 'kicking opens the two-step destructive-flow modal')
  await type(teacherPage, '#destructive-reason', 'Not following instructions')
  await clickButtonWithText(teacherPage, 'Continue')
  check(
    await waitForText(teacherPage, '.modal-consequence', 'ends their attempt', 5000),
    'the confirm step restates the consequence in a danger-tint card',
  )
  await shot(teacherPage, '93b-live-monitor-kick-confirm.png')
  await clickButtonWithText(teacherPage, 'Remove student')
  check(
    await waitForText(teacherPage, '.chip-roster-kicked', 'Kicked', 8000),
    'the roster flips to Kicked once the teacher removes the student',
  )
  const readmitVisible = await teacherPage
    .waitForFunction(
      () =>
        [...document.querySelectorAll('button')].some((b) =>
          b.textContent.trim().includes('Readmit'),
        ),
      { timeout: 5000 },
    )
    .then(() => true)
    .catch(() => false)
  check(readmitVisible, 'a Readmit control appears for the kicked row')
  await shot(teacherPage, '94-live-monitor-kicked.png')
}

async function studentIsLockedOut(studentPage) {
  // The player's attempt:{id} socket delivers the kick lockout message
  // (docs/06 section 4 step 4) as soon as the teacher's Kick lands - well
  // before any further student action - so the done screen already shows
  // the reason-aware "removed" copy rather than the generic REST-fallback
  // "closed" text.
  check(
    await waitForText(studentPage, '.player-done', 'You were removed from this quiz', 8000),
    "the kicked student's attempt socket delivers the lockout screen",
  )
  check(
    await waitForText(
      studentPage,
      '.player-done',
      'Reason given: Not following instructions',
      2000,
    ),
    'the lockout screen shows the reason the teacher gave',
  )
}

await mkdir(SHOT_DIR, { recursive: true })
const { quizId } = await provision()
void quizId

const browser = await puppeteer.launch({
  executablePath: CHROMIUM,
  headless: 'new',
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
})

try {
  const teacher = await teacherOpensLiveMonitor(browser)
  const student = await studentStartsAndAnswers(browser)
  await teacherSeesProgressThenKicks(teacher.page)
  await studentIsLockedOut(student.page)
  await teacher.context.close()
  await student.context.close()
} finally {
  await browser.close()
}

if (failures > 0) {
  console.error(`\n${failures} check(s) failed`)
  process.exit(1)
}
console.log('\nAll live monitor checks passed.')
