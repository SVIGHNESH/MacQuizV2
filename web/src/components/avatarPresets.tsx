import type { ReactNode } from 'react'

/**
 * The built-in avatar sticker set: flat "laptop sticker" marks drawn for
 * college students, every color a design token (docs/11 - components never
 * use raw hex). The slugs mirror the server's allowlist in
 * server/internal/authusers/avatar.go exactly; the e2e profile suite pins
 * the two lists together. Danger red is deliberately absent - on this
 * platform red means violation evidence, and an avatar must never read as
 * a flag on the live monitor.
 *
 * Every sticker is drawn on a 64x64 canvas in two inks: `currentColor`
 * (the preset's fg token, set by the Avatar wrapper) and
 * `var(--avatar-bg)` (the preset's bg token) for cutouts like eyes.
 */
export interface AvatarPreset {
  slug: string
  /** The display name shown in the picker - named stickers get picked. */
  name: string
  /** Background token consumed as `var(...)` by the Avatar wrapper. */
  bg: string
  /** Foreground token; becomes `currentColor` inside the art. */
  fg: string
  art: ReactNode
}

const cut = 'var(--avatar-bg)'

export const AVATAR_PRESETS: AvatarPreset[] = [
  {
    slug: 'robot',
    name: 'Beep Boop',
    bg: '--color-primary-tint',
    fg: '--color-primary-on-tint',
    art: (
      <>
        <line x1="32" y1="12" x2="32" y2="20" stroke="currentColor" strokeWidth="3" />
        <circle cx="32" cy="10" r="3.5" fill="currentColor" />
        <rect x="16" y="20" width="32" height="26" rx="7" fill="currentColor" />
        <circle cx="25" cy="31" r="3.5" fill={cut} />
        <circle cx="39" cy="31" r="3.5" fill={cut} />
        <rect x="25" y="38" width="14" height="3" rx="1.5" fill={cut} />
        <rect x="22" y="48" width="20" height="6" rx="3" fill="currentColor" />
      </>
    ),
  },
  {
    slug: 'alien',
    name: 'Transfer Student',
    bg: '--color-success-tint',
    fg: '--color-success-on-tint',
    art: (
      <>
        <path d="M32 12c11 0 18 8 18 17 0 11-10 23-18 23S14 40 14 29c0-9 7-17 18-17z" fill="currentColor" />
        <ellipse cx="25" cy="30" rx="4.5" ry="6" transform="rotate(20 25 30)" fill={cut} />
        <ellipse cx="39" cy="30" rx="4.5" ry="6" transform="rotate(-20 39 30)" fill={cut} />
        <rect x="30" y="43" width="4" height="2.5" rx="1.25" fill={cut} />
      </>
    ),
  },
  {
    slug: 'ghost',
    name: 'Deadline Ghost',
    bg: '--color-well',
    fg: '--color-text-strong',
    art: (
      <>
        <path
          d="M32 12c-10 0-16 8-16 17v20l5-4 5.5 5 5.5-5 5.5 5 5.5-5 5 4V29c0-9-6-17-16-17z"
          fill="currentColor"
        />
        <circle cx="26" cy="29" r="3" fill={cut} />
        <circle cx="38" cy="29" r="3" fill={cut} />
        <ellipse cx="32" cy="37" rx="3" ry="4" fill={cut} />
      </>
    ),
  },
  {
    slug: 'cool-cat',
    name: 'Cool Cat',
    bg: '--color-chart-tint-2',
    fg: '--color-primary-on-tint',
    art: (
      <>
        <path d="M18 26l-3-12 10 6z" fill="currentColor" />
        <path d="M46 26l3-12-10 6z" fill="currentColor" />
        <circle cx="32" cy="34" r="17" fill="currentColor" />
        <rect x="18" y="27" width="12" height="9" rx="4" fill={cut} />
        <rect x="34" y="27" width="12" height="9" rx="4" fill={cut} />
        <rect x="29" y="29" width="6" height="3" fill="currentColor" />
        <path d="M28 43c1.5 1.5 6.5 1.5 8 0" stroke={cut} strokeWidth="2.5" fill="none" strokeLinecap="round" />
      </>
    ),
  },
  {
    slug: 'skull-jam',
    name: 'Lo-Fi Skull',
    bg: '--color-well',
    fg: '--color-text-strong',
    art: (
      <>
        <path d="M32 14c-9.5 0-16 7-16 15 0 5 2.5 9 6 11v6h20v-6c3.5-2 6-6 6-11 0-8-6.5-15-16-15z" fill="currentColor" />
        <circle cx="25.5" cy="30" r="4" fill={cut} />
        <circle cx="38.5" cy="30" r="4" fill={cut} />
        <rect x="30.5" y="38" width="3" height="5" rx="1.5" fill={cut} />
        <path d="M12 30c0-12 9-20 20-20s20 8 20 20" stroke="currentColor" strokeWidth="3" fill="none" />
        <rect x="9" y="28" width="7" height="12" rx="3.5" fill="currentColor" />
        <rect x="48" y="28" width="7" height="12" rx="3.5" fill="currentColor" />
      </>
    ),
  },
  {
    slug: 'dino',
    name: 'Study Rex',
    bg: '--color-success-tint',
    fg: '--color-success-on-tint',
    art: (
      <>
        <path d="M24 18l4-6 3 5 3-5 3 5 3-5 4 6z" fill="currentColor" />
        <rect x="20" y="18" width="26" height="24" rx="10" fill="currentColor" />
        <circle cx="29" cy="28" r="3" fill={cut} />
        <circle cx="41" cy="28" r="3" fill={cut} />
        <path d="M27 36h14" stroke={cut} strokeWidth="2.5" strokeLinecap="round" />
        <rect x="24" y="42" width="6" height="8" rx="3" fill="currentColor" />
        <rect x="36" y="42" width="6" height="8" rx="3" fill="currentColor" />
      </>
    ),
  },
  {
    slug: 'astro-duck',
    name: 'Astro Duck',
    bg: '--color-chart-tint-1',
    fg: '--color-primary-on-tint',
    art: (
      <>
        <circle cx="32" cy="30" r="18" stroke="currentColor" strokeWidth="3" fill="none" />
        <circle cx="32" cy="31" r="12" fill="currentColor" />
        <circle cx="28" cy="28" r="2.5" fill={cut} />
        <circle cx="37" cy="28" r="2.5" fill={cut} />
        <ellipse cx="32.5" cy="35" rx="5" ry="3" fill={cut} />
        <path d="M20 50h24" stroke="currentColor" strokeWidth="4" strokeLinecap="round" />
      </>
    ),
  },
  {
    slug: 'wizard',
    name: 'Syntax Wizard',
    bg: '--color-primary-tint',
    fg: '--color-primary-on-tint',
    art: (
      <>
        <path d="M32 8l7 20 12 2-38 0 12-2z" fill="currentColor" />
        <path d="M13 32h38l-4 5H17z" fill="currentColor" />
        <circle cx="32" cy="45" r="9" fill="currentColor" />
        <circle cx="29" cy="44" r="2" fill={cut} />
        <circle cx="36" cy="44" r="2" fill={cut} />
        <path d="M45 14l1.2 3 3 1.2-3 1.2-1.2 3-1.2-3-3-1.2 3-1.2z" fill="currentColor" />
      </>
    ),
  },
  {
    slug: 'coffee',
    name: 'Fifth Coffee',
    bg: '--color-warning-tint',
    fg: '--color-warning-on-tint',
    art: (
      <>
        <path d="M26 10c-2 3 2 4 0 7M34 10c-2 3 2 4 0 7" stroke="currentColor" strokeWidth="2.5" fill="none" strokeLinecap="round" />
        <path d="M16 22h30v14a12 12 0 0 1-12 12h-6a12 12 0 0 1-12-12z" fill="currentColor" />
        <path d="M46 26h4a5 5 0 0 1 0 10h-5" stroke="currentColor" strokeWidth="3" fill="none" />
        <path d="M20 52h22" stroke="currentColor" strokeWidth="3" strokeLinecap="round" />
      </>
    ),
  },
  {
    slug: 'noodles',
    name: 'Instant Noodles',
    bg: '--color-warning-tint',
    fg: '--color-warning-on-tint',
    art: (
      <>
        <path d="M40 8L22 26M48 12L30 27" stroke="currentColor" strokeWidth="2.5" strokeLinecap="round" />
        <path d="M14 28h36l-4 20a6 6 0 0 1-6 5H24a6 6 0 0 1-6-5z" fill="currentColor" />
        <path d="M20 34c4 3 8-3 12 0s8-3 12 0" stroke={cut} strokeWidth="2.5" fill="none" strokeLinecap="round" />
      </>
    ),
  },
  {
    slug: 'boba',
    name: 'Boba Break',
    bg: '--color-chart-tint-2',
    fg: '--color-primary-on-tint',
    art: (
      <>
        <path d="M36 8l-8 14" stroke="currentColor" strokeWidth="3.5" strokeLinecap="round" />
        <path d="M20 22h24l-3 28a4 4 0 0 1-4 4H27a4 4 0 0 1-4-4z" fill="currentColor" />
        <circle cx="27" cy="46" r="2.5" fill={cut} />
        <circle cx="33" cy="49" r="2.5" fill={cut} />
        <circle cx="38" cy="45" r="2.5" fill={cut} />
        <path d="M21 30h22" stroke={cut} strokeWidth="2" />
      </>
    ),
  },
  {
    slug: 'controller',
    name: 'One More Game',
    bg: '--color-primary-tint',
    fg: '--color-primary-on-tint',
    art: (
      <>
        <path d="M18 22h28a10 10 0 0 1 10 11l-1.5 8a6 6 0 0 1-10.5 3l-3-4H23l-3 4a6 6 0 0 1-10.5-3L8 33a10 10 0 0 1 10-11z" fill="currentColor" />
        <rect x="18" y="29" width="10" height="3.5" rx="1.75" fill={cut} />
        <rect x="21.25" y="25.75" width="3.5" height="10" rx="1.75" fill={cut} />
        <circle cx="42" cy="28" r="2.5" fill={cut} />
        <circle cx="47" cy="33" r="2.5" fill={cut} />
      </>
    ),
  },
  {
    slug: 'pizza',
    name: 'Cold Pizza',
    bg: '--color-warning-tint',
    fg: '--color-warning-on-tint',
    art: (
      <>
        <path d="M32 54L12 14a44 44 0 0 1 40 0z" fill="currentColor" />
        <path d="M15.5 17.5a40 40 0 0 1 33 0" stroke={cut} strokeWidth="3" fill="none" />
        <circle cx="30" cy="27" r="3" fill={cut} />
        <circle cx="38" cy="34" r="3" fill={cut} />
        <circle cx="29" cy="41" r="3" fill={cut} />
      </>
    ),
  },
  {
    slug: 'cassette',
    name: 'Mixtape',
    bg: '--color-well',
    fg: '--color-text-strong',
    art: (
      <>
        <rect x="10" y="18" width="44" height="28" rx="4" fill="currentColor" />
        <rect x="16" y="24" width="32" height="10" rx="5" fill={cut} />
        <circle cx="23" cy="29" r="3" fill="currentColor" />
        <circle cx="41" cy="29" r="3" fill="currentColor" />
        <path d="M20 46l3-7h18l3 7z" fill={cut} />
      </>
    ),
  },
  {
    slug: 'bolt',
    name: 'Full Charge',
    bg: '--color-warning-tint',
    fg: '--color-warning-on-tint',
    art: <path d="M36 8L16 36h11l-3 20 20-28H33z" fill="currentColor" />,
  },
  {
    slug: 'rocket',
    name: 'Ship It',
    bg: '--color-chart-tint-1',
    fg: '--color-primary-on-tint',
    art: (
      <>
        <path d="M32 6c7 5 10 13 10 21 0 6-2 12-4 15H26c-2-3-4-9-4-15 0-8 3-16 10-21z" fill="currentColor" />
        <circle cx="32" cy="24" r="4.5" fill={cut} />
        <path d="M22 32l-7 10 8-2zM42 32l7 10-8-2z" fill="currentColor" />
        <path d="M32 46v10" stroke="currentColor" strokeWidth="4" strokeLinecap="round" />
      </>
    ),
  },
]

/** Preset lookup by slug; unknown slugs (a newer server) fall back to initials. */
export function presetBySlug(slug: string): AvatarPreset | undefined {
  return AVATAR_PRESETS.find((p) => p.slug === slug)
}
