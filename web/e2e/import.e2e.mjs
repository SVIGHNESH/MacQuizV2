// Browser end-to-end check of the Milestone 7 bulk-import review UI against
// a running stack: `docker compose up` (API on :8080) plus `npm run dev`
// (SPA on :5173).
//
// Drives a real Chromium through the docs/12 Milestone 7 exit criterion:
//   1. provision a fresh teacher over the API and complete the forced reset
//      API-side, create a draft quiz
//   2. upload a bad CSV (a correct answer not among its own options) ->
//      the panel shows the row-level error report and writes nothing
//   3. upload a clean two-row CSV -> the panel polls to "ready" and commits
//      -> both rows land as questions in the editor
//
// Run:  node e2e/import.e2e.mjs
// Env:  E2E_BASE_URL (default http://localhost:5173)
//       E2E_CHROMIUM (default /usr/bin/chromium)
//       E2E_ADMIN_EMAIL / E2E_ADMIN_PASSWORD (default compose bootstrap creds)
// Screenshots land in /tmp/macquiz-e2e/.

import { mkdir, writeFile } from 'node:fs/promises'
import puppeteer from 'puppeteer-core'

const BASE = process.env.E2E_BASE_URL ?? 'http://localhost:5173'
const CHROMIUM = process.env.E2E_CHROMIUM ?? '/usr/bin/chromium'
const ADMIN_EMAIL = process.env.E2E_ADMIN_EMAIL ?? 'admin@macquiz.local'
const ADMIN_PASSWORD = process.env.E2E_ADMIN_PASSWORD ?? 'admin-dev-password'
const SHOT_DIR = '/tmp/macquiz-e2e'

const teacherEmail = `import.e2e.${process.pid}@macquiz.local`
const teacherPassword = 'rivers-outlast-borders'
const QUIZ_TITLE = 'Bulk import target'

const CSV_HEADER =
  'type,question,option_a,option_b,option_c,option_d,option_e,option_f,correct,points\n'
const BAD_CSV =
  CSV_HEADER + 'single,Pick red,Red,Blue,,,,,z,2\n' // 'z' is not an option
const GOOD_CSV =
  CSV_HEADER +
  'single,Pick red,Red,Blue,,,,,a,2\n' +
  'truefalse,Sky is blue,,,,,,,true,1\n'

let failures = 0
function check(ok, label) {
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${label}`)
  if (!ok) failures++
}

async function shot(page, name) {
  await new Promise((resolve) => setTimeout(resolve, 500))
  await page.screenshot({ path: `${SHOT_DIR}/${name}` })
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

function questionCount(page) {
  return page.$$eval('.question-card', (cards) => cards.length)
}

// --- API-side setup: a ready-to-work teacher --------------------------------

async function provisionReadyTeacher() {
  const adminLogin = await fetch(`${BASE}/api/v1/auth/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email: ADMIN_EMAIL, password: ADMIN_PASSWORD }),
  })
  if (!adminLogin.ok) throw new Error(`admin login failed: ${adminLogin.status}`)
  const adminCookies = adminLogin.headers
    .getSetCookie()
    .map((c) => c.split(';')[0])
    .join('; ')

  const created = await fetch(`${BASE}/api/v1/users`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Cookie: adminCookies },
    body: JSON.stringify({
      role: 'teacher',
      email: teacherEmail,
      full_name: 'Import Tester',
    }),
  })
  if (created.status !== 201) {
    throw new Error(`provisioning failed: ${created.status}`)
  }
  const { initial_password: oneTime } = await created.json()
  await fetch(`${BASE}/api/v1/auth/logout`, {
    method: 'POST',
    headers: { Cookie: adminCookies },
  })

  const teacherLogin = await fetch(`${BASE}/api/v1/auth/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email: teacherEmail, password: oneTime }),
  })
  if (!teacherLogin.ok) {
    throw new Error(`teacher one-time login failed: ${teacherLogin.status}`)
  }
  const teacherCookies = teacherLogin.headers
    .getSetCookie()
    .map((c) => c.split(';')[0])
    .join('; ')
  const changed = await fetch(`${BASE}/api/v1/auth/password`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Cookie: teacherCookies },
    body: JSON.stringify({
      current_password: oneTime,
      new_password: teacherPassword,
    }),
  })
  if (changed.status !== 204) {
    throw new Error(`password change failed: ${changed.status}`)
  }
}

// --- The import review journey ----------------------------------------------

async function importFlow(browser, badCsvPath, goodCsvPath) {
  const page = await browser.newPage()
  await page.setViewport({ width: 1280, height: 960 })
  await page.goto(BASE, { waitUntil: 'networkidle0' })

  await page.waitForSelector('#login-email', { timeout: 5000 })
  await type(page, '#login-email', teacherEmail)
  await type(page, '#login-password', teacherPassword)
  await page.click('button[type=submit]')

  check(
    await waitForText(page, '.page-title', 'Quizzes'),
    'teacher lands on the quizzes workspace',
  )

  await clickButtonWithText(page, 'New quiz')
  await type(page, '#new-quiz-title', QUIZ_TITLE)
  await clickButtonWithText(page, 'Create draft')
  check(
    await waitForText(page, '.save-indicator', 'All changes saved'),
    'creating a draft opens the editor',
  )
  check(
    await page
      .waitForSelector('.import-panel', { timeout: 5000 })
      .then(() => true)
      .catch(() => false),
    'the bulk-import panel is visible on a draft quiz',
  )
  await shot(page, '20-import-empty.png')

  // --- Bad file: row-level error report, nothing imported ---
  {
    const input = await page.$('.import-panel input[type=file]')
    await input.uploadFile(badCsvPath)
    check(
      await waitForText(page, '.import-panel', 'errors and nothing was imported', 8000),
      'a bad file resolves to the row-error report',
    )
    check(
      await page
        .$eval('.import-error-row', (el) => el.textContent ?? '')
        .then((t) => t.includes('Row 1'))
        .catch(() => false),
      'the error report names the offending row',
    )
    await shot(page, '21-import-failed.png')
    check((await questionCount(page)) === 0, 'a failed import writes no questions')

    await clickButtonWithText(page, 'Try another file', '.import-panel')
  }

  // --- Good file: validates, commits, questions land in the editor ---
  {
    const input = await page.$('.import-panel input[type=file]')
    await input.uploadFile(goodCsvPath)
    check(
      await waitForText(page, '.import-panel', 'validated and ready to add', 8000),
      'a clean file resolves to ready with the row count',
    )
    await shot(page, '22-import-ready.png')

    await clickButtonWithText(page, 'Add 2 questions', '.import-panel')
    check(
      await page
        .waitForFunction(() => document.querySelectorAll('.question-card').length === 2, {
          timeout: 5000,
        })
        .then(() => true)
        .catch(() => false),
      'committing adds both imported rows as questions',
    )
    await shot(page, '23-import-committed.png')
  }

  await page.close()
}

await mkdir(SHOT_DIR, { recursive: true })
const badCsvPath = `${SHOT_DIR}/import-bad.csv`
const goodCsvPath = `${SHOT_DIR}/import-good.csv`
await writeFile(badCsvPath, BAD_CSV)
await writeFile(goodCsvPath, GOOD_CSV)

const browser = await puppeteer.launch({
  executablePath: CHROMIUM,
  headless: true,
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
})
try {
  await provisionReadyTeacher()
  await importFlow(browser, badCsvPath, goodCsvPath)
} finally {
  await browser.close()
}

console.log(
  failures === 0
    ? '\nAll import E2E checks passed.'
    : `\n${failures} check(s) FAILED.`,
)
process.exit(failures === 0 ? 0 : 1)
