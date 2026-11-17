import { ref, watch } from 'vue'

export type ThemePreference = 'system' | 'light' | 'dark'

export const THEME_PREFERENCES: ThemePreference[] = ['system', 'light', 'dark']
const STORAGE_KEY = 'obs-theme'

function stored(): ThemePreference {
  try {
    const v = localStorage.getItem(STORAGE_KEY)
    return THEME_PREFERENCES.includes(v as ThemePreference) ? (v as ThemePreference) : 'system'
  } catch {
    return 'system'
  }
}

const theme = ref<ThemePreference>(stored())
const prefersDark = window.matchMedia('(prefers-color-scheme: dark)')

function apply() {
  const dark = theme.value === 'dark' || (theme.value === 'system' && prefersDark.matches)
  document.documentElement.dataset.theme = dark ? 'dark' : 'light'
}

watch(
  theme,
  (t) => {
    apply()
    try {
      localStorage.setItem(STORAGE_KEY, t)
    } catch {
      // Storage denied: the choice still applies to this tab.
    }
  },
  { immediate: true },
)
prefersDark.addEventListener('change', apply)

export function useTheme() {
  return { theme }
}
