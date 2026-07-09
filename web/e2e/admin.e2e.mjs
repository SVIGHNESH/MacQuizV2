// Browser end-to-end check of the Milestone 1 admin console (docs/12: "admin
// provisions a teacher and a student") against a running stack:
// `docker compose up` (API on :8080) plus `npm run dev` (SPA on :5173).
//
// Drives a real Chromium through:
//   1. admin provisions a teacher over the Users panel, sees the one-time
//      credential exactly once, and the account appears in the table
//   2. disabling and re-enabling an account round-trips through the table
//   3. admin creates a cohort in the Groups panel, provisions a student,
//      checks the student into the cohort, saves, then reopens the
//      membership editor and confirms it reloads pre-checked (the new GET
//      /groups/:id/members endpoint)
//
// Run:  node e2e/admin.e2e.mjs
// Env:  E2E_BASE_URL (default http://localhost:5173)
//       E2E_CHROMIUM (default /usr/bin/chromium)
//       E2E_ADMIN_EMAIL / E2E_ADMIN_PASSWORD (default compose bootstrap creds)
// Screenshots land in /tmp/macquiz-e2e/.

import { mkdir } from 'node:fs/promises'
import puppeteer from 'puppeteer-core'

const BASE = process.env.E2E_BASE_URL ?? 'http://localhost:5173'
const CHROMIUM = process.env.E2E_CHROMIUM ?? '/usr/bin/chromium'
const ADMIN_EMAIL = process.env.E2E_ADMIN_EMAIL ?? 'admin@macquiz.local'
// docker-compose.yml MACQUIZ_BOOTSTRAP_ADMIN_NAME - the audit log stores only
// actor_id, so the panel joins it against the accounts list to show this.
const ADMIN_NAME = process.env.E2E_ADMIN_NAME ?? 'Dev Admin'
const ADMIN_PASSWORD = process.env.E2E_ADMIN_PASSWORD ?? 'admin-dev-password'
const SHOT_DIR = '/tmp/macquiz-e2e'

const teacherEmail = `admin-e2e-teacher.${process.pid}@macquiz.local`
const studentEmail = `admin-e2e-student.${process.pid}@macquiz.local`
const groupName = `Admin E2E Cohort ${process.pid}`

let failures = 0
function check(ok, label) {
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${label}`)
  if (!ok) failures++
}

async function shot(page, name) {
  await new Promise((resolve) => setTimeout(resolve, 400))
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

async function adminConsoleFlow(browser) {
  const page = await browser.newPage()
  await page.setViewport({ width: 1280, height: 900 })
  await page.goto(BASE, { waitUntil: 'networkidle0' })
  await signIn(page, ADMIN_EMAIL, ADMIN_PASSWORD)
  check(
    await waitForText(page, '.page-title', 'Overview'),
    'admin signs in and lands on the Overview console',
  )
  await page.waitForSelector('.stat-card-hero', { timeout: 5000 })
  check(
    await waitForText(page, '.stat-card-hero', 'Active users'),
    'the overview leads with the active-users hero card',
  )
  await shot(page, 'admin-00-overview.png')

  await clickButtonWithText(page, 'Users', '.rail-nav')
  check(
    await waitForText(page, '.page-title', 'Users'),
    'the Users nav item switches to the accounts console',
  )

  // --- Provision a teacher --------------------------------------------------
  await clickButtonWithText(page, 'Provision user')
  await type(page, '.admin-user-form input[type=email]', teacherEmail)
  await type(page, '.admin-user-form input[type=text]', 'Terry Tester')
  await page.select('.admin-user-form select', 'teacher')
  await page.click('.admin-user-form button[type=submit]')

  check(
    await waitForText(page, '.admin-credential-reveal', 'One-time credential'),
    'creating a teacher reveals a one-time credential',
  )
  const revealed = await textOf(page, '.admin-credential-value')
  check(revealed.trim().length > 0, 'the revealed credential is non-empty')
  check(
    await waitForText(page, '.credential-notice', 'must reset this password'),
    'the provision modal states the first-login consequence',
  )
  await shot(page, 'admin-01-credential-reveal.png')

  check(
    await waitForText(page, '.admin-user-table', teacherEmail),
    'the new teacher appears in the accounts table',
  )
  await page.click('.admin-credential-reveal .button-commit')

  // --- Disable then re-enable the teacher -----------------------------------
  const teacherRow = await page.evaluateHandle(
    (email) =>
      [...document.querySelectorAll('.admin-user-table .qt-row')].find((row) =>
        row.textContent.includes(email),
      ),
    teacherEmail,
  )
  const disableButton = await teacherRow.asElement()?.$('button.button-quiet-danger')
  await disableButton?.click()
  check(
    await waitForText(page, '.admin-user-table', 'Disabled'),
    'disabling an account flips its status chip to Disabled',
  )
  await shot(page, 'admin-02-disabled.png')

  const teacherRowAgain = await page.evaluateHandle(
    (email) =>
      [...document.querySelectorAll('.admin-user-table .qt-row')].find((row) =>
        row.textContent.includes(email),
      ),
    teacherEmail,
  )
  const reenableButton = await teacherRowAgain.asElement()?.$('button.button-quiet:not(.button-quiet-danger)')
  // The row now has two quiet buttons (Reset password, Re-enable); the
  // status-toggle one carries the current label.
  const buttons = await teacherRowAgain.asElement()?.$$('button')
  for (const b of buttons ?? []) {
    const label = await b.evaluate((el) => el.textContent)
    if (label?.includes('Re-enable')) {
      await b.click()
      break
    }
  }
  check(
    await waitForText(page, '.admin-user-table', 'Active'),
    're-enabling restores the Active status chip',
  )
  void reenableButton

  // --- Groups: create a cohort, provision a student, set membership --------
  await clickButtonWithText(page, 'Groups', '.rail-nav')
  check(
    await waitForText(page, '.page-title', 'Groups'),
    'the Groups nav item switches to the cohorts console',
  )

  await page.click('.page-head .button-primary')
  await type(page, '.create-form input[type=text]', groupName)
  await page.click('.create-form button[type=submit]')
  check(
    await waitForText(page, '.admin-group-table', groupName),
    'the new cohort appears in the groups table',
  )

  // Provision the student the membership picker will need, over the API so
  // this run stays a groups-flow check rather than repeating the Users flow.
  const login = await loginRetry(ADMIN_EMAIL, ADMIN_PASSWORD)
  const cookies = login.headers
    .getSetCookie()
    .map((c) => c.split(';')[0])
    .join('; ')
  await fetch(`${BASE}/api/v1/users`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Cookie: cookies },
    body: JSON.stringify({ role: 'student', email: studentEmail, full_name: 'Sam Sample' }),
  })

  // A full reload resets the SPA's client-side view state back to the
  // Overview tab, so land on Groups again before looking for the cohort row.
  await page.reload({ waitUntil: 'networkidle0' })
  await clickButtonWithText(page, 'Groups', '.rail-nav')
  await page.waitForSelector('.admin-group-table', { timeout: 5000 })
  const groupRow = await page.evaluateHandle(
    (name) =>
      [...document.querySelectorAll('.admin-group-table .group-card')].find((row) =>
        row.textContent.includes(name),
      ),
    groupName,
  )
  const editButton = await groupRow.asElement()?.$('button')
  await editButton?.click()
  check(
    await waitForText(page, '.admin-members-panel', studentEmail.split('@')[0]) ||
      (await waitForText(page, '.admin-members-panel', 'Sam Sample')),
    'the membership editor lists the freshly provisioned student',
  )

  const studentRow = await page.evaluateHandle(
    (email) =>
      [...document.querySelectorAll('.admin-members-panel .audience-row')].find((row) =>
        row.textContent.includes(email),
      ),
    studentEmail,
  )
  const checkbox = await studentRow.asElement()?.$('input[type=checkbox]')
  await checkbox?.click()
  await page.click('.admin-members-panel .button-primary')
  check(
    await waitForText(page, '.admin-group-table', '1 member'),
    'saving membership updates the member_count shown on the card',
  )
  await shot(page, 'admin-03-group-members-saved.png')

  // Reopen the editor: this is the real assertion for the new GET
  // /groups/:id/members endpoint - it must reload pre-checked, not blank.
  const groupRowAgain = await page.evaluateHandle(
    (name) =>
      [...document.querySelectorAll('.admin-group-table .group-card')].find((row) =>
        row.textContent.includes(name),
      ),
    groupName,
  )
  const editButtonAgain = await groupRowAgain.asElement()?.$('button')
  await editButtonAgain?.click()
  await page.waitForSelector('.admin-members-panel', { timeout: 5000 })
  await new Promise((resolve) => setTimeout(resolve, 400))
  const rechecked = await page.evaluate((email) => {
    const row = [...document.querySelectorAll('.admin-members-panel .audience-row')].find((r) =>
      r.textContent.includes(email),
    )
    return row?.querySelector('input[type=checkbox]')?.checked ?? false
  }, studentEmail)
  check(rechecked, 'reopening the membership editor shows the saved student pre-checked')
  await shot(page, 'admin-04-group-members-reopened.png')

  // --- Audit log ------------------------------------------------------------
  // Everything this run just did (provision, disable, create group, set
  // members) is append-only evidence; the log must already show it.
  await clickButtonWithText(page, 'Audit log', '.rail-nav')
  check(
    await waitForText(page, '.page-title', 'Audit log', 5000),
    'the Audit log nav item opens the append-only trail',
  )
  await page.waitForSelector('.audit-row', { timeout: 5000 })
  check(
    await waitForText(page, '.audit-table', 'created'),
    'the log records the accounts and cohorts this run created',
  )
  // actor_id is a uuid on the wire; the row must resolve it to a human name.
  check(
    await waitForText(page, '.audit-actor', ADMIN_NAME),
    'the log resolves actor_id to the admin account name',
  )
  const resolvesResources = await page.evaluate(() =>
    [...document.querySelectorAll('.audit-resource')].every(
      (cell) => cell.textContent.trim().length > 0,
    ),
  )
  check(resolvesResources, 'every entry names the resource it acted on')
  await shot(page, 'admin-05-audit-log.png')

  await page.close()
}

await mkdir(SHOT_DIR, { recursive: true })
const browser = await puppeteer.launch({
  executablePath: CHROMIUM,
  headless: true,
  args: ['--no-sandbox', '--disable-dev-shm-usage'],
})
try {
  await adminConsoleFlow(browser)
} finally {
  await browser.close()
}

console.log(failures === 0 ? '\nAll admin console E2E checks passed.' : `\n${failures} check(s) FAILED.`)
process.exit(failures === 0 ? 0 : 1)
