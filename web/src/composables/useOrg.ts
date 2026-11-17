import { ref } from 'vue'

import { api } from '@/api'
import type { Org } from '@/composables/useMe'

export interface OrgDetail extends Org {
  permissions: string[]
}

const org = ref<OrgDetail | null>(null)
let currentId = ''

async function load(id: string) {
  currentId = id
  org.value = null
  const loaded = await api<OrgDetail>(`/api/orgs/${id}`)
  // A newer navigation superseded this request.
  if (id === currentId) org.value = loaded
}

const reload = () => load(currentId)
const can = (action: string) => !!org.value?.permissions.includes(action)

export function useOrg() {
  return { org, can, load, reload }
}
