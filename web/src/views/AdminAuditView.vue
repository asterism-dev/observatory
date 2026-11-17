<script setup lang="ts">
import { Badge, Button, Table, TextInput, type TableColumn } from '@asterism-dev/lumen'
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'

import { api, ApiError } from '@/api'
import { useMe } from '@/composables/useMe'

interface Entry {
  id: number
  at: string
  actorKind: string
  actorId: string
  orgId: string | null
  action: string
  resource: string
  result: 'allowed' | 'denied' | 'failed'
}

const { t, d } = useI18n()
const { expire } = useMe()

const entries = ref<Entry[]>([])
const next = ref<number | null>(null)
const error = ref('')
const actor = ref('')
const action = ref('')
const from = ref('')
const to = ref('')

const columns: TableColumn[] = [
  { key: 'at', label: t('audit.time') },
  { key: 'actorId', label: t('audit.actor') },
  { key: 'orgId', label: t('audit.org') },
  { key: 'action', label: t('audit.action') },
  { key: 'resource', label: t('audit.resource') },
  { key: 'result', label: t('audit.result') },
]

const tones = { allowed: 'success', denied: 'warning', failed: 'danger' } as const

async function load(more: boolean) {
  const q = new URLSearchParams()
  if (actor.value) q.set('actor', actor.value)
  if (action.value) q.set('action', action.value)
  if (from.value) q.set('from', new Date(from.value).toISOString())
  if (to.value) q.set('to', new Date(to.value).toISOString())
  if (more && next.value !== null) q.set('before', String(next.value))
  try {
    const res = await api<{ entries: Entry[]; next: number | null }>(`/api/admin/audit?${q}`)
    entries.value = more ? [...entries.value, ...res.entries] : res.entries
    next.value = res.next
    error.value = ''
  } catch (e) {
    if (e instanceof ApiError && e.status === 401) return expire()
    error.value = e instanceof Error ? e.message : String(e)
  }
}

onMounted(() => load(false))
</script>

<template>
  <h1>{{ t('nav.audit') }}</h1>
  <form class="filters" @submit.prevent="load(false)">
    <TextInput v-model="actor" :placeholder="t('audit.actor')" :aria-label="t('audit.actor')" />
    <TextInput v-model="action" :placeholder="t('audit.action')" :aria-label="t('audit.action')" />
    <label>{{ t('audit.from') }} <input v-model="from" type="datetime-local" /></label>
    <label>{{ t('audit.to') }} <input v-model="to" type="datetime-local" /></label>
    <Button type="submit" variant="accent">{{ t('audit.filter') }}</Button>
  </form>
  <p v-if="error" role="alert">{{ error }}</p>
  <Table :columns="columns" :rows="entries" row-key="id">
    <template #cell-at="{ row }">{{ d(new Date(row.at), { dateStyle: 'short', timeStyle: 'medium' }) }}</template>
    <template #cell-actorId="{ row }">{{ row.actorKind }}:{{ row.actorId }}</template>
    <template #cell-orgId="{ row }">{{ row.orgId ?? '' }}</template>
    <template #cell-result="{ row }"><Badge :tone="tones[row.result]">{{ row.result }}</Badge></template>
    <template #empty>{{ t('audit.empty') }}</template>
  </Table>
  <Button v-if="next !== null" @click="load(true)">{{ t('audit.loadMore') }}</Button>
</template>

<style scoped>
.filters {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  margin-bottom: 12px;
}
</style>
