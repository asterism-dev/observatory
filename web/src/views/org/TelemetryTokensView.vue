<script setup lang="ts">
import { Button, Dialog, TextInput, type TableColumn } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import { useOrg } from '@/composables/useOrg'
import { CopyButton, DataTable, Section, ValueField } from '@/ui'

interface Token {
  id: string
  name: string
  createdBy: string
  createdAt: string
  lastUsedAt: string | null
}

const { t, d } = useI18n()
const { org } = useOrg()

const tokens = ref<Token[]>([])
const creators = ref<Record<string, string>>({})
const creating = ref(false)
const name = ref('')
const created = ref<(Token & { token: string }) | null>(null)
const pending = ref<Token | null>(null)
const busy = ref(false)
const error = ref('')
const dialogError = ref('')

const publicUrl = window.location.origin
const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('grafana.name') },
  { key: 'createdBy', label: t('grafana.createdBy') },
  { key: 'createdAt', label: t('grafana.created') },
  { key: 'lastUsedAt', label: t('grafana.lastUsed') },
  { key: 'actions', label: '' },
])
const fmt = (v: string) => d(new Date(v), { dateStyle: 'short', timeStyle: 'short' })
const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const [ts, ms] = await Promise.all([
      api<Token[]>(`/api/orgs/${id}/telemetry-tokens`),
      api<{ id: string; name: string; email: string }[]>(`/api/orgs/${id}/members`).catch(() => []),
    ])
    if (id !== org.value?.id) return
    tokens.value = ts
    creators.value = Object.fromEntries(ms.map((m) => [m.id, m.name || m.email]))
  } catch (e) {
    if (id === org.value?.id) error.value = message(e)
  }
}

async function create() {
  busy.value = true
  dialogError.value = ''
  try {
    created.value = await api(`/api/orgs/${org.value?.id}/telemetry-tokens`, { method: 'POST', body: JSON.stringify({ name: name.value }) })
    await refresh()
  } catch (e) {
    dialogError.value = message(e)
  } finally {
    busy.value = false
  }
}

async function revoke() {
  const tok = pending.value
  if (!tok) return
  busy.value = true
  error.value = ''
  try {
    await api(`/api/orgs/${org.value?.id}/telemetry-tokens/${tok.id}`, { method: 'DELETE' })
    pending.value = null
    await refresh()
  } catch (e) {
    error.value = message(e)
    pending.value = null
  } finally {
    busy.value = false
  }
}

function openCreate() {
  name.value = dialogError.value = ''
  created.value = null
  creating.value = true
}

watch(
  () => org.value?.id,
  () => {
    error.value = ''
    tokens.value = []
    creating.value = false
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <Section :label="t('grafana.title')" :description="t('grafana.hint')">
      <p v-if="error" role="alert">{{ error }}</p>
      <div><Button variant="accent" @click="openCreate">{{ t('grafana.create') }}</Button></div>
      <DataTable :columns="columns" :rows="tokens" row-key="id">
        <template #cell-name="{ row }"><span class="mono">{{ row.name }}</span></template>
        <template #cell-createdBy="{ row }">{{ creators[row.createdBy] ?? row.createdBy }}</template>
        <template #cell-createdAt="{ row }">{{ fmt(row.createdAt) }}</template>
        <template #cell-lastUsedAt="{ row }">{{ row.lastUsedAt ? fmt(row.lastUsedAt) : t('grafana.never') }}</template>
        <template #cell-actions="{ row }">
          <Button variant="danger" size="sm" @click="pending = row">{{ t('profile.revoke') }}</Button>
        </template>
        <template #empty><span class="muted">{{ t('grafana.empty') }}</span></template>
      </DataTable>
    </Section>

    <Dialog :open="creating" :title="t('grafana.createTitle')" @update:open="(o) => !o && !busy && (creating = false)">
      <div v-if="created" class="fields">
        <p class="muted">{{ t('grafana.tokenOnce') }}</p>
        <ValueField :label="t('grafana.token')" :value="created.token" mono copy />
        <ValueField :label="t('grafana.prometheusUrl')" :value="`${publicUrl}/grafana/prometheus`" mono copy />
        <ValueField :label="t('grafana.logsUrl')" :value="`${publicUrl}/grafana/logs`" mono copy />
        <ValueField :label="t('grafana.header')" mono>
          <span>Authorization: Bearer {{ created.token }}</span>
          <CopyButton :text="`Bearer ${created.token}`" />
        </ValueField>
      </div>
      <div v-else class="fields">
        <TextInput v-model="name" :aria-label="t('grafana.tokenName')" :placeholder="t('grafana.tokenName')" @keydown.enter="name.trim() && !busy && create()" />
        <p v-if="dialogError" role="alert">{{ dialogError }}</p>
      </div>
      <template #actions>
        <template v-if="created">
          <Button variant="accent" @click="creating = false">{{ t('grafana.done') }}</Button>
        </template>
        <template v-else>
          <Button :disabled="busy" @click="creating = false">{{ t('profile.cancel') }}</Button>
          <Button variant="accent" :disabled="busy || !name.trim()" @click="create">{{ t('grafana.create') }}</Button>
        </template>
      </template>
    </Dialog>

    <Dialog
      :open="!!pending"
      :title="t('grafana.revokeTitle')"
      :description="t('grafana.revokeBody', { name: pending?.name })"
      @update:open="(o) => !o && !busy && (pending = null)"
    >
      <template #actions>
        <Button :disabled="busy" @click="pending = null">{{ t('profile.cancel') }}</Button>
        <Button variant="danger" :disabled="busy" @click="revoke">{{ t('profile.revoke') }}</Button>
      </template>
    </Dialog>
  </template>
</template>

<style scoped>
.fields {
  display: grid;
  gap: 12px;
}
</style>
