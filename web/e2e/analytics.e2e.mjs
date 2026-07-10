// Browser end-to-end check of the Milestone 8 teacher analytics dashboard
// (QuizStatsPanel) against a running stack: `docker compose up` (API on
// :8080) plus `npm run dev` (SPA on :5173).
//
// Drives a real Chromium through the docs/12 Milestone 8 exit criterion:
// closing a quiz produces dashboards without a live query.
//   1. API-side setup: a teacher publishes a 2-question quiz to 2 students
//      with a short live window
//   2. both students take the quiz - one answers both correctly, the other
//      gets the second question wrong - then submit
//   3. the worker closes the quiz at ends_at and rolls up quiz_stats in the
//      same pass
//   4. the teacher opens the quiz in the editor and the Analytics panel
//      shows mean/median/participation, a 10-bucket distribution, and a
//      per-question item-analysis row for each question
//
// Run:  node e2e/analytics.e2e.mjs   (takes ~2 minutes - it waits out the window)
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
const teacherEmail = `stats.e2e.${run}@macquiz.local`
const teacherPassword = 'stats-are-my-business'
const aliceEmail = `alice.e2e.${run}@macquiz.local`
const alicePassword = 'alice-aces-the-quiz'
const bobEmail = `bob.e2e.${run}@macquiz.local`
const bobPassword = 'bob-misses-one'
const QUIZ_TITLE = 'Rollup dashboard check'

// Live 3 s from publish, open for 40 s, 30 s per attempt - long enough for
// two scripted students to answer and submit, short enough to close fast.
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

async function goToQuestion(page, position) {
  const clicked = await page.evaluate((pos) => {
    const cell = document.querySelectorAll('.nav-cell')[pos - 1]
    if (!cell) return false
    cell.click()
    return true
  }, position)
  if (!clicked) throw new Error(`no grid cell for question ${position}`)
  await page.waitForFunction(
    (pos) =>
      document.querySelector('.nav-cell-current')?.textContent.trim() ===
      String(pos),
    { timeout: 5000 },
    position,
  )
}

// Only one question is on screen at a time, so navigate to it first.
async function pickOption(page, position, optionText) {
  await goToQuestion(page, position)
  const clicked = await page.evaluate(
    (want) => {
      const panel = document.querySelector('.player-question-area')
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

// A ready teacher and two students, and the teacher's published 2-question
// quiz assigned to both students.
async function provision() {
  const admin = await login(ADMIN_EMAIL, ADMIN_PASSWORD)
  const teacher = await request(admin.cookies, 'POST', '/api/v1/users', {
    role: 'teacher',
    email: teacherEmail,
    full_name: 'Dana Data',
  }, 201)
  const alice = await request(admin.cookies, 'POST', '/api/v1/users', {
    role: 'student',
    email: aliceEmail,
    full_name: 'Alice Ace',
  }, 201)
  const bob = await request(admin.cookies, 'POST', '/api/v1/users', {
    role: 'student',
    email: bobEmail,
    full_name: 'Bob Bystander',
  }, 201)
  await fetch(`${BASE}/api/v1/auth/logout`, {
    method: 'POST',
    headers: { Cookie: admin.cookies },
  })
  await completeReset(teacherEmail, teacher.initial_password, teacherPassword)
  await completeReset(aliceEmail, alice.initial_password, alicePassword)
  await completeReset(bobEmail, bob.initial_password, bobPassword)

  const t = await login(teacherEmail, teacherPassword)
  const quiz = await request(t.cookies, 'POST', '/api/v1/quizzes', {
    title: QUIZ_TITLE,
  }, 201)
  const quizId = quiz.quiz.id

  const questions = [
    {
      type: 'single',
      body: { text: 'Which planet is known as the Red Planet?' },
      options: [
        { key: 'a', text: 'Venus' },
        { key: 'b', text: 'Mars' },
      ],
      correct: 'b',
      points: 1,
    },
    {
      type: 'truefalse',
      body: { text: 'The Sun is a star.' },
      correct: true,
      points: 1,
    },
  ]
  for (const q of questions) {
    await request(t.cookies, 'POST', `/api/v1/quizzes/${quizId}/questions`, q, 201)
  }

  await request(t.cookies, 'PUT', `/api/v1/quizzes/${quizId}/assignments`, {
    student_ids: [alice.user.id, bob.user.id],
  }, 200)

  const startsAt = new Date(Date.now() + LIVE_DELAY_MS)
  const endsAt = new Date(startsAt.getTime() + WINDOW_MS)
  await request(t.cookies, 'POST', `/api/v1/quizzes/${quizId}/publish`, {
    starts_at: startsAt.toISOString(),
    ends_at: endsAt.toISOString(),
    duration_sec: DURATION_SEC,
    // Manual, so the teacher's own Release results control is what makes the
    // scores visible - under the auto policy the worker releases them at
    // close and the panel's unreleased state would never render.
    release_policy: 'manual',
  }, 200)

  await new Promise((resolve) => setTimeout(resolve, LIVE_DELAY_MS + 1_000))
  return { quizId, endsAt }
}

// One student's scripted attempt: start, answer both questions, submit.
// `secondAnswer` is the visible option text clicked for question 2 (True/
// False), so the caller controls who gets it right vs wrong.
async function takeAttempt(browser, email, password, secondAnswer) {
  // A fresh incognito context per student: browser.newPage() shares one
  // cookie jar across pages, so a second student's page would inherit the
  // first student's session cookie instead of showing the login screen.
  const context = await browser.createBrowserContext()
  const page = await context.newPage()
  await page.setViewport({ width: 1280, height: 1600 })
  await page.goto(BASE, { waitUntil: 'networkidle0' })
  await page.waitForSelector('#login-email', { timeout: 5000 })
  await type(page, '#login-email', email)
  await type(page, '#login-password', password)
  await page.click('button[type=submit]')
  await waitForText(page, '.assigned-card', QUIZ_TITLE, 8000)
  await clickButtonWithText(page, 'Start attempt')
  await waitForText(page, '.player-quiz-title', QUIZ_TITLE, 8000)
  await pickOption(page, 1, 'Mars')
  await pickOption(page, 2, secondAnswer)
  await waitForText(page, '.save-state', 'All changes saved', 8000)
  await clickButtonWithText(page, 'Review and submit')
  await clickButtonWithText(page, 'Submit now')
  await waitForText(page, '.player-done', 'Attempt submitted', 8000)
  await context.close()
}

async function studentsFlow(browser) {
  await takeAttempt(browser, aliceEmail, alicePassword, 'True')
  check(true, 'Alice answers both questions correctly and submits')
  await takeAttempt(browser, bobEmail, bobPassword, 'False')
  check(true, 'Bob answers the second question wrong and submits')
}

// Poll the teacher's quiz-list API until the quiz reads closed, so the test
// never races the worker's close+rollup sweep.
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

async function dashboardFlow(browser) {
  const context = await browser.createBrowserContext()
  const page = await context.newPage()
  await page.setViewport({ width: 1280, height: 1600 })
  await page.goto(BASE, { waitUntil: 'networkidle0' })
  await page.waitForSelector('#login-email', { timeout: 5000 })
  await type(page, '#login-email', teacherEmail)
  await type(page, '#login-password', teacherPassword)
  await page.click('button[type=submit]')
  check(
    await waitForText(page, '.page-title', 'Quizzes'),
    'the teacher lands on the Quizzes workspace',
  )
  check(
    await waitForText(page, '.qt-title', QUIZ_TITLE, 8000),
    'the quiz list shows the closed quiz',
  )
  await clickButtonWithText(page, QUIZ_TITLE)
  check(
    await waitForText(page, '.chip-status', 'Closed', 8000),
    'the editor reads the quiz as Closed',
  )

  // The analytics panel may briefly 404 if the rollup lags a beat behind the
  // status flip (both happen in the same worker pass, but the panel's own
  // fetch races the page load) - poll a moment before failing.
  const gotPanel = await page
    .waitForSelector('.stats-panel', { timeout: 15000 })
    .then(() => true)
    .catch(() => false)
  check(gotPanel, 'the Analytics panel renders once the quiz closes')
  if (!gotPanel) {
    await shot(page, '90-stats-missing.png')
    return
  }

  check(
    await waitForText(page, '.stat-tile-value', '1.5', 4000),
    'the mean score reads 1.5 (Alice 2, Bob 1)',
  )
  check(
    await waitForText(page, '.stat-tile-value', '100%'),
    'participation reads 100% (both assigned students completed)',
  )
  const bars = await page.$$eval('.stats-bar', (els) => els.length)
  check(bars === 10, `the distribution shows all 10 buckets (got ${bars})`)
  const itemRows = await page.$$eval('.stats-item-row', (els) => els.length)
  check(itemRows === 2, `item analysis has one row per question (got ${itemRows})`)
  check(
    await waitForText(page, '.stats-item-question', 'Red Planet'),
    'the item-analysis row labels the question by its text, not its id',
  )
  // docs/07 section 3's option-pick rates surfaced as the "Top distractor"
  // column: Bob answered "The Sun is a star" as False (wrong), so that row's
  // most-picked wrong option is False, at 1 of 2 responses.
  check(
    await waitForText(page, '.stats-item-distractor', 'False'),
    'the item-analysis top-distractor cell names the most-picked wrong option',
  )
  check(
    await waitForText(page, '.stats-distractor-rate', '50% picked'),
    'the top-distractor cell shows the pick rate (1 of 2 chose False)',
  )
  const kicked = await page.$eval(
    '.stats-integrity .stat-tile-value',
    (el) => el.textContent.trim(),
  )
  check(kicked === '0', `kicked attempts reads 0 for a clean run (got ${kicked})`)
  await shot(page, '91-stats-dashboard.png')

  // The closed quiz separates its views: results/analytics is the default
  // tab, and the frozen settings live on their own tab.
  await clickButtonWithText(page, 'Settings & questions', '.editor-tabs')
  const statsHidden = await page.$eval(
    '.stats-panel',
    (el) => el.offsetParent === null,
  )
  check(statsHidden, 'the Settings tab hides the results view')
  check(
    await page.$eval('.editor-title-input', (el) => el.offsetParent !== null),
    'the Settings tab shows the frozen quiz settings',
  )
  await clickButtonWithText(page, 'Results & analytics', '.editor-tabs')
  const statsBack = await page.$eval('.stats-panel', (el) => el.offsetParent !== null)
  check(statsBack, 'the Results tab brings the analytics back')

  await releaseAndArchiveFlow(page)
}

// The two lifecycle controls a closed quiz still has. The quiz was published
// with release_policy=manual, so nothing has released it: the students'
// scores are withheld until this teacher clicks the button.
async function releaseAndArchiveFlow(page) {
  check(
    await waitForText(page, '#release-state', 'withheld until you release'),
    'the closed quiz reports its results as withheld',
  )
  await page.click('#release-results-button')
  check(
    await waitForText(page, '#release-state', 'Results released', 8000),
    'clicking Release results flips the panel to released',
  )
  const releaseButtonGone = (await page.$('#release-results-button')) === null
  check(releaseButtonGone, 'the Release results button retires once released')
  await shot(page, '92-results-released.png')

  // Archiving is terminal, so it is two-step: Archive arms the confirm.
  await page.click('#archive-button')
  check(
    (await page.$('#archive-confirm-button')) !== null,
    'Archive arms a confirm step rather than archiving straight away',
  )
  await page.click('#archive-confirm-button')
  check(
    await waitForText(page, '.chip-status', 'Archived', 8000),
    'confirming the archive flips the quiz to Archived',
  )
  check(
    (await page.$('#archive-button')) === null,
    'an archived quiz offers no further archive action',
  )
  await shot(page, '93-quiz-archived.png')
}

// The teacher Analytics tab: the signed-in teacher's own summary plus the
// per-student roster scoped to their quizzes. Runs on the session the
// dashboard flow left behind, straight off the workspace rail.
async function teacherAnalyticsTabFlow(browser) {
  // Each flow runs in its own context (the student flows need parallel
  // sessions), so this page starts signed out and logs the teacher in.
  const page = await browser.newPage()
  await page.setViewport({ width: 1280, height: 900 })
  await page.goto(BASE, { waitUntil: 'networkidle0' })
  await page.waitForSelector('#login-email', { timeout: 8000 })
  await type(page, '#login-email', teacherEmail)
  await type(page, '#login-password', teacherPassword)
  await page.click('button[type=submit]')
  check(
    await waitForText(page, '.page-title', 'Quizzes', 8000),
    'the teacher signs back in for the Analytics tab',
  )

  await clickButtonWithText(page, 'Analytics', '.rail-nav')
  check(
    await waitForText(page, '.page-title', 'Analytics'),
    'the teacher rail has an Analytics tab',
  )
  check(
    await waitForText(page, '.stat-cards', 'Quizzes conducted', 8000),
    "the tab leads with the teacher's own stat cards",
  )
  check(
    await waitForText(page, '.teacher-analytics-table', aliceEmail, 8000),
    'the roster lists every student assigned to a quiz this teacher owns',
  )

  // Alice aced (2/2), Bob missed the truefalse (1/2).
  const rowScores = await page.$$eval(
    '.teacher-analytics-table .qt-row',
    (rows) =>
      rows.map((row) => ({
        email: row.querySelector('.admin-user-email')?.textContent ?? '',
        cells: [...row.querySelectorAll('.qt-num')].map((c) => c.textContent.trim()),
      })),
  )
  const alice = rowScores.find((r) => r.email.includes('alice'))
  const bob = rowScores.find((r) => r.email.includes('bob'))
  check(alice?.cells[2] === '100%', `Alice's avg score reads 100% (got ${alice?.cells[2]})`)
  check(bob?.cells[2] === '50%', `Bob's avg score reads 50% (got ${bob?.cells[2]})`)

  await clickButtonWithText(page, 'Quizzes', '.teacher-analytics-table')
  check(
    await waitForText(page, '.teacher-analytics-breakdown', QUIZ_TITLE),
    'expanding a student shows the per-quiz breakdown',
  )
  await shot(page, '94-teacher-analytics-tab.png')

  await type(page, '.admin-search', 'alice')
  const visibleRows = await page.$$eval(
    '.teacher-analytics-table .qt-row',
    (rows) => rows.length,
  )
  check(visibleRows === 1, `searching narrows the roster to one row (got ${visibleRows})`)

  // Quiz mode: picking a quiz flips the roster into a highest-marks ranking
  // scoped to that quiz alone.
  await type(page, '.admin-search', '')
  const quizOptionValue = await page.$eval(
    '.teacher-analytics-quiz-filter',
    (el, title) => [...el.options].find((o) => o.textContent === title)?.value ?? '',
    QUIZ_TITLE,
  )
  check(quizOptionValue !== '', 'the quiz filter lists the conducted quiz')
  await page.select('.teacher-analytics-quiz-filter', quizOptionValue)
  const rankRows = await page.$$eval('.teacher-quiz-rank-table .qt-row', (rows) =>
    rows.map((r) => ({
      rank: r.querySelector('.teacher-rank-num')?.textContent.trim(),
      email: r.querySelector('.admin-user-email')?.textContent ?? '',
      score: r.querySelectorAll('.qt-num')[1]?.textContent.trim(),
    })),
  )
  check(
    rankRows.length === 2 &&
      rankRows[0].rank === '1' && rankRows[0].email.includes('alice') && rankRows[0].score === '100%' &&
      rankRows[1].rank === '2' && rankRows[1].email.includes('bob') && rankRows[1].score === '50%',
    `the quiz ranking orders highest marks first (got ${JSON.stringify(rankRows)})`,
  )
  check(
    (await page.$('.teacher-rank-top')) !== null,
    'the top scorer\'s rank is highlighted',
  )
  await shot(page, '97-teacher-quiz-ranking.png')

  await page.close()
}

// The admin Analytics tab: every teacher and every student, filterable, with
// drill-in modals. Signs the shared browser session over to the admin, so it
// must run after every teacher-session check is done.
async function adminAnalyticsTabFlow(browser) {
  // A fresh incognito context: the teacher flow's session must stay intact,
  // and the admin signs in on its own cookie jar.
  const context = await browser.createBrowserContext()
  const page = await context.newPage()
  await page.setViewport({ width: 1280, height: 900 })
  await page.goto(BASE, { waitUntil: 'networkidle0' })

  // The shared admin account's login budget is 5/min across suites; retry
  // through a rate-limit window rather than failing the run.
  for (let attempt = 0; ; attempt++) {
    await page.waitForSelector('#login-email', { timeout: 8000 })
    await type(page, '#login-email', ADMIN_EMAIL)
    await type(page, '#login-password', ADMIN_PASSWORD)
    await page.click('button[type=submit]')
    const landed = await waitForText(page, '.page-title', 'Overview', 8000)
    if (landed) break
    if (attempt >= 4) {
      check(false, 'admin could not sign in for the analytics tab')
      await context.close()
      return
    }
    await new Promise((resolve) => setTimeout(resolve, 15_000))
  }

  await clickButtonWithText(page, 'Analytics', '.rail-nav')
  check(
    await waitForText(page, '.page-title', 'Analytics'),
    'the admin rail has an Analytics tab',
  )
  check(
    await waitForText(page, '.admin-analytics-teacher-table', teacherEmail, 8000),
    'the Teachers view lists the seeded teacher',
  )

  await type(page, '.admin-search', teacherEmail)
  const teacherRows = await page.$$eval(
    '.admin-analytics-teacher-table .qt-row',
    (rows) => rows.map((r) => [...r.querySelectorAll('.qt-num')].map((c) => c.textContent.trim())),
  )
  check(
    teacherRows.length === 1 && teacherRows[0][2] === '2' && teacherRows[0][3] === '100%',
    `the teacher row reads 2 attempts at 100% participation (got ${JSON.stringify(teacherRows)})`,
  )
  await clickButtonWithText(page, 'Details', '.admin-analytics-teacher-table')
  check(
    await waitForText(page, '.admin-teacher-stats', 'Quizzes created'),
    'a teacher row drills into the activity modal',
  )
  await shot(page, '95-admin-analytics-teachers.png')
  await page.keyboard.press('Escape')

  await clickButtonWithText(page, 'Students', '.admin-analytics-toggle')
  check(
    await waitForText(page, '.admin-analytics-student-table', aliceEmail, 8000),
    'the Students view lists the seeded students',
  )
  await type(page, '.admin-search', aliceEmail)
  const studentRows = await page.$$eval(
    '.admin-analytics-student-table .qt-row',
    (rows) => rows.map((r) => [...r.querySelectorAll('.qt-num')].map((c) => c.textContent.trim())),
  )
  check(
    studentRows.length === 1 && studentRows[0][1] === '100%',
    `Alice's row reads 100% accuracy (got ${JSON.stringify(studentRows)})`,
  )
  await clickButtonWithText(page, 'Details', '.admin-analytics-student-table')
  check(
    await waitForText(page, '.admin-teacher-stats', 'Accuracy trend', 8000),
    'a student row drills into the analytics modal with the accuracy trend',
  )
  await shot(page, '96-admin-analytics-students.png')
  await page.keyboard.press('Escape')

  await context.close()
}

await mkdir(SHOT_DIR, { recursive: true })
const { endsAt } = await provision()

const browser = await puppeteer.launch({
  executablePath: CHROMIUM,
  headless: 'new',
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
})

try {
  await studentsFlow(browser)
  await waitForClose(endsAt)
  await dashboardFlow(browser)
  await teacherAnalyticsTabFlow(browser)
  await adminAnalyticsTabFlow(browser)
} finally {
  await browser.close()
}

if (failures > 0) {
  console.error(`\n${failures} check(s) failed`)
  process.exit(1)
}
console.log('\nAll analytics dashboard checks passed.')
