import { ref } from 'vue'

export interface Crumb {
  label: string
  to?: string
}

// Keyed by path: the next view sets its crumbs before the previous one unmounts.
const extra = ref<{ path: string; crumbs: Crumb[] }>({ path: '', crumbs: [] })

export function useCrumbs() {
  return {
    extra,
    setCrumbs: (path: string, crumbs: Crumb[]) => {
      extra.value = { path, crumbs }
    },
  }
}
