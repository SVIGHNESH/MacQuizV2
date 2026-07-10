// Browser end-to-end check of the Milestone 2 teacher authoring workspace
// against a running stack: `docker compose up` (API on :8080) plus
// `npm run dev` (SPA on :5173).
//
// Drives a real Chromium through the Milestone 2 exit criterion - a teacher
// creates a draft quiz with questions of all four types:
//   1. provision a fresh teacher over the API and complete the forced reset
//      API-side (browser logins are rate-limited to 5/account/minute)
//   2. teacher signs in -> empty quiz list -> creates a draft -> editor
//   3. adds all four question types, rewrites the single-choice question
//      (text, options, correct answer, points) and waits for autosave
//   4. reorders with the move buttons, deletes a question (two-step)
//   5. reloads the editor to prove every change persisted server-side
//   6. back to the list: the quiz row shows the final question count
//
// Run:  node e2e/authoring.e2e.mjs
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

const teacherEmail = `author.e2e.${process.pid}@macquiz.local`
const teacherPassword = 'gardens-grade-themselves'
const QUIZ_TITLE = 'Photosynthesis basics'

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
  const ok = await page
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
  return ok
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

// Autosave settles when the aggregate indicator returns to "All changes
// saved" after the edit; the debounce is 700 ms so give it room.
async function waitForSaved(page) {
  return waitForText(page, '.save-indicator', 'All changes saved', 8000)
}

async function questionCount(page) {
  return page.$$eval('.question-card', (cards) => cards.length)
}

async function nthQuestionType(page, index) {
  return page.$eval(
    `.question-card:nth-of-type(${index + 1}) .chip-type`,
    (el) => el.textContent ?? '',
  )
}

// --- API-side setup: a ready-to-work teacher --------------------------------

// Retries on 429: the e2e suites share one hardcoded admin account
// (docs/08 section 4's 5/account/minute login limit), so running several
// suites back to back easily exhausts it. The server's Retry-After header
// says exactly how long the sliding window needs to drain.
async function loginRetry(email, password) {
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
    return res
  }
}

async function provisionReadyTeacher() {
  const adminLogin = await loginRetry(ADMIN_EMAIL, ADMIN_PASSWORD)
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
      full_name: 'Nina Okafor',
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

  // Complete the forced first-login reset over the API so the browser part
  // of this check starts at a normal sign-in.
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

// --- The authoring journey ---------------------------------------------------

async function authoringFlow(browser) {
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
  check(
    await waitForText(page, '.panel', 'No quizzes yet'),
    'a fresh teacher sees the empty state',
  )
  await shot(page, '10-quizzes-empty.png')

  // Create a draft.
  await clickButtonWithText(page, 'New quiz')
  await type(page, '#new-quiz-title', QUIZ_TITLE)
  await clickButtonWithText(page, 'Create draft')
  check(
    await waitForText(page, '.save-indicator', 'All changes saved'),
    'creating a draft opens the editor',
  )
  check(
    (await page.$eval('#quiz-title', (el) => el.value)) === QUIZ_TITLE,
    'the editor shows the new draft title',
  )
  check(
    await waitForText(page, '.chip-status', 'Draft'),
    'the draft status chip reads Draft',
  )

  // Add all four question types. Each click is a POST, so wait for the card
  // it creates rather than for a fixed delay - under a full-suite run the
  // server is busy enough that 300 ms was not always enough, and the next
  // click then landed before the previous question existed.
  const TYPES = ['Single choice', 'Multi select', 'True / false', 'Short answer']
  for (const [index, label] of TYPES.entries()) {
    await clickButtonWithText(page, label, '.add-question-panel')
    await page.waitForFunction(
      (want) => document.querySelectorAll('.question-card').length === want,
      { timeout: 8000 },
      index + 1,
    )
  }
  check(
    (await questionCount(page)) === 4,
    'all four question types were added',
  )
  await shot(page, '11-editor-four-types.png')

  // Rewrite the single-choice question: text, options, correct, points.
  const q1 = '.question-card:nth-of-type(1)'
  await type(
    page,
    `${q1} .question-text`,
    'Which gas do plants absorb for photosynthesis?',
  )
  await type(page, `${q1} .option-row:nth-of-type(1) .option-text`, 'Oxygen')
  await type(
    page,
    `${q1} .option-row:nth-of-type(2) .option-text`,
    'Carbon dioxide',
  )
  await clickButtonWithText(page, 'Add option', q1)
  const withThird = await page.$$(`${q1} .option-text`)
  await withThird[2].type('Nitrogen')
  // Mark option B correct.
  await page.click(`${q1} .option-row:nth-of-type(2) input`)
  await type(page, `${q1} .input-points`, '2')
  check(
    await waitForSaved(page),
    'the rewritten single-choice question autosaves',
  )
  check(
    await page
      .$eval(`${q1} .option-row:nth-of-type(2)`, (el) =>
        el.classList.contains('option-row-selected'),
      )
      .catch(() => false),
    'the marked option renders as selected',
  )
  await shot(page, '12-question-edited.png')

  // --- Preview: the draft toolbar's student view -----------------------------
  // The whole point of the feature is that it shows what a student receives,
  // so the load-bearing assertion is the negative one: the option the teacher
  // just marked correct must NOT come through as selected.
  await clickButtonWithText(page, 'Preview', '.editor-topline-actions')
  check(
    await waitForText(page, '.preview-panel .modal-title', QUIZ_TITLE),
    'the Preview button opens the student preview',
  )
  check(
    await waitForText(
      page,
      '.preview-question-text',
      'Which gas do plants absorb for photosynthesis?',
    ),
    'the preview shows the question text the teacher just saved, not the stale load',
  )
  check(
    (await page.$$eval('.preview-panel .option-row', (rows) => rows.length)) ===
      3,
    'the preview renders all three options of the single-choice question',
  )
  check(
    (await page.$$eval('.preview-panel .option-row-selected', (r) => r.length)) ===
      0 &&
      (await page.$$eval('.preview-panel .option-row input:checked', (r) => r.length)) === 0,
    'the preview does not reveal the correct answer',
  )
  check(
    await page.$eval(
      '.preview-actions button',
      (el) => el.textContent.trim() === 'Previous' && el.disabled,
    ),
    'Previous is disabled on the first question',
  )
  await shot(page, '12b-preview.png')
  await clickButtonWithText(page, 'Next', '.preview-actions')
  check(
    await waitForText(page, '.preview-panel .chip-type', 'Multi select'),
    'Next pages the preview to the second question',
  )
  await page.keyboard.press('Escape')
  check(
    await page
      .waitForFunction(() => !document.querySelector('.preview-panel'), {
        timeout: 3000,
      })
      .then(() => true)
      .catch(() => false),
    'Escape dismisses the preview',
  )

  // Reorder: move the short-answer question (4th) up one.
  await page.click(
    '.question-card:nth-of-type(4) [aria-label="Move question up"]',
  )
  check(
    await page
      .waitForFunction(
        () =>
          document
            .querySelectorAll('.question-card')[2]
            ?.querySelector('.chip-type')
            ?.textContent?.includes('Short answer'),
        { timeout: 5000 },
      )
      .then(() => true)
      .catch(() => false),
    'moving a question up reorders the list',
  )

  // Delete the true/false question (now 4th) with the two-step control.
  await clickButtonWithText(page, 'Delete', '.question-card:nth-of-type(4)')
  await clickButtonWithText(
    page,
    'Remove question',
    '.question-card:nth-of-type(4)',
  )
  check(
    await page
      .waitForFunction(
        () => document.querySelectorAll('.question-card').length === 3,
        { timeout: 5000 },
      )
      .then(() => true)
      .catch(() => false),
    'the two-step delete removes the question',
  )

  // Reload: everything above must have persisted server-side (autosave).
  await page.reload({ waitUntil: 'networkidle0' })
  check(
    await waitForText(page, '.page-title', 'Quizzes'),
    'the session survives the reload back to the quiz list',
  )
  await clickButtonWithText(page, QUIZ_TITLE)
  check(
    await page
      .waitForSelector('.question-card', { timeout: 8000 })
      .then(() => true)
      .catch(() => false),
    'reopening the quiz shows the loaded editor',
  )
  check(
    (await page
      .$eval('.question-card:nth-of-type(1) .question-text', (el) => el.value)
      .catch(() => '')) === 'Which gas do plants absorb for photosynthesis?',
    'the edited question text persisted through autosave',
  )
  check(
    (await page
      .$eval(
        '.question-card:nth-of-type(1) .option-row:nth-of-type(2) .option-text',
        (el) => el.value,
      )
      .catch(() => '')) === 'Carbon dioxide',
    'the edited option text persisted through autosave',
  )
  check((await questionCount(page)) === 3, 'the deletion persisted')
  check(
    (await nthQuestionType(page, 2)).includes('Short answer'),
    'the reorder persisted',
  )
  check(
    (await page
      .$eval(
        '.question-card:nth-of-type(1) .input-points',
        (el) => el.value,
      )
      .catch(() => '')) === '2',
    'the points change persisted',
  )
  await shot(page, '13-editor-after-reload.png')

  // Back to the list: the row shows the final question count.
  await clickButtonWithText(page, 'All quizzes')
  check(
    await waitForText(page, '.qt-row', QUIZ_TITLE),
    'the quiz list shows the draft',
  )
  check(
    (await page
      .$eval('.qt-row .qt-num', (el) => el.textContent)
      .catch(() => '')) === '3',
    'the list shows the final question count',
  )
  await shot(page, '14-quiz-list.png')

  await page.close()
}

await mkdir(SHOT_DIR, { recursive: true })
const browser = await puppeteer.launch({
  executablePath: CHROMIUM,
  headless: true,
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
})
try {
  await provisionReadyTeacher()
  await authoringFlow(browser)
} finally {
  await browser.close()
}

console.log(
  failures === 0
    ? '\nAll authoring E2E checks passed.'
    : `\n${failures} check(s) FAILED.`,
)
process.exit(failures === 0 ? 0 : 1)
