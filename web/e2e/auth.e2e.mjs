// Browser end-to-end check of the Milestone 1 auth flows against a running
// stack: `docker compose up` (API on :8080) plus `npm run dev` (SPA on :5173).
//
// Drives a real Chromium through:
//   1. bad-password rejection on the sign-in screen
//   2. admin sign-in -> the Overview console, session survives a reload, sign out
//   3. admin provisions a teacher over the API (one-time credential)
//   4. teacher first sign-in -> forced password reset (mismatch error, then
//      success) -> auto re-login -> the teacher quizzes workspace, and the
//      one-time password is dead
//
// Run:  node e2e/auth.e2e.mjs
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

const teacherEmail = `teacher.e2e.${process.pid}@macquiz.local`
const teacherFinalPassword = 'carrots-outrun-mondays'

let failures = 0
function check(ok, label) {
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${label}`)
  if (!ok) failures++
}

async function shot(page, name) {
  // Let the card entrance animation (0.35s) finish so captures show the
  // settled state.
  await new Promise((resolve) => setTimeout(resolve, 500))
  await page.screenshot({ path: `${SHOT_DIR}/${name}` })
}

async function textOf(page, selector) {
  return page.$eval(selector, (el) => el.textContent ?? '').catch(() => '')
}

async function waitForText(page, selector, needle) {
  await page
    .waitForFunction(
      (sel, want) =>
        [...document.querySelectorAll(sel)].some((el) =>
          (el.textContent ?? '').includes(want),
        ),
      { timeout: 5000 },
      selector,
      needle,
    )
    .catch(() => {})
  return (await textOf(page, selector)).includes(needle)
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

async function signIn(page, email, password) {
  await page.waitForSelector('#login-email', { timeout: 5000 })
  await type(page, '#login-email', email)
  await type(page, '#login-password', password)
  await page.click('button[type=submit]')
}

// --- API-side setup: provision a teacher as the admin -----------------------

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

async function provisionTeacher() {
  const login = await loginRetry(ADMIN_EMAIL, ADMIN_PASSWORD)
  if (!login.ok) throw new Error(`admin API login failed: ${login.status}`)
  const cookies = login.headers
    .getSetCookie()
    .map((c) => c.split(';')[0])
    .join('; ')

  const created = await fetch(`${BASE}/api/v1/users`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Cookie: cookies },
    body: JSON.stringify({
      role: 'teacher',
      email: teacherEmail,
      full_name: 'Edna Krabappel',
    }),
  })
  if (created.status !== 201) {
    throw new Error(`provisioning failed: ${created.status}`)
  }
  const body = await created.json()
  await fetch(`${BASE}/api/v1/auth/logout`, {
    method: 'POST',
    headers: { Cookie: cookies },
  })
  return body.initial_password
}

// --- Browser flows -----------------------------------------------------------

async function adminFlow(browser) {
  const page = await browser.newPage()
  await page.setViewport({ width: 1280, height: 860 })
  await page.goto(BASE, { waitUntil: 'networkidle0' })

  check(
    await waitForText(page, '.page-title', 'Sign in'),
    'landing on the app unauthenticated shows the sign-in screen',
  )
  await shot(page, '01-login.png')

  await signIn(page, ADMIN_EMAIL, 'definitely-wrong-password')
  check(
    await waitForText(page, '.form-error', 'not right'),
    'wrong password shows the invalid-credentials error',
  )
  await shot(page, '02-login-error.png')

  await signIn(page, ADMIN_EMAIL, ADMIN_PASSWORD)
  check(
    await waitForText(page, '.page-title', 'Overview'),
    'admin lands on the Overview console after signing in',
  )
  check(
    await waitForText(page, '.chip-role', 'Admin'),
    'admin role chip reads Admin',
  )
  await shot(page, '03-admin-home.png')

  await page.reload({ waitUntil: 'networkidle0' })
  check(
    await waitForText(page, '.page-title', 'Overview'),
    'admin session survives a full page reload',
  )

  await page.click('.rail-signout')
  check(
    await waitForText(page, '.page-title', 'Sign in'),
    'sign out returns to the sign-in screen',
  )
  await page.close()
}

async function teacherFlow(browser, oneTimePassword) {
  const page = await browser.newPage()
  await page.setViewport({ width: 1280, height: 860 })
  await page.goto(BASE, { waitUntil: 'networkidle0' })

  await signIn(page, teacherEmail, oneTimePassword)
  check(
    await waitForText(page, '.page-title', 'Set your password'),
    'teacher first sign-in lands on the forced password-reset screen',
  )
  await shot(page, '04-forced-reset.png')

  await type(page, '#current-password', oneTimePassword)
  await type(page, '#new-password', teacherFinalPassword)
  await type(page, '#confirm-password', 'something-else-entirely')
  await page.click('button[type=submit]')
  check(
    await waitForText(page, '.form-error', 'do not match'),
    'mismatched confirmation is rejected client-side',
  )
  await shot(page, '05-reset-mismatch.png')

  await type(page, '#confirm-password', teacherFinalPassword)
  await page.click('button[type=submit]')
  check(
    await waitForText(page, '.page-title', 'Quizzes'),
    'password change auto re-signs the teacher in and lands on the quizzes workspace',
  )
  check(
    await waitForText(page, '.chip-role', 'Teacher'),
    'teacher role chip reads Teacher',
  )
  await shot(page, '06-teacher-workspace.png')
  await page.close()

  const stale = await fetch(`${BASE}/api/v1/auth/login`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ email: teacherEmail, password: oneTimePassword }),
  })
  check(stale.status === 401, 'the one-time password no longer works')
}

// --- Sh3: the rate-limited sign-in state ------------------------------------

// The login limiter is 5 attempts per account per minute (docs/08 section 4)
// and it refuses on the *email* before the credential is ever looked up, so an
// address that belongs to nobody trips it exactly the same way. Priming the
// bucket over the API keeps the browser to the one submit that matters: the
// one whose 429 the UI has to render.
async function rateLimitFlow(browser) {
  const victim = `ratelimit.e2e.${process.pid}@macquiz.local`
  for (let i = 0; i < 5; i++) {
    await fetch(`${BASE}/api/v1/auth/login`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ email: victim, password: 'not-the-password' }),
    })
  }

  // Its own cookie jar: teacherFlow leaves a live session behind, and a
  // signed-in browser never renders the sign-in form at all.
  const context = await browser.createBrowserContext()
  const page = await context.newPage()
  await page.goto(BASE, { waitUntil: 'networkidle0' })
  await signIn(page, victim, 'not-the-password')

  check(
    await waitForText(page, '.rate-limit-notice', 'Too many sign-in attempts'),
    'the sixth attempt in a minute renders the rate-limited notice',
  )
  check(
    (await textOf(page, '.chip-lifecycle-warning')).includes('Rate limited'),
    'the notice carries the Rate limited chip',
  )

  const first = await textOf(page, '.rate-limit-countdown-value')
  check(
    /^\d{2}:\d{2}$/.test(first) && first !== '00:00',
    `the countdown reads mm:ss and has time left (got ${JSON.stringify(first)})`,
  )
  check(
    await page.$eval('button[type=submit]', (el) => el.disabled),
    'Sign in is disabled while the lockout runs',
  )
  check(
    (await page.$('.form-error')) === null,
    'the countdown replaces the static error, rather than doubling it',
  )
  await shot(page, '07-rate-limited.png')

  const spokenBefore = await textOf(page, '.rate-limit-notice .visually-hidden')
  check(
    /^\s*Try again in \d+ seconds\.\s*$/.test(spokenBefore),
    `the notice carries a spoken wait (got ${JSON.stringify(spokenBefore)})`,
  )

  // Retry-After is a deadline, not a static number: it must visibly drain.
  await new Promise((resolve) => setTimeout(resolve, 2200))
  const later = await textOf(page, '.rate-limit-countdown-value')
  check(
    later !== first && seconds(later) < seconds(first),
    `the countdown ticks down (${first} -> ${later})`,
  )
  // ...but the text inside the role=alert must not, or a screen reader is
  // interrupted with a fresh announcement on every tick.
  check(
    (await textOf(page, '.rate-limit-notice .visually-hidden')) === spokenBefore,
    'the spoken wait stays frozen while the numeral ticks',
  )
  await context.close()
}

function seconds(mmss) {
  const [m, s] = mmss.split(':').map(Number)
  return m * 60 + s
}

await mkdir(SHOT_DIR, { recursive: true })
const browser = await puppeteer.launch({
  executablePath: CHROMIUM,
  headless: true,
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
})
try {
  await adminFlow(browser)
  const oneTimePassword = await provisionTeacher()
  await teacherFlow(browser, oneTimePassword)
  // Last: it burns six of the per-IP login budget that every flow above shares.
  await rateLimitFlow(browser)
} finally {
  await browser.close()
}

console.log(failures === 0 ? '\nAll auth E2E checks passed.' : `\n${failures} check(s) FAILED.`)
process.exit(failures === 0 ? 0 : 1)
