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
//   3. Start quiz -> the player shows all four questions, a countdown, and
//      autosaves each answer (3 correct, the multi deliberately wrong)
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

// Click the option row whose visible text equals `optionText`, inside the
// player question at 1-based `position` - exact match, so option "2" never
// hits "25".
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
    await waitForText(page, '.page-title', 'My quizzes'),
    'the student lands on the My quizzes workspace',
  )
  check(
    await waitForText(page, '.assigned-card', QUIZ_TITLE, 8000),
    'the assigned list shows the quiz',
  )
  check(
    await waitForText(page, '.assigned-card .chip-status', 'Live'),
    'the quiz reads Live inside its window',
  )
  check(
    await waitForText(page, '.assigned-meta', '4 questions'),
    'the card shows the snapshot question count',
  )
  await shot(page, '30-assigned-live.png')

  // Start: the player with all four questions and a running countdown.
  await clickButtonWithText(page, 'Start quiz')
  check(
    await waitForText(page, '.player-topbar .page-title', QUIZ_TITLE, 8000),
    'starting opens the player with the quiz title',
  )
  check(
    (await page.$$('.player-question')).length === 4,
    'the player shows all four snapshot questions',
  )
  check(
    await page.$eval('.countdown', (el) => /\d:\d\d/.test(el.textContent)),
    'the countdown is running',
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
  await type(page, '.player-short-input', 'mitochondria')
  check(
    await waitForText(page, '.player-footer-note', 'Every question has an answer'),
    'the footer counts every question as answered',
  )
  check(
    await waitForText(page, '.save-badge', 'All changes saved', 8000),
    'every answer autosaves',
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
    await waitForText(page, '.player-topbar .page-title', QUIZ_TITLE, 8000),
    'resume reopens the player',
  )
  check(
    await page.evaluate(() => {
      const q1 = document.querySelectorAll('.player-question')[0]
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
    (await page.$eval('.player-short-input', (el) => el.value)) ===
      'mitochondria',
    'the short answer survives the reload',
  )
  await shot(page, '32-player-resumed.png')

  // Submit through the inline confirm, then back to the list: the attempt
  // is terminal and its score withheld until release.
  await clickButtonWithText(page, 'Submit attempt')
  await clickButtonWithText(page, 'Submit now')
  check(
    await waitForText(page, '.player-done', 'Attempt submitted', 8000),
    'submitting lands on the done panel',
  )
  await shot(page, '33-submitted.png')
  await clickButtonWithText(page, 'Back to my quizzes')
  check(
    await waitForText(page, '.attempt-row', 'Attempt 1', 8000),
    'the list shows the finished attempt',
  )
  check(
    await waitForText(page, '.attempt-score', 'Score withheld'),
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
    await waitForText(page, '.assigned-card .chip-status', 'Closed', 8000),
    'after the window the quiz reads Closed',
  )
  check(
    await waitForText(page, '.attempt-score', '4 pts', 8000),
    'the released score shows on the attempt row',
  )
  await shot(page, '35-score-released.png')

  await clickButtonWithText(page, 'Review')
  check(
    await waitForText(page, '.score-figure', '4', 8000) &&
      (await waitForText(page, '.score-figure', '/ 6')),
    'the review banner shows 4 / 6 points',
  )
  const verdicts = await page.$$eval('.chip-verdict-correct', (els) => els.length)
  check(verdicts === 3, `three questions read Correct (got ${verdicts})`)
  check(
    (await page.$$eval('.chip-verdict-incorrect', (els) => els.length)) === 1,
    'the deliberately wrong multi reads Incorrect',
  )
  check(
    await page.evaluate(() => {
      const q2 = document.querySelectorAll('.review-question')[1]
      const keyRows = [...q2.querySelectorAll('.review-option-key')].map(
        (row) => row.querySelector('.option-static').textContent.trim(),
      )
      return keyRows.length === 2 && keyRows.includes('2') && keyRows.includes('5')
    }),
    'the review marks both correct options of the missed multi',
  )
  check(
    await waitForText(page, '.short-review', 'mitochondria'),
    'the short answer review shows the accepted answer',
  )
  await shot(page, '36-review.png')
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
