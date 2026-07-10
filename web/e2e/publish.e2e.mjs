// Browser end-to-end check of the Milestone 3 publish/assign workspace
// against a running stack: `docker compose up` (API on :8080) plus
// `npm run dev` (SPA on :5173).
//
// Drives a real Chromium through the teacher side of the Milestone 3 exit
// criterion - assign an audience and publish a scheduled quiz:
//   1. API-side setup: a ready teacher, two students (one reset and ready),
//      and a cohort containing the second student
//   2. teacher signs in -> creates a draft with one question (wizard step 1)
//   3. audience step lists both students; the Next gate refuses to advance
//      with nobody assigned
//   4. audience = one student checked directly + one whole cohort chip;
//      Next auto-saves and reports the group-expanded result
//   5. schedule step: an empty and a past window are caught client-side; a
//      valid future window with non-default guardrails publishes -> Scheduled
//      chip, version 1, frozen content
//   6. reload proves the scheduled state - window, release policy, guardrail
//      ladder, and audience - all persist; republish reschedules as version 2;
//      the quiz list shows the Scheduled chip
//   7. API-side: the cohort student's GET /quizzes/assigned carries the quiz
//
// Run:  node e2e/publish.e2e.mjs
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
const teacherEmail = `publisher.e2e.${run}@macquiz.local`
const teacherPassword = 'quizzes-launch-on-time'
const studentAEmail = `asha.e2e.${run}@macquiz.local`
const studentBEmail = `zane.e2e.${run}@macquiz.local`
const studentBPassword = 'zane-owns-this-password'
const QUIZ_TITLE = 'Chemical reactions check'
const GROUP_NAME = `Cohort 10-B (${run})`

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

// React controlled inputs ignore assignments to .value, so drive the native
// setter and let the synthetic onChange see a real input event. This is the
// only reliable way to fill a datetime-local input headlessly.
async function setInputValue(page, selector, value) {
  await page.waitForSelector(selector, { timeout: 5000 })
  await page.evaluate(
    (sel, val) => {
      const input = document.querySelector(sel)
      const setter = Object.getOwnPropertyDescriptor(
        window.HTMLInputElement.prototype,
        'value',
      ).set
      setter.call(input, val)
      input.dispatchEvent(new Event('input', { bubbles: true }))
    },
    selector,
    value,
  )
}

/** A Date as a datetime-local input value in the browser's local time. */
function toLocalInput(date) {
  const pad = (n) => String(n).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(
    date.getDate(),
  )}T${pad(date.getHours())}:${pad(date.getMinutes())}`
}

const minutesFromNow = (min) => new Date(Date.now() + min * 60_000)

// Click a wizard step in the header. The step button's text is the index
// glued to the label ("2Audience"), so a substring match on the label lands it.
async function goToStep(page, label) {
  await clickButtonWithText(page, label, '.wizard-steps')
}

/** The label of the wizard step currently marked aria-current="step". */
async function activeStep(page) {
  return page.$eval('.wizard-steps button[aria-current="step"]', (el) =>
    (el.textContent ?? '').trim(),
  )
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

// A ready teacher, two students (Zane fully reset), and a cohort holding
// Zane - everything the publish flow needs, provisioned over the API.
async function provisionCast() {
  const admin = await login(ADMIN_EMAIL, ADMIN_PASSWORD)

  const post = async (path, body) => {
    const res = await fetch(`${BASE}${path}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Cookie: admin.cookies },
      body: JSON.stringify(body),
    })
    if (res.status !== 201) {
      throw new Error(`POST ${path} failed: ${res.status}`)
    }
    return res.json()
  }

  const teacher = await post('/api/v1/users', {
    role: 'teacher',
    email: teacherEmail,
    full_name: 'Priya Publisher',
  })
  const studentA = await post('/api/v1/users', {
    role: 'student',
    email: studentAEmail,
    full_name: 'Asha Rao',
  })
  const studentB = await post('/api/v1/users', {
    role: 'student',
    email: studentBEmail,
    full_name: 'Zane Park',
  })

  const group = await post('/api/v1/groups', { name: GROUP_NAME })
  const members = await fetch(
    `${BASE}/api/v1/groups/${group.group.id}/members`,
    {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json', Cookie: admin.cookies },
      body: JSON.stringify({ student_ids: [studentB.user.id] }),
    },
  )
  if (members.status !== 200) {
    throw new Error(`set group members failed: ${members.status}`)
  }
  await fetch(`${BASE}/api/v1/auth/logout`, {
    method: 'POST',
    headers: { Cookie: admin.cookies },
  })

  await completeReset(teacherEmail, teacher.initial_password, teacherPassword)
  await completeReset(
    studentBEmail,
    studentB.initial_password,
    studentBPassword,
  )
  return { studentAId: studentA.user.id, studentBId: studentB.user.id }
}

// --- The publish journey -----------------------------------------------------

async function publishFlow(browser) {
  const page = await browser.newPage()
  await page.setViewport({ width: 1280, height: 1400 })
  await page.goto(BASE, { waitUntil: 'networkidle0' })

  await page.waitForSelector('#login-email', { timeout: 5000 })
  await type(page, '#login-email', teacherEmail)
  await type(page, '#login-password', teacherPassword)
  await page.click('button[type=submit]')
  check(
    await waitForText(page, '.page-title', 'Quizzes'),
    'teacher lands on the quizzes workspace',
  )

  // A draft with one question, so the only publish blocker is the audience.
  await clickButtonWithText(page, 'New quiz')
  await type(page, '#new-quiz-title', QUIZ_TITLE)
  await clickButtonWithText(page, 'Create draft')
  await page.waitForSelector('.add-question-panel', { timeout: 8000 })
  await clickButtonWithText(page, 'Single choice', '.add-question-panel')
  check(
    await waitForText(page, '.save-indicator', 'All changes saved', 8000),
    'the draft with one question autosaves',
  )

  // --- Step 2: the audience picker loads the teacher-readable directory. ---
  await goToStep(page, 'Audience')
  check(
    await waitForText(page, '.audience-row', 'Asha Rao', 8000),
    'the audience picker lists the directly assignable student',
  )
  check(
    await waitForText(page, '.group-chip', GROUP_NAME),
    'the audience picker lists the cohort with a member count',
  )
  check(
    await waitForText(page, '.audience-count', 'No students assigned yet'),
    'a fresh draft reports an empty audience',
  )
  await shot(page, '20-audience-step.png')

  // The wizard's Next is the friendly gate that replaces reaching the server's
  // empty-audience precondition: with nobody assigned it refuses to advance.
  await clickButtonWithText(page, 'Next', '.wizard-nav-actions')
  check(
    await waitForText(
      page,
      '.wizard-step-error',
      'Assign at least one student',
    ),
    'advancing past the audience step with nobody assigned is blocked inline',
  )
  check(
    (await activeStep(page)).includes('Audience'),
    'the audience step stays active when Next is blocked',
  )
  await shot(page, '21-audience-gate.png')

  // Audience: Asha directly, Zane via the whole cohort. Next auto-saves.
  await page.click('.audience-row input[type=checkbox]')
  await clickButtonWithText(page, GROUP_NAME, '.group-chip-row')
  await clickButtonWithText(page, 'Next', '.wizard-nav-actions')
  check(
    (await activeStep(page)).includes('Schedule'),
    'a non-empty audience lets Next advance to the schedule step',
  )
  // Return to the audience step to prove the Next auto-save landed and the
  // server-expanded audience came back.
  await goToStep(page, 'Audience')
  check(
    await waitForText(page, '.audience-count', '2 students assigned'),
    'the Next auto-save reports the group-expanded count',
  )
  check(
    (await page.$$eval(
      '.audience-row input[type=checkbox]',
      (boxes) => boxes.filter((b) => b.checked).length,
    )) === 2,
    'the cohort member reads as checked after the expansion',
  )
  check(
    (await page.$$eval('.group-chip-on', (chips) => chips.length)) === 0,
    'the cohort chip resets once its members are individual assignments',
  )
  await shot(page, '22-audience-saved.png')

  // --- Step 3: schedule + guardrails. ---
  await goToStep(page, 'Schedule')

  // Publish with an empty schedule: the client catches it before any request.
  await clickButtonWithText(page, 'Publish quiz')
  check(
    await waitForText(page, '.field-error', 'required'),
    'an empty window is refused client-side',
  )

  // A past open time is caught client-side with the server vocabulary.
  await setInputValue(
    page,
    '#publish-starts-at',
    toLocalInput(minutesFromNow(-10)),
  )
  await setInputValue(
    page,
    '#publish-ends-at',
    toLocalInput(minutesFromNow(90)),
  )
  await clickButtonWithText(page, 'Publish quiz')
  check(
    await waitForText(page, '.field-error', 'must be in the future'),
    'a past open time is refused client-side',
  )

  // The real publish, with non-default guardrails so the reload below can
  // prove the ladder round-trips instead of resetting to the defaults.
  await setInputValue(
    page,
    '#publish-starts-at',
    toLocalInput(minutesFromNow(30)),
  )
  await setInputValue(
    page,
    '#publish-ends-at',
    toLocalInput(minutesFromNow(90)),
  )
  // Withhold the scores until this teacher releases them, rather than the
  // auto default the worker applies at close.
  await page.select('#publish-release-policy', 'manual')
  await page.select('#guardrail-fullscreen', 'count')
  await setInputValue(page, '#guardrail-max-violations', '7')
  await clickButtonWithText(page, 'Publish quiz')
  check(
    await waitForText(page, '.chip-status', 'Scheduled', 8000),
    'publishing flips the status chip to Scheduled',
  )
  check(
    await waitForText(page, '.window-summary', 'Version 1'),
    'the scheduled summary shows version 1 and the window',
  )
  check(
    await page.$eval('#quiz-title', (el) => el.disabled),
    'the quiz settings freeze after publishing',
  )
  check(
    await page.$eval('.question-text', (el) => el.disabled),
    'the question content freezes after publishing',
  )
  check(
    (await page.$$('.question-card .icon-button')).length === 0,
    'reorder and delete controls disappear after publishing',
  )
  await shot(page, '23-published.png')

  // Cold reload: the scheduled state is server truth, not local state.
  await page.reload({ waitUntil: 'networkidle0' })
  await clickButtonWithText(page, QUIZ_TITLE)
  // Reopen lands on step 1; the scheduled facts live on the schedule step.
  await goToStep(page, 'Schedule')
  check(
    await waitForText(page, '.window-summary', 'Version 1', 8000),
    'the scheduled state survives a cold reload',
  )
  check(
    (await page.$eval('#publish-release-policy', (el) => el.value)) ===
      'manual',
    'the chosen manual release policy came back from the server',
  )
  // The guardrail ladder must reseed from the server; before the read-path
  // fix it silently fell back to the defaults (off / 3), so a blind republish
  // would have reset it.
  check(
    (await page.$eval('#guardrail-fullscreen', (el) => el.value)) === 'count',
    'the published fullscreen guardrail round-trips after a cold reload',
  )
  check(
    (await page.$eval('#guardrail-max-violations', (el) => el.value)) === '7',
    'the published violation threshold round-trips after a cold reload',
  )
  check(
    await waitForText(page, '#publish-button', 'Reschedule & republish'),
    'a scheduled quiz offers republish instead of publish',
  )
  await goToStep(page, 'Audience')
  check(
    await waitForText(page, '.audience-count', '2 students assigned', 8000),
    'the saved audience survives a cold reload',
  )
  await goToStep(page, 'Schedule')
  // Past the debounce window: merely opening a frozen quiz must not fire a
  // ghost autosave (which would 409 against the published snapshot).
  await new Promise((resolve) => setTimeout(resolve, 1500))
  check(
    await waitForText(page, '.save-indicator', 'All changes saved'),
    'opening a scheduled quiz fires no ghost autosave',
  )

  // Republish with a fresh window: version 2.
  await setInputValue(
    page,
    '#publish-starts-at',
    toLocalInput(minutesFromNow(60)),
  )
  await setInputValue(
    page,
    '#publish-ends-at',
    toLocalInput(minutesFromNow(120)),
  )
  await clickButtonWithText(page, 'Reschedule & republish')
  check(
    await waitForText(page, '.window-summary', 'Version 2', 8000),
    'republishing reschedules the quiz as version 2',
  )
  await shot(page, '24-republished.png')

  // The list shows the lifecycle chip.
  await clickButtonWithText(page, 'All quizzes')
  check(
    await page
      .waitForFunction(
        (title) =>
          [...document.querySelectorAll('.qt-row')].some(
            (row) =>
              row.textContent.includes(title) &&
              row.textContent.includes('Scheduled'),
          ),
        { timeout: 5000 },
        QUIZ_TITLE,
      )
      .then(() => true)
      .catch(() => false),
    'the quiz list shows the Scheduled chip',
  )
  await shot(page, '25-list-scheduled.png')
  await page.close()
}

// The other half of the assignment: the cohort student really sees the quiz.
async function assignedListCheck() {
  const zane = await login(studentBEmail, studentBPassword)
  const res = await fetch(`${BASE}/api/v1/quizzes/assigned`, {
    headers: { Cookie: zane.cookies },
  })
  const body = await res.json()
  const quiz = (body.quizzes ?? []).find((q) => q.title === QUIZ_TITLE)
  check(
    res.status === 200 && Boolean(quiz),
    'the cohort-assigned student sees the quiz in GET /quizzes/assigned',
  )
  check(
    quiz?.status === 'scheduled' &&
      quiz?.question_count === 1 &&
      quiz?.version === 2,
    'the assigned quiz carries the scheduled window, snapshot size, and version 2',
  )
}

await mkdir(SHOT_DIR, { recursive: true })
await provisionCast()

const browser = await puppeteer.launch({
  executablePath: CHROMIUM,
  headless: 'new',
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
})

try {
  await publishFlow(browser)
  await assignedListCheck()
} finally {
  await browser.close()
}

if (failures > 0) {
  console.error(`\n${failures} check(s) failed`)
  process.exit(1)
}
console.log('\nAll publish/assign checks passed.')
