import { computed, ref } from 'vue'

import { api } from '@/api'

export interface Org {
  id: string
  name: string
  personal: boolean
  role: string
}

export interface Me {
  user: { id: string; email: string; name: string; platformAdmin: boolean }
  orgs: Org[]
  docsURL: string
  version: string
  latestVersion: string
}

const STORAGE_KEY = 'obs-org'

function storedOrgId(): string | null {
  try {
    return localStorage.getItem(STORAGE_KEY)
  } catch {
    return null
  }
}

const me = ref<Me | null>(null)
const selectedId = ref<string | null>(storedOrgId())

const activeOrg = computed<Org | null>(() => {
  const orgs = me.value?.orgs ?? []
  return orgs.find((o) => o.id === selectedId.value) ?? orgs.find((o) => o.personal) ?? orgs[0] ?? null
})

async function load() {
  me.value = await api<Me>('/api/me')
}

function expire() {
  me.value = null
  location.assign('/login')
}

function selectOrg(id: string) {
  selectedId.value = id
  try {
    localStorage.setItem(STORAGE_KEY, id)
  } catch {
    // Storage denied: the choice still applies to this tab.
  }
}

export function useMe() {
  return { me, activeOrg, load, expire, selectOrg }
}
