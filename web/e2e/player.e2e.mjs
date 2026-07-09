// Browser end-to-end check of the Milestone 4 student attempt player and
// results review against a running stack: `docker compose up` (API on :8080)
// plus `npm run dev` (SPA on :5173).
//
// Drives a real Chromium through the student half of the Milestone 4 exit
// criterion - take a quiz in the browser and read the released results:
//   1. API-side setup: a ready teacher and student; the teacher authors a
//      4-question quiz (one of each type) and publishes it with a short
//      live window (auto release policy)
//   2. student signs in -> the assigned list shows the quiz Live
//   3. Start quiz -> the player shows one question at a time with a
//      four-cell sidebar grid and a countdown, and autosaves each answer
//      (3 correct, the multi deliberately wrong)
//   4. a cold reload mid-attempt -> Resume restores every saved answer
//   5. submit -> the done panel, then the list shows the attempt with the
//      score withheld pre-release
//   6. the worker closes, grades, and auto-releases at ends_at; the list
//      then shows the released score and a Review button
//   7. the review shows 4/6, per-question verdicts, and the answer key
//      (the wrong multi's correct options, the short answer's accepted list)
//
// Run:  node e2e/player.e2e.mjs   (takes ~4 minutes - it waits out the window)
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
const teacherEmail = `examiner.e2e.${run}@macquiz.local`
const teacherPassword = 'graded-fairly-every-time'
const studentEmail = `mira.e2e.${run}@macquiz.local`
const studentPassword = 'mira-takes-the-quiz'
const QUIZ_TITLE = 'Space and cells check'

// The window: live 3 s from publish, open for 100 s, 85 s per attempt.
// The browser flow (start, answer, reload, resume, submit) fits well
// inside the attempt budget; the auto release rides the close at ends_at.
const LIVE_DELAY_MS = 3_000
const WINDOW_MS = 100_000
const DURATION_SEC = 85

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

// Jump to the question at 1-based `position` via the sidebar grid, then
// wait until the single visible question panel shows that position.
async function goToQuestion(page, position) {
  const clicked = await page.evaluate((pos) => {
    const cell = document.querySelectorAll('.nav-cell')[pos - 1]
    if (!cell) return false
    cell.click()
    return true
  }, position)
  if (!clicked) throw new Error(`no grid cell for question ${position}`)
  // The navigator's current cell is the only one carrying the position of
  // the question the pane is showing.
  await page.waitForFunction(
    (pos) =>
      document.querySelector('.nav-cell-current')?.textContent.trim() ===
      String(pos),
    { timeout: 5000 },
    position,
  )
}

// Click the option row whose visible text equals `optionText`, inside the
// player question at 1-based `position` - exact match, so option "2" never
// hits "25". Navigates to the question first: only one is visible at a time.
async function pickOption(page, position, optionText) {
  await goToQuestion(page, position)
  const clicked = await page.evaluate((want) => {
    const panel = document.querySelector('.player-question-area')
    if (!panel) return false
    const row = [...panel.querySelectorAll('.option-row')].find(
      (el) =>
        (el.querySelector('.option-static')?.textContent ?? '').trim() === want,
    )
    if (!row) return false
    row.querySelector('input').click()
    return true
  }, optionText)
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

// A ready teacher and student, and the teacher's published 4-question quiz
// (one of each type, 6 points total) assigned to the student.
async function provision() {
  const admin = await login(ADMIN_EMAIL, ADMIN_PASSWORD)
  const teacher = await request(admin.cookies, 'POST', '/api/v1/users', {
    role: 'teacher',
    email: teacherEmail,
    full_name: 'Evan Examiner',
  }, 201)
  const student = await request(admin.cookies, 'POST', '/api/v1/users', {
    role: 'student',
    email: studentEmail,
    full_name: 'Mira Patel',
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

  const questions = [
    {
      type: 'single',
      body: { text: 'Which planet is known as the Red Planet?' },
      options: [
        { key: 'a', text: 'Venus' },
        { key: 'b', text: 'Mars' },
        { key: 'c', text: 'Jupiter' },
      ],
      correct: 'b',
      points: 1,
    },
    {
      type: 'multi',
      body: { text: 'Which of these numbers are prime?' },
      options: [
        { key: 'a', text: '2' },
        { key: 'b', text: '4' },
        { key: 'c', text: '5' },
        { key: 'd', text: '9' },
      ],
      correct: ['a', 'c'],
      points: 2,
    },
    {
      type: 'truefalse',
      body: { text: 'The Sun is a star.' },
      correct: true,
      points: 1,
    },
    {
      type: 'short',
      body: { text: 'Which organelle is the powerhouse of the cell?' },
      correct: { accepted: ['mitochondria'] },
      points: 2,
    },
  ]
  for (const q of questions) {
    await request(t.cookies, 'POST', `/api/v1/quizzes/${quizId}/questions`, q, 201)
  }

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

  // Into the live window before the student arrives.
  await new Promise((resolve) => setTimeout(resolve, LIVE_DELAY_MS + 1_000))
  return { quizId, endsAt }
}

// --- The student journey -----------------------------------------------------

async function playerFlow(browser) {
  const page = await browser.newPage()
  await page.setViewport({ width: 1280, height: 1600 })
  await page.goto(BASE, { waitUntil: 'networkidle0' })

  await page.waitForSelector('#login-email', { timeout: 5000 })
  await type(page, '#login-email', studentEmail)
  await type(page, '#login-password', studentPassword)
  await page.click('button[type=submit]')
  check(
    await waitForText(page, '.page-title', 'Assigned quizzes'),
    'the student lands on the assigned quizzes workspace',
  )
  check(
    await waitForText(page, '.assigned-card', QUIZ_TITLE, 8000),
    'the assigned list shows the quiz',
  )
  check(
    await waitForText(page, '.assigned-card .chip-lifecycle', 'Live'),
    'the quiz reads Live inside its window',
  )
  check(
    await waitForText(page, '.assigned-meta', '4 questions'),
    'the card shows the snapshot question count',
  )
  await shot(page, '30-assigned-live.png')

  // Start: one visible question, the four-cell sidebar grid, and a running
  // countdown.
  await clickButtonWithText(page, 'Start attempt')
  check(
    await waitForText(page, '.player-quiz-title', QUIZ_TITLE, 8000),
    'starting opens the player with the quiz title',
  )
  check(
    (await page.$$('.nav-cell')).length === 4,
    'the sidebar grid lists all four snapshot questions',
  )
  check(
    (await page.$$('.player-question-text')).length === 1,
    'the player shows a single question at a time',
  )
  check(
    await page.$eval('.player-timer-value', (el) => /\d:\d\d/.test(el.textContent)),
    'the countdown is running',
  )
  // The exam chrome owns the viewport: the workspace rail must be gone.
  check(
    (await page.$$('.rail')).length === 0,
    'the rail is hidden during a live attempt',
  )
  check(
    await page.evaluate(
      () => !document.body.innerText.includes('Correct answer'),
    ),
    'the player never shows an answer key',
  )

  // Answer: single and truefalse and short correct, the multi wrong (only
  // one of the two primes), so the released score is a deterministic 4/6.
  await pickOption(page, 1, 'Mars')
  await pickOption(page, 2, '2')
  await pickOption(page, 3, 'True')
  await goToQuestion(page, 4)
  await type(page, '.player-short-input', 'mitochondria')
  check(
    await waitForText(page, '.player-nav-count', '4 of 4 answered'),
    'the navigator counts every question as answered',
  )
  check(
    (await page.$$('.nav-cell-answered')).length === 4,
    'the sidebar grid marks every question answered',
  )
  check(
    await waitForText(page, '.save-state', 'All changes saved', 8000),
    'every answer autosaves',
  )
  // Flagging is a client-side navigation aid: the marker rides the cell.
  await clickButtonWithText(page, 'Flag')
  check(
    (await page.$$('.nav-flag')).length === 1,
    'flagging the current question marks its grid cell',
  )
  await shot(page, '31-player-answered.png')

  // Cold reload mid-attempt: the attempt survives server-side and the list
  // offers Resume; resuming restores every autosaved answer.
  await page.reload({ waitUntil: 'networkidle0' })
  check(
    await waitForText(page, '.assigned-card button', 'Resume attempt', 8000),
    'after a reload the card offers Resume',
  )
  await clickButtonWithText(page, 'Resume attempt')
  check(
    await waitForText(page, '.player-quiz-title', QUIZ_TITLE, 8000),
    'resume reopens the player',
  )
  check(
    await page.evaluate(() => {
      const q1 = document.querySelector('.player-question-area')
      const picked = [...q1.querySelectorAll('.option-row')].find((row) =>
        row.querySelector('input').checked,
      )
      return (
        (picked?.querySelector('.option-static')?.textContent ?? '') === 'Mars'
      )
    }),
    'the single-choice answer survives the reload',
  )
  check(
    (await page.$$('.nav-cell-answered')).length === 4,
    'the resumed sidebar grid remembers every answered question',
  )
  await goToQuestion(page, 4)
  check(
    (await page.$eval('.player-short-input', (el) => el.value)) ===
      'mitochondria',
    'the short answer survives the reload',
  )
  await shot(page, '32-player-resumed.png')

  // Submit through the inline confirm, then back to the list: the attempt
  // is terminal and its score withheld until release.
  await clickButtonWithText(page, 'Review and submit')
  await clickButtonWithText(page, 'Submit now')
  check(
    await waitForText(page, '.player-done', 'Attempt submitted', 8000),
    'submitting lands on the done panel',
  )
  await shot(page, '33-submitted.png')
  await clickButtonWithText(page, 'Back to assigned quizzes')
  check(
    await waitForText(page, '.assigned-card .chip-lifecycle', 'Submitted', 8000),
    'the list shows the finished attempt as submitted',
  )
  check(
    await waitForText(page, '.assigned-note', 'Results not released yet'),
    'the score stays withheld before release',
  )
  await shot(page, '34-score-withheld.png')
  return page
}

// The worker closes the quiz at ends_at, grades are already in, and the
// auto policy releases in the same pass - poll the student's own list API
// until the release lands.
async function waitForRelease(endsAt) {
  const student = await login(studentEmail, studentPassword)
  const deadline = endsAt.getTime() + 120_000
  for (;;) {
    const res = await fetch(`${BASE}/api/v1/quizzes/assigned`, {
      headers: { Cookie: student.cookies },
    })
    const body = await res.json()
    const quiz = (body.quizzes ?? []).find((q) => q.title === QUIZ_TITLE)
    if (quiz?.results_released_at && quiz.attempts[0]?.score !== null) {
      check(true, 'the worker auto-releases the results at close')
      check(
        quiz.attempts[0].score === 4,
        `the released score is the expected 4 (got ${quiz.attempts[0]?.score})`,
      )
      return
    }
    if (Date.now() > deadline) {
      check(false, 'the worker auto-releases the results at close')
      return
    }
    await new Promise((resolve) => setTimeout(resolve, 3_000))
  }
}

async function reviewFlow(page) {
  await page.reload({ waitUntil: 'networkidle0' })
  check(
    await waitForText(page, '.assigned-card .chip-lifecycle', 'Closed', 8000),
    'after the window the quiz reads Closed',
  )
  check(
    await waitForText(page, '.assigned-score', '4 pts', 8000),
    'the released score shows on the card',
  )
  await shot(page, '35-score-released.png')

  await clickButtonWithText(page, 'Review answers')
  // 4 of 6 points is 67%; the hero card carries the percentage, the points
  // card the raw fraction.
  check(
    await waitForText(page, '.score-figure', '67%', 8000),
    'the hero card shows the score as a percentage',
  )
  check(
    await waitForText(page, '.stat-card-value', '4 / 6'),
    'the points card shows 4 / 6',
  )
  check(
    await waitForText(page, '.stat-card-hero-sub', '3 of 4 correct'),
    'the hero card counts the correct questions',
  )
  const verdicts = await page.$$eval(
    '.answer-verdict-correct',
    (els) => els.length,
  )
  check(verdicts === 3, `three questions read Correct (got ${verdicts})`)
  check(
    (await page.$$eval('.answer-verdict-incorrect', (els) => els.length)) === 1,
    'the deliberately wrong multi reads Incorrect',
  )
  // The answer key spells out both missed options with their text, since the
  // table never lists the options themselves.
  check(
    await page.evaluate(() => {
      const key = document
        .querySelectorAll('.answer-row')[1]
        ?.querySelector('.answer-row-key')?.textContent
      return Boolean(key?.includes('· 2') && key?.includes('· 5'))
    }),
    'the review marks both correct options of the missed multi',
  )
  check(
    await waitForText(page, '.answer-row-response', 'mitochondria'),
    'the short answer row shows what the student typed',
  )
  // The key is evidence the student still needs; a question they already got
  // right does not restate it.
  check(
    await page.evaluate(
      () =>
        document.querySelectorAll('.answer-row').length === 4 &&
        document.querySelectorAll('.answer-row-key').length === 1,
    ),
    'only the missed question restates the answer key',
  )
  await shot(page, '36-review.png')

  // St6: the student's own rollup. The worker writes it on the close+grade
  // sweep, so it is normally there by the time the review renders; the screen
  // must still degrade to an empty state rather than to zeroes if it lags.
  await clickButtonWithText(page, 'My analytics')
  check(
    await waitForText(page, '.page-title', 'My analytics', 8000),
    'the rail reaches the analytics destination',
  )
  const rolledUp = await page
    .waitForSelector('.stat-cards', { timeout: 20_000 })
    .then(() => true)
    .catch(() => false)
  if (rolledUp) {
    check(
      await waitForText(page, '.stat-card-value', '67%', 4000),
      'average accuracy matches the released 4/6 attempt',
    )
    check(
      await waitForText(page, '.stat-card-value', '100%', 4000),
      'completion rate counts the one closed quiz as done',
    )
    check(
      (await page.$$('.trend-bar')).length === 1,
      'the accuracy trend plots the one graded quiz',
    )
  } else {
    check(
      await waitForText(page, '.empty-state', 'Nothing to summarise yet'),
      'a missing rollup reads as an empty state, not as zeroes',
    )
  }
  await shot(page, '37-my-analytics.png')
}

await mkdir(SHOT_DIR, { recursive: true })
const { endsAt } = await provision()

const browser = await puppeteer.launch({
  executablePath: CHROMIUM,
  headless: 'new',
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
})

try {
  const page = await playerFlow(browser)
  await waitForRelease(endsAt)
  await reviewFlow(page)
} finally {
  await browser.close()
}

if (failures > 0) {
  console.error(`\n${failures} check(s) failed`)
  process.exit(1)
}
console.log('\nAll player/results checks passed.')
