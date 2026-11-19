<script setup lang="ts">
import type { TableColumn } from '@asterism-dev/lumen'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import { vmTones, type VmInfo } from '@/composables/nodes'
import { useOrg } from '@/composables/useOrg'
import { DataTable, Section, Tag } from '@/ui'

const { t, d } = useI18n()
const { org } = useOrg()

const vms = ref<VmInfo[]>([])
const error = ref('')

const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('vms.name') },
  { key: 'state', label: t('images.state') },
  { key: 'owner', label: t('vms.owner') },
  { key: 'node', label: t('vms.node') },
  { key: 'createdAt', label: t('vms.created') },
])

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const res = await api<{ vms: VmInfo[] }>(`/api/orgs/${id}/builder-vms`)
    if (id === org.value?.id) vms.value = res.vms
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
    vms.value = []
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <Section :label="t('nav.builders')" :description="t('images.buildersHint')">
    <p v-if="error" role="alert">{{ error }}</p>
    <DataTable :columns="columns" :rows="vms" row-key="id">
      <template #cell-name="{ row }"><span class="mono">{{ row.name }}</span></template>
      <template #cell-state="{ row }"><Tag :tone="vmTones[row.state as VmInfo['state']]">{{ t(`vms.states.${row.state}`) }}</Tag></template>
      <template #cell-owner="{ row }">{{ row.owner.name }}</template>
      <template #cell-node="{ row }">{{ row.node.name }}</template>
      <template #cell-createdAt="{ row }">{{ d(new Date(row.createdAt), { dateStyle: 'short', timeStyle: 'short' }) }}</template>
      <template #empty><span class="muted">{{ t('images.noBuilders') }}</span></template>
    </DataTable>
  </Section>
</template>
