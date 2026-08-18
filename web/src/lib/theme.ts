// Theme preference: 'light' | 'dark' | 'system', persisted in localStorage.
// The resolved theme is always stamped as data-theme on <html> - the inline
// boot script in index.html does the same thing before first paint, so the
// page never flashes the wrong theme. tokens.css keys every color off the
// attribute, which is why 'system' is resolved here instead of relying on a
// prefers-color-scheme media query in CSS.

export type ThemePreference = 'light' | 'dark' | 'system'

const STORAGE_KEY = 'mq-theme'
const media = window.matchMedia('(prefers-color-scheme: dark)')

export function getThemePreference(): ThemePreference {
  const raw = localStorage.getItem(STORAGE_KEY)
  return raw === 'light' || raw === 'dark' ? raw : 'system'
}

function resolve(pref: ThemePreference): 'light' | 'dark' {
  if (pref === 'system') return media.matches ? 'dark' : 'light'
  return pref
}

function apply(pref: ThemePreference) {
  document.documentElement.dataset.theme = resolve(pref)
}

export function setThemePreference(pref: ThemePreference) {
  if (pref === 'system') localStorage.removeItem(STORAGE_KEY)
  else localStorage.setItem(STORAGE_KEY, pref)
  apply(pref)
}

// Follow OS changes live while the preference is 'system'.
media.addEventListener('change', () => {
  if (getThemePreference() === 'system') apply('system')
})
