// Browser end-to-end check of the Milestone 8 "CSV exports" gap (docs/07
// section 4) against a running stack: `docker compose up` (API on :8080)
// plus `npm run dev` (SPA on :5173).
//
// Drives a real Chromium through: a teacher publishes a 1-question quiz to
// one student with a short live window, the student takes and submits it,
// the worker closes the quiz, and the teacher's Analytics panel shows a
// "Download results CSV" link that actually serves a valid CSV gradebook
// with the student's row in it.
//
// Run:  node e2e/resultscsv.e2e.mjs
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
const teacherEmail = `csv.e2e.${run}@macquiz.local`
const teacherPassword = 'csv-exports-are-my-business'
const aliceEmail = `alice.csv.e2e.${run}@macquiz.local`
const alicePassword = 'alice-downloads-the-gradebook'
const QUIZ_TITLE = 'CSV export check'

const LIVE_DELAY_MS = 3_000
const WINDOW_MS = 40_000
const DURATION_SEC = 30

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

async function login(email, password) {
  const res = await fetch(`${BASE}/api/v1/auth/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email, password }),
  })
  if (!res.ok) throw new Error(`login ${email} failed: ${res.status}`)
  return { cookies: cookiesOf(res), body: await res.json() }
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

async function provision() {
  const admin = await login(ADMIN_EMAIL, ADMIN_PASSWORD)
  const teacher = await request(admin.cookies, 'POST', '/api/v1/users', {
    role: 'teacher',
    email: teacherEmail,
    full_name: 'Cass Exporter',
  }, 201)
  const alice = await request(admin.cookies, 'POST', '/api/v1/users', {
    role: 'student',
    email: aliceEmail,
    full_name: 'Alice Ace',
  }, 201)
  await fetch(`${BASE}/api/v1/auth/logout`, {
    method: 'POST',
    headers: { Cookie: admin.cookies },
  })
  await completeReset(teacherEmail, teacher.initial_password, teacherPassword)
  await completeReset(aliceEmail, alice.initial_password, alicePassword)

  const t = await login(teacherEmail, teacherPassword)
  const quiz = await request(t.cookies, 'POST', '/api/v1/quizzes', {
    title: QUIZ_TITLE,
  }, 201)
  const quizId = quiz.quiz.id

  await request(t.cookies, 'POST', `/api/v1/quizzes/${quizId}/questions`, {
    type: 'truefalse',
    body: { text: 'CSV exports are part of Milestone 8.' },
    correct: true,
    points: 1,
  }, 201)

  await request(t.cookies, 'PUT', `/api/v1/quizzes/${quizId}/assignments`, {
    student_ids: [alice.user.id],
  }, 200)

  const startsAt = new Date(Date.now() + LIVE_DELAY_MS)
  const endsAt = new Date(startsAt.getTime() + WINDOW_MS)
  await request(t.cookies, 'POST', `/api/v1/quizzes/${quizId}/publish`, {
    starts_at: startsAt.toISOString(),
    ends_at: endsAt.toISOString(),
    duration_sec: DURATION_SEC,
  }, 200)

  await new Promise((resolve) => setTimeout(resolve, LIVE_DELAY_MS + 1_000))
  return { quizId, endsAt }
}

async function takeAttempt(browser) {
  const context = await browser.createBrowserContext()
  const page = await context.newPage()
  await page.setViewport({ width: 1280, height: 1600 })
  await page.goto(BASE, { waitUntil: 'networkidle0' })
  await page.waitForSelector('#login-email', { timeout: 5000 })
  await type(page, '#login-email', aliceEmail)
  await type(page, '#login-password', alicePassword)
  await page.click('button[type=submit]')
  await waitForText(page, '.assigned-card', QUIZ_TITLE, 8000)
  await clickButtonWithText(page, 'Start quiz')
  await waitForText(page, '.player-topbar .page-title', QUIZ_TITLE, 8000)
  await pickOption(page, 1, 'True')
  await waitForText(page, '.save-badge', 'All changes saved', 8000)
  await clickButtonWithText(page, 'Submit attempt')
  await clickButtonWithText(page, 'Submit now')
  await waitForText(page, '.player-done', 'Attempt submitted', 8000)
  await context.close()
  check(true, 'Alice takes and submits her attempt')
}

async function waitForClose(endsAt) {
  const t = await login(teacherEmail, teacherPassword)
  const deadline = endsAt.getTime() + 60_000
  for (;;) {
    const body = await request(t.cookies, 'GET', '/api/v1/quizzes', undefined, 200)
    const quiz = (body.quizzes ?? []).find((q) => q.title === QUIZ_TITLE)
    if (quiz?.status === 'closed') {
      check(true, 'the worker closes the quiz at ends_at')
      return
    }
    if (Date.now() > deadline) {
      check(false, `the worker closes the quiz at ends_at (status was ${quiz?.status})`)
      return
    }
    await new Promise((resolve) => setTimeout(resolve, 2_000))
  }
}

async function csvExportFlow(browser) {
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
    'the quiz list shows the closed quiz',
  )
  await clickButtonWithText(page, QUIZ_TITLE)
  check(
    await waitForText(page, '.chip-status', 'Closed', 8000),
    'the editor reads the quiz as Closed',
  )

  const gotPanel = await page
    .waitForSelector('.stats-panel', { timeout: 15000 })
    .then(() => true)
    .catch(() => false)
  check(gotPanel, 'the Analytics panel renders once the quiz closes')
  if (!gotPanel) {
    await shot(page, '95-csv-export-missing-panel.png')
    return
  }

  const href = await page.$eval(
    '.stats-panel-head a[download]',
    (el) => el.getAttribute('href'),
  ).catch(() => null)
  check(
    typeof href === 'string' && href.endsWith('/results.csv'),
    `the panel shows a "Download results CSV" link (got href ${href})`,
  )
  await shot(page, '96-csv-export-panel.png')

  if (href) {
    // Fetch the link's target in-page so the browser's real session cookie
    // is used, same as a real click-to-download would send.
    const result = await page.evaluate(async (url) => {
      const res = await fetch(url)
      return {
        status: res.status,
        contentType: res.headers.get('content-type'),
        disposition: res.headers.get('content-disposition'),
        body: await res.text(),
      }
    }, href)
    check(result.status === 200, `GET ${href} = ${result.status}, want 200`)
    check(
      (result.contentType ?? '').startsWith('text/csv'),
      `content-type = ${result.contentType}, want text/csv`,
    )
    check(
      (result.disposition ?? '').includes('attachment'),
      `content-disposition = ${result.disposition}, want attachment`,
    )
    const lines = result.body.trim().split('\n')
    check(lines.length === 2, `CSV has a header + one student row (got ${lines.length} lines)`)
    check(
      result.body.includes(aliceEmail),
      "the CSV row contains the student's email",
    )
    check(
      result.body.includes('submitted'),
      'the CSV row shows the submitted status',
    )
  }
}

await mkdir(SHOT_DIR, { recursive: true })
const { endsAt } = await provision()

const browser = await puppeteer.launch({
  executablePath: CHROMIUM,
  headless: 'new',
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
})

try {
  await takeAttempt(browser)
  await waitForClose(endsAt)
  await csvExportFlow(browser)
} finally {
  await browser.close()
}

if (failures > 0) {
  console.error(`\n${failures} check(s) failed`)
  process.exit(1)
}
console.log('\nAll CSV export checks passed.')
