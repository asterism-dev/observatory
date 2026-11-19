<script setup lang="ts">
import { Button, SegmentedControl, Select, Tabs, TextInput } from '@asterism-dev/lumen'
import { Plus, Server } from '@lucide/vue'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import CreateVMDialog from '@/components/CreateVMDialog.vue'
import PageHead from '@/components/PageHead.vue'
import VMList from '@/components/VMList.vue'
import type { VmInfo } from '@/composables/nodes'
import { useMe } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'

interface Listing {
  vms: VmInfo[]
  creatableOwners: { kind: 'user' | 'team'; id: string; name: string }[]
  nodes: { id: string; name: string; online: boolean }[]
}

const { t } = useI18n()
const { org } = useOrg()
const { me } = useMe()

const data = ref<Listing>({ vms: [], creatableOwners: [], nodes: [] })
const error = ref('')
const createOpen = ref(false)
const tab = ref('all')
const status = ref('all')
const node = ref('all')
const search = ref('')

const tabs = computed(() => ['all', 'mine', 'teams'].map((v) => ({ value: v, label: t(`vms.tab.${v}`) })))
const statuses = computed(() => ['all', 'running', 'paused', 'hibernated', 'stopped'].map((v) => ({ value: v, label: v === 'all' ? t('vms.tab.all') : t(`vms.states.${v}`) })))
const nodeOptions = computed(() => [
  { value: 'all', label: t('vms.allNodes') },
  ...[...new Map(data.value.vms.map((v) => [v.node.id, v.node.name])).entries()].map(([value, label]) => ({ value, label })),
])
const isMine = (v: VmInfo) => v.owner.kind === 'user' && v.owner.id === me.value?.user.id
const rows = computed(() => {
  const q = search.value.trim().toLowerCase()
  return data.value.vms.filter(
    (v) =>
      (tab.value === 'mine' ? isMine(v) : tab.value === 'teams' ? v.owner.kind === 'team' : true) &&
      (status.value === 'all' || v.state === status.value) &&
      (node.value === 'all' || v.node.id === node.value) &&
      (!q || v.name.includes(q)),
  )
})
const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const res = await api<Listing>(`/api/orgs/${id}/vms`)
    if (id === org.value?.id) data.value = res
  } catch (e) {
    if (id === org.value?.id) error.value = message(e)
  }
}

const timer = window.setInterval(() => document.visibilityState === 'visible' && refresh(), 5000)
onBeforeUnmount(() => window.clearInterval(timer))

watch(
  () => org.value?.id,
  () => {
    error.value = ''
    node.value = 'all'
    data.value = { vms: [], creatableOwners: [], nodes: [] }
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <PageHead :icon="Server" :title="t('nav.vms')" :subtitle="t('vms.subtitle')">
      <Button v-if="data.creatableOwners.length" variant="accent" :icon="Plus" @click="createOpen = true">{{ t('vms.create') }}</Button>
    </PageHead>
    <p v-if="error" role="alert">{{ error }}</p>
    <div class="filters">
      <Tabs v-model="tab" :tabs="tabs" variant="underline" />
      <SegmentedControl v-model="status" :options="statuses" size="sm" />
      <span class="spacer" />
      <Select v-model="node" :options="nodeOptions" :aria-label="t('vms.node')" />
      <TextInput v-model="search" type="search" :placeholder="t('vms.search')" :aria-label="t('vms.search')" />
    </div>
    <VMList :org-id="org.id" :vms="rows" :empty="data.vms.length ? t('vms.noMatch') : t('vms.empty')" @changed="refresh" />
    <CreateVMDialog v-model:open="createOpen" :org-id="org.id" :owners="data.creatableOwners" :nodes="data.nodes" @created="refresh" />
  </template>
</template>

<style scoped>
.filters {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 12px;
  margin-bottom: 16px;
}
.spacer {
  flex: 1;
}
</style>
