import { ref } from 'vue'

import { iamClient } from '@/composables/iamClient'
import type { Org } from '@/composables/useMe'

export interface OrgDetail extends Org {
  permissions: string[]
}

const org = ref<OrgDetail | null>(null)
let currentId = ''

async function load(id: string) {
  currentId = id
  if (org.value?.id !== id) org.value = null
  const res = await iamClient.getOrg({ org: id })
  const loaded = { ...res.org!, permissions: res.permissions }
  // A newer navigation superseded this request.
  if (id === currentId) org.value = loaded
}

const reload = () => load(currentId)
const can = (action: string) => !!org.value?.permissions.includes(action)

export function useOrg() {
  return { org, can, load, reload }
}
