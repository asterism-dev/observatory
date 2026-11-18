<script setup lang="ts">
import { Badge, Button, Dialog, RadioGroup, Table, Tabs, type TableColumn } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import ConnectIntegrationDialog from '@/components/ConnectIntegrationDialog.vue'
import { scopeLabel } from '@/composables/envScopes'
import { kindLabels, tokenPages, type Integration, type IntegrationMode, type IntegrationScope } from '@/composables/integrations'
import { useOrg } from '@/composables/useOrg'

const { t } = useI18n()
const { org } = useOrg()

const items = ref<Integration[]>([])
const scopes = ref<IntegrationScope[]>([])
const tab = ref('all')
const error = ref('')
const busy = ref(false)
const formOpen = ref(false)
const replacing = ref<Integration | null>(null)
const modeTarget = ref<Integration | null>(null)
const newMode = ref<IntegrationMode>('proxy')
const pendingDelete = ref<Integration | null>(null)

const tabs = computed(() => ['all', 'org', 'teams', 'private'].map((v) => ({ value: v, label: t(`env.tab.${v}`) })))
const columns = computed<TableColumn[]>(() => [
  { key: 'kind', label: t('integrations.kind') },
  { key: 'scope', label: t('integrations.scope') },
  { key: 'account', label: t('integrations.account') },
  { key: 'mode', label: t('integrations.delivery') },
  { key: 'status', label: t('integrations.status') },
  { key: 'actions', label: '' },
])
const rows = computed(() => {
  const scope = { all: null, org: 'org', teams: 'team', private: 'user' }[tab.value]
  return scope ? items.value.filter((v) => v.scope === scope) : items.value
})
const modeOptions = computed(() => [
  { value: 'proxy' as const, label: t('integrations.modeProxy') },
  { value: 'vm' as const, label: t('integrations.modeVm') },
])
const revokeNeeded = computed(() => !!modeTarget.value && modeTarget.value.mode === 'vm' && newMode.value === 'proxy' && modeTarget.value.wasInVm)
const teamVmWarning = computed(() => modeTarget.value?.scope === 'team' && newMode.value === 'vm')

const daysLeft = (v: Integration) => (v.providerExpiresAt ? Math.max(0, Math.ceil((Date.parse(v.providerExpiresAt) - Date.now()) / 86_400_000)) : null)
const expiringSoon = (v: Integration) => (daysLeft(v) ?? Infinity) <= 7
const needsSignIn = (v: Integration) => v.source === 'oauth'

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const res = await api<{ integrations: Integration[]; writableScopes: IntegrationScope[] }>(`/api/orgs/${id}/integrations`)
    if (id !== org.value?.id) return
    items.value = res.integrations
    scopes.value = res.writableScopes
  } catch (e) {
    if (id === org.value?.id) error.value = e instanceof Error ? e.message : String(e)
  }
}

function openConnect() {
  replacing.value = null
  formOpen.value = true
}

function openReplace(v: Integration) {
  replacing.value = v
  formOpen.value = true
}

function openMode(v: Integration) {
  modeTarget.value = v
  newMode.value = v.mode
}

async function mutate(fn: () => Promise<unknown>) {
  busy.value = true
  error.value = ''
  try {
    await fn()
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
    modeTarget.value = null
    pendingDelete.value = null
  }
  await refresh()
}

const saveMode = () =>
  modeTarget.value &&
  newMode.value !== modeTarget.value.mode &&
  mutate(() => api(`/api/orgs/${org.value?.id}/integrations/${modeTarget.value?.id}`, { method: 'PATCH', body: JSON.stringify({ mode: newMode.value }) }))

const remove = () => pendingDelete.value && mutate(() => api(`/api/orgs/${org.value?.id}/integrations/${pendingDelete.value?.id}`, { method: 'DELETE' }))

watch(
  () => org.value?.id,
  () => {
    error.value = ''
    items.value = []
    scopes.value = []
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <div class="head">
      <div>
        <h1>{{ t('nav.integrations') }}</h1>
        <p>{{ t('integrations.subtitle') }}</p>
      </div>
      <Button v-if="scopes.length" variant="accent" @click="openConnect">{{ t('integrations.connect') }}</Button>
    </div>
    <p v-if="error" role="alert">{{ error }}</p>
    <Tabs v-model="tab" :tabs="tabs" />
    <Table :columns="columns" :rows="rows" row-key="id">
      <template #cell-kind="{ row }">{{ kindLabels[row.kind as Integration['kind']] }}</template>
      <template #cell-scope="{ row }">{{ scopeLabel(row, t) }}</template>
      <template #cell-account="{ row }">{{ row.account || '—' }}</template>
      <template #cell-mode="{ row }">{{ t(`integrations.mode_${row.mode}`) }}</template>
      <template #cell-status="{ row }">
        <Badge v-if="row.status === 'expired'" tone="danger" :title="row.lastError">{{ t('integrations.expired') }}</Badge>
        <Badge v-else :tone="expiringSoon(row) ? 'warning' : 'success'">
          {{ daysLeft(row) === null ? t('integrations.ok') : t('integrations.okExpires', { n: daysLeft(row) }) }}
        </Badge>
      </template>
      <template #cell-actions="{ row }">
        <span class="cell">
          <Button v-if="row.canEdit" size="sm" @click="openReplace(row)">{{ needsSignIn(row) ? t('integrations.signInAgain') : t('integrations.replace') }}</Button>
          <Button v-if="row.canEdit" size="sm" @click="openMode(row)">{{ t('integrations.mode') }}</Button>
          <Button v-if="row.canDelete" size="sm" variant="danger" @click="pendingDelete = row">{{ t('integrations.disconnect') }}</Button>
        </span>
      </template>
      <template #empty>{{ t('integrations.empty') }}</template>
    </Table>

    <ConnectIntegrationDialog v-model:open="formOpen" :org-id="org.id" :scopes="scopes" :replacing="replacing" @saved="refresh" @conflict="refresh" />
    <Dialog :open="!!modeTarget" :title="t('integrations.modeTitle')" @update:open="(o) => !o && !busy && (modeTarget = null)">
      <RadioGroup v-model="newMode" :options="modeOptions" :aria-label="t('integrations.delivery')" />
      <p v-if="teamVmWarning" role="alert">{{ t('integrations.teamVmWarning') }}</p>
      <p v-if="revokeNeeded" role="alert">
        {{ t('integrations.revokeBody') }}
        <a v-if="modeTarget" :href="tokenPages[modeTarget.kind]" target="_blank" rel="noopener">{{ t('integrations.revokeLink', { kind: kindLabels[modeTarget.kind] }) }}</a>
      </p>
      <template #actions>
        <Button :disabled="busy" @click="modeTarget = null">{{ t('profile.cancel') }}</Button>
        <Button variant="accent" :disabled="busy || newMode === modeTarget?.mode" @click="saveMode">{{ t('env.save') }}</Button>
      </template>
    </Dialog>
    <Dialog
      :open="!!pendingDelete"
      :title="t('integrations.disconnectTitle')"
      :description="t('integrations.disconnectBody', { kind: pendingDelete ? kindLabels[pendingDelete.kind] : '' })"
      @update:open="(o) => !o && !busy && (pendingDelete = null)"
    >
      <p v-if="pendingDelete?.wasInVm" role="alert">
        {{ t('integrations.revokeBody') }}
        <a :href="tokenPages[pendingDelete.kind]" target="_blank" rel="noopener">{{ t('integrations.revokeLink', { kind: kindLabels[pendingDelete.kind] }) }}</a>
      </p>
      <template #actions>
        <Button :disabled="busy" @click="pendingDelete = null">{{ t('profile.cancel') }}</Button>
        <Button variant="danger" :disabled="busy" @click="remove">{{ t('integrations.disconnect') }}</Button>
      </template>
    </Dialog>
  </template>
</template>

<style scoped>
.head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 8px;
}
.cell {
  display: inline-flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
</style>
