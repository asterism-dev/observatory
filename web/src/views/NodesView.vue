<script setup lang="ts">
import { Badge, Button, Table, type TableColumn } from '@asterism-dev/lumen'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'

import { api } from '@/api'
import AddNodeDialog from '@/components/AddNodeDialog.vue'
import { gb, imageLine, stateTones, type NodeInfo } from '@/composables/nodes'
import { useOrg } from '@/composables/useOrg'

const { t, locale } = useI18n()
const router = useRouter()
const { org, can } = useOrg()

const nodes = ref<NodeInfo[]>([])
const error = ref('')
const addOpen = ref(false)

const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('nodes.name') },
  { key: 'state', label: t('nodes.state') },
  { key: 'version', label: t('nodes.version') },
  { key: 'image', label: t('nodes.guestImage') },
  { key: 'cpu', label: t('nodes.cpu') },
  { key: 'memory', label: t('nodes.memory') },
  { key: 'vmCount', label: t('nodes.vms') },
  { key: 'actions', label: '' },
])

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const res = await api<NodeInfo[]>(`/api/orgs/${id}/nodes`)
    // Org switched while loading.
    if (id === org.value?.id) nodes.value = res
  } catch (e) {
    if (id === org.value?.id) error.value = e instanceof Error ? e.message : String(e)
  }
}

const timer = window.setInterval(() => document.visibilityState === 'visible' && refresh(), 5000)
onBeforeUnmount(() => window.clearInterval(timer))

watch(
  () => org.value?.id,
  () => {
    error.value = ''
    nodes.value = []
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <div class="head">
      <div>
        <h1>{{ t('nav.nodes') }}</h1>
        <p>{{ t('nodes.subtitle') }}</p>
      </div>
      <Button v-if="can('node:create')" variant="accent" @click="addOpen = true">{{ t('nodes.add') }}</Button>
    </div>
    <p v-if="error" role="alert">{{ error }}</p>
    <Table :columns="columns" :rows="nodes" row-key="id">
      <template #cell-state="{ row }"><Badge :tone="stateTones[row.state as NodeInfo['state']]">{{ t(`nodes.states.${row.state}`) }}</Badge></template>
      <template #cell-version="{ row }">{{ row.version || '–' }}</template>
      <template #cell-image="{ row }">{{ imageLine(row.image, t, locale) }}</template>
      <template #cell-cpu="{ row }">{{ row.state === 'pending' ? '–' : `${row.usedCPUs} / ${row.cpus}` }}</template>
      <template #cell-memory="{ row }">{{ row.state === 'pending' ? '–' : `${gb(row.usedMemoryMB)} / ${gb(row.memoryMB)} GB` }}</template>
      <template #cell-actions="{ row }">
        <Button size="sm" @click="router.push(`/orgs/${org.id}/nodes/${row.id}`)">{{ t('nodes.open') }}</Button>
      </template>
      <template #empty>{{ t('nodes.empty') }}</template>
    </Table>
    <AddNodeDialog v-model:open="addOpen" :org-id="org.id" @created="refresh" />
  </template>
</template>

<style scoped>
.head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 8px;
}
</style>
