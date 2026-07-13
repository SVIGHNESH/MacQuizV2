// E2E: the profile/avatar feature (docs/11's identity primitives).
//
// Flow: provision a student over the admin API, walk the student through the
// forced first-login reset into the profile page, pin the sticker picker to
// the server's preset allowlist, pick a sticker (plus "surprise me"), upload
// a photo and watch it re-encoded and served, remove it, and finally verify
// the admin users table renders the student's avatar and can moderate it
// away with the row's Remove avatar control.
//
// Run:  node web/e2e/profile.e2e.mjs   (stack + vite dev already running)
// Env:  E2E_BASE_URL (default http://localhost:5173)
//       E2E_CHROMIUM (default /usr/bin/chromium)
//       E2E_ADMIN_EMAIL / E2E_ADMIN_PASSWORD (compose bootstrap admin)

import { mkdir, writeFile } from 'node:fs/promises'
import puppeteer from 'puppeteer-core'

const BASE = process.env.E2E_BASE_URL ?? 'http://localhost:5173'
const CHROMIUM = process.env.E2E_CHROMIUM ?? '/usr/bin/chromium'
const ADMIN_EMAIL = process.env.E2E_ADMIN_EMAIL ?? 'admin@macquiz.local'
const ADMIN_PASSWORD = process.env.E2E_ADMIN_PASSWORD ?? 'admin-dev-password'
const SHOT_DIR = '/tmp/macquiz-e2e'

const studentEmail = `avatar.e2e.${process.pid}@macquiz.local`
const studentName = 'Boba Fan'
const studentFinalPassword = 'stickers-beat-suits'

// A 1x1 red PNG: the smallest decodable upload; the server scales it to 256.
const PNG_FIXTURE = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==',
  'base64',
)

let failures = 0
function check(ok, label) {
  console.log(`${ok ? 'PASS' : 'FAIL'}  ${label}`)
  if (!ok) failures++
}

async function shot(page, name) {
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

// Retries on 429: the suites share one admin account and its 5/min login
// budget (docs/08 section 4); Retry-After says how long the window needs.
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

async function provisionStudent() {
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
      role: 'student',
      email: studentEmail,
      full_name: studentName,
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

// --- the student's profile journey ------------------------------------------

async function studentFlow(browser, oneTimePassword) {
  const context = await browser.createBrowserContext()
  const page = await context.newPage()
  await page.setViewport({ width: 1360, height: 900 })
  await page.goto(BASE, { waitUntil: 'networkidle0' })

  await signIn(page, studentEmail, oneTimePassword)
  check(
    await waitForText(page, '.page-title', 'Set your password'),
    'student first sign-in lands on the forced password-reset screen',
  )
  await type(page, '#current-password', oneTimePassword)
  await type(page, '#new-password', studentFinalPassword)
  await type(page, '#confirm-password', studentFinalPassword)
  await page.click('button[type=submit]')

  await page.waitForSelector('[data-testid=rail-profile]', { timeout: 10000 })
  check(
    (await textOf(page, '[data-testid=rail-profile] .avatar')).trim() === 'BF',
    'a fresh account wears the initials chip in the rail',
  )

  await page.click('[data-testid=rail-profile]')
  await page.waitForSelector('.preset-grid', { timeout: 5000 })
  check(
    await waitForText(page, '.profile-identity', studentEmail),
    'the profile identity card shows the account email read-only',
  )
  await shot(page, 'profile-01-page.png')

  // Pin the picker to the server allowlist: every sticker the SPA offers
  // must be accepted by POST /auth/me/avatar/preset. A drifted slug 422s.
  const slugs = await page.$$eval('[data-testid^=avatar-preset-]', (tiles) =>
    tiles.map((t) => t.dataset.testid.replace('avatar-preset-', '')),
  )
  check(slugs.length === 16, `the picker offers 16 stickers (got ${slugs.length})`)
  const statuses = await page.evaluate(async (all) => {
    const out = []
    for (const slug of all) {
      const res = await fetch('/api/v1/auth/me/avatar/preset', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ preset: slug }),
      })
      out.push(`${slug}:${res.status}`)
    }
    return out
  }, slugs)
  check(
    statuses.every((s) => s.endsWith(':200')),
    `every SPA sticker slug is in the server allowlist (${statuses.filter((s) => !s.endsWith(':200')).join(', ') || 'all 200'})`,
  )

  // Picking a sticker updates the tile, the rail, and survives a reload.
  await page.click('[data-testid=avatar-preset-boba]')
  await page.waitForSelector('[data-testid=avatar-preset-boba][aria-selected=true]', { timeout: 5000 })
  check(
    (await page.$('[data-testid=rail-profile] [data-avatar-preset=boba]')) !== null,
    'the picked sticker appears in the rail immediately',
  )
  await page.reload({ waitUntil: 'networkidle0' })
  await page.waitForSelector('[data-testid=rail-profile]', { timeout: 10000 })
  check(
    (await page.$('[data-testid=rail-profile] [data-avatar-preset=boba]')) !== null,
    'the sticker survives a reload (persisted, not client state)',
  )
  await page.click('[data-testid=rail-profile]')
  await page.waitForSelector('.preset-grid', { timeout: 5000 })
  await shot(page, 'profile-02-sticker.png')

  await page.click('[data-testid=avatar-surprise]')
  await page
    .waitForFunction(
      () => document.querySelector('[data-testid=rail-profile] [data-avatar-preset]')?.dataset.avatarPreset !== 'boba',
      { timeout: 5000 },
    )
    .catch(() => {})
  const surprised = await page.$eval(
    '[data-testid=rail-profile] [data-avatar-preset]',
    (el) => el.dataset.avatarPreset,
  ).catch(() => null)
  check(
    surprised !== null && surprised !== 'boba',
    `surprise me picks a different sticker (got ${surprised})`,
  )

  // A non-image file is refused client-side before any request.
  const junkPath = `${SHOT_DIR}/avatar-junk.txt`
  await writeFile(junkPath, 'not pixels')
  const junkInput = await page.$('[data-testid=avatar-upload-input]')
  await junkInput.uploadFile(junkPath)
  check(
    await waitForText(page, '.form-error', 'PNG, JPEG, WebP, or GIF'),
    'a non-image file is rejected with the format hint',
  )

  // A real photo: preview, confirm, re-encoded and served with an ETag.
  const photoPath = `${SHOT_DIR}/avatar-src.png`
  await writeFile(photoPath, PNG_FIXTURE)
  const input = await page.$('[data-testid=avatar-upload-input]')
  await input.uploadFile(photoPath)
  await page.waitForSelector('[data-testid=avatar-upload-confirm]', { timeout: 5000 })
  await shot(page, 'profile-03-preview.png')
  await page.click('[data-testid=avatar-upload-confirm]')
  await page.waitForSelector('[data-testid=rail-profile] img.avatar-photo', { timeout: 10000 })
  check(true, 'the uploaded photo replaces the rail avatar')

  const served = await page.evaluate(async () => {
    const img = document.querySelector('[data-testid=rail-profile] img.avatar-photo')
    const res = await fetch(img.src)
    return { status: res.status, type: res.headers.get('Content-Type'), etag: res.headers.get('ETag') }
  })
  check(
    served.status === 200 && served.type === 'image/jpeg' && !!served.etag,
    `the avatar endpoint serves a JPEG with an ETag (${served.status} ${served.type})`,
  )
  await shot(page, 'profile-04-photo.png')

  await page.click('[data-testid=profile-remove-avatar]')
  await page.waitForFunction(
    () => {
      const rail = document.querySelector('[data-testid=rail-profile] .avatar')
      return rail && !rail.querySelector('img') && (rail.textContent ?? '').trim() === 'BF'
    },
    { timeout: 5000 },
  )
  check(true, 'removing the avatar reverts the rail to the initials chip')

  // Leave a sticker on for the admin leg.
  await page.click('[data-testid=avatar-preset-rocket]')
  await page.waitForSelector('[data-testid=rail-profile] [data-avatar-preset=rocket]', { timeout: 5000 })
  await context.close()
}

// --- the admin's view: propagation + moderation ------------------------------

async function adminFlow(browser) {
  const context = await browser.createBrowserContext()
  const page = await context.newPage()
  await page.setViewport({ width: 1360, height: 900 })
  await page.goto(BASE, { waitUntil: 'networkidle0' })
  await signIn(page, ADMIN_EMAIL, ADMIN_PASSWORD)
  await page.waitForSelector('[data-testid=rail-profile]', { timeout: 10000 })

  await page.evaluate(() => {
    const btn = [...document.querySelectorAll('.rail-item')].find((b) =>
      (b.textContent ?? '').includes('Users'),
    )
    btn?.click()
  })
  await page.waitForSelector('.admin-user-table', { timeout: 5000 })
  const row = await page
    .waitForFunction(
      (email) =>
        [...document.querySelectorAll('.qt-row')].find((r) =>
          (r.textContent ?? '').includes(email),
        ) ?? false,
      { timeout: 5000 },
      studentEmail,
    )
    .catch(() => null)
  check(row !== null, 'the admin users table lists the new student')

  const rowPreset = await page.evaluate((email) => {
    const r = [...document.querySelectorAll('.qt-row')].find((el) =>
      (el.textContent ?? '').includes(email),
    )
    return r?.querySelector('[data-avatar-preset]')?.dataset.avatarPreset ?? null
  }, studentEmail)
  check(rowPreset === 'rocket', `the users table renders the student's sticker (got ${rowPreset})`)
  await shot(page, 'profile-05-admin-table.png')

  await page.evaluate((email) => {
    const r = [...document.querySelectorAll('.qt-row')].find((el) =>
      (el.textContent ?? '').includes(email),
    )
    r?.querySelector('[data-testid=admin-clear-avatar]')?.click()
  }, studentEmail)
  await page.waitForFunction(
    (email) => {
      const r = [...document.querySelectorAll('.qt-row')].find((el) =>
        (el.textContent ?? '').includes(email),
      )
      return r && !r.querySelector('[data-avatar-preset]') && !r.querySelector('img.avatar-photo')
    },
    { timeout: 5000 },
    studentEmail,
  )
  check(true, 'the admin Remove avatar control reverts the row to initials')
  await shot(page, 'profile-06-admin-cleared.png')
  await context.close()
}

// --- drive ---------------------------------------------------------------------

await mkdir(SHOT_DIR, { recursive: true })
const oneTimePassword = await provisionStudent()

const browser = await puppeteer.launch({
  executablePath: CHROMIUM,
  args: ['--no-sandbox'],
})
try {
  await studentFlow(browser, oneTimePassword)
  await adminFlow(browser)
} finally {
  await browser.close()
}

console.log(failures === 0 ? 'profile e2e: all checks passed' : `profile e2e: ${failures} FAILED`)
process.exit(failures === 0 ? 0 : 1)
