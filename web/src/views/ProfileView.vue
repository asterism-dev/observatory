<script setup lang="ts">
import { Badge, Button, Dialog, Table, type TableColumn } from '@asterism-dev/lumen'
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'

import { api, ApiError } from '@/api'
import { useMe } from '@/composables/useMe'

interface Session {
  id: string
  kind: string
  userAgent: string
  ip: string
  createdAt: string
  lastUsedAt: string
  expiresAt: string
  current: boolean
}

const { t, d } = useI18n()
const { me, expire } = useMe()

const sessions = ref<Session[]>([])
const issuer = ref('')
const pending = ref<Session | null>(null)
const busy = ref(false)
const error = ref('')

const columns: TableColumn[] = [
  { key: 'kind', label: t('profile.kind') },
  { key: 'userAgent', label: t('profile.userAgent') },
  { key: 'ip', label: t('profile.ip') },
  { key: 'lastUsedAt', label: t('profile.lastUsed') },
  { key: 'expiresAt', label: t('profile.expires') },
  { key: 'actions', label: '' },
]

const fmt = (v: string) => d(new Date(v), { dateStyle: 'short', timeStyle: 'short' })

function fail(e: unknown) {
  if (e instanceof ApiError && e.status === 401) return expire()
  error.value = e instanceof Error ? e.message : String(e)
}

async function loadSessions() {
  try {
    sessions.value = await api<Session[]>('/api/me/sessions')
  } catch (e) {
    fail(e)
  }
}

async function revoke() {
  if (!pending.value) return
  const wasCurrent = pending.value.current
  busy.value = true
  try {
    await api(`/api/me/sessions/${pending.value.id}`, { method: 'DELETE' })
    pending.value = null
    if (wasCurrent) return expire()
    await loadSessions()
  } catch (e) {
    pending.value = null
    // 404: already gone, just refresh.
    if (e instanceof ApiError && e.status === 404) await loadSessions()
    else fail(e)
  } finally {
    busy.value = false
  }
}

onMounted(async () => {
  loadSessions()
  try {
    issuer.value = (await api<{ issuer: string }>('/api/auth/config')).issuer
  } catch {
    // The note is informational; the link is simply omitted.
  }
})
</script>

<template>
  <h1>{{ me?.user.name || me?.user.email }}</h1>
  <p>
    {{ me?.user.email }}
    <Badge v-if="me?.user.platformAdmin" tone="merged">{{ t('profile.admin') }}</Badge>
  </p>
  <p>
    {{ t('profile.idpNote') }}
    <a v-if="issuer" :href="issuer" target="_blank" rel="noopener">{{ t('profile.idpLink') }}</a>
  </p>

  <h2>{{ t('profile.sessions') }}</h2>
  <p v-if="error" role="alert">{{ error }}</p>
  <Table :columns="columns" :rows="sessions" row-key="id">
    <template #cell-kind="{ row }">
      <Badge>{{ row.kind }}</Badge>
      <Badge v-if="row.current" tone="success">{{ t('profile.current') }}</Badge>
    </template>
    <template #cell-lastUsedAt="{ row }">{{ fmt(row.lastUsedAt) }}</template>
    <template #cell-expiresAt="{ row }">{{ fmt(row.expiresAt) }}</template>
    <template #cell-actions="{ row }">
      <Button variant="danger" size="sm" @click="pending = row">{{ t('profile.revoke') }}</Button>
    </template>
  </Table>

  <Dialog
    :open="!!pending"
    :title="t('profile.revokeTitle')"
    :description="pending?.current ? t('profile.revokeCurrent') : t('profile.revokeBody')"
    @update:open="(o) => !o && !busy && (pending = null)"
  >
    <template #actions>
      <Button :disabled="busy" @click="pending = null">{{ t('profile.cancel') }}</Button>
      <Button variant="danger" :disabled="busy" @click="revoke">{{ t('profile.revoke') }}</Button>
    </template>
  </Dialog>
</template>
