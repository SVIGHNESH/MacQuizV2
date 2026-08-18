import { useState } from 'react'
import { getThemePreference, setThemePreference, type ThemePreference } from '../lib/theme'

const ORDER: ThemePreference[] = ['system', 'light', 'dark']
const LABEL: Record<ThemePreference, string> = {
  system: 'Theme: System',
  light: 'Theme: Light',
  dark: 'Theme: Dark',
}

/** Quiet cycling control for the sidebar rail: System -> Light -> Dark. */
export function ThemeToggle() {
  const [pref, setPref] = useState<ThemePreference>(getThemePreference)
  return (
    <button
      className="button button-quiet rail-theme-toggle"
      type="button"
      onClick={() => {
        const next = ORDER[(ORDER.indexOf(pref) + 1) % ORDER.length]
        setThemePreference(next)
        setPref(next)
      }}
    >
      {LABEL[pref]}
    </button>
  )
}
