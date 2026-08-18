import { useState, type ReactElement } from 'react'
import { getThemePreference, setThemePreference, type ThemePreference } from '../lib/theme'

const ORDER: ThemePreference[] = ['system', 'light', 'dark']
const LABEL: Record<ThemePreference, string> = {
  system: 'Theme: follows your system',
  light: 'Theme: light',
  dark: 'Theme: dark',
}

// Filled sun with rounded ray dashes, matching the rail's stroke weight.
const SunIcon = (
  <svg viewBox="0 0 24 24" width="16" height="16" aria-hidden="true">
    <circle cx="12" cy="12" r="4.4" fill="currentColor" />
    <g
      stroke="currentColor"
      strokeWidth="2.2"
      strokeLinecap="round"
      fill="none"
    >
      <path d="M12 2.6v2" />
      <path d="M12 19.4v2" />
      <path d="M2.6 12h2" />
      <path d="M19.4 12h2" />
      <path d="M5.2 5.2l1.4 1.4" />
      <path d="M17.4 17.4l1.4 1.4" />
      <path d="M18.8 5.2l-1.4 1.4" />
      <path d="M6.6 17.4l-1.4 1.4" />
    </g>
  </svg>
)

const MoonIcon = (
  <svg viewBox="0 0 24 24" width="16" height="16" aria-hidden="true">
    <path
      d="M20.2 14.1a8.2 8.2 0 1 1-10.3-10.3 6.8 6.8 0 1 0 10.3 10.3Z"
      fill="currentColor"
    />
  </svg>
)

// Half-filled disc: "follow the system", whichever side it lands on.
const SystemIcon = (
  <svg viewBox="0 0 24 24" width="16" height="16" aria-hidden="true">
    <circle
      cx="12"
      cy="12"
      r="8.4"
      fill="none"
      stroke="currentColor"
      strokeWidth="2.2"
    />
    <path d="M12 4.6a7.4 7.4 0 0 1 0 14.8Z" fill="currentColor" />
  </svg>
)

const ICON: Record<ThemePreference, ReactElement> = {
  system: SystemIcon,
  light: SunIcon,
  dark: MoonIcon,
}

/** Compact icon control for the sidebar rail: System -> Light -> Dark. */
export function ThemeToggle() {
  const [pref, setPref] = useState<ThemePreference>(getThemePreference)
  return (
    <button
      className="rail-theme-toggle"
      type="button"
      aria-label={`${LABEL[pref]} - switch theme`}
      title={LABEL[pref]}
      onClick={() => {
        const next = ORDER[(ORDER.indexOf(pref) + 1) % ORDER.length]
        setThemePreference(next)
        setPref(next)
      }}
    >
      {ICON[pref]}
    </button>
  )
}
