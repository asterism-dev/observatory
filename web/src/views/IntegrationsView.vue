<script setup lang="ts">
import { Button, Dialog, RadioGroup, Switch, Tabs } from '@asterism-dev/lumen'
import { Plug, Plus } from '@lucide/vue'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import ClaudeAccounts from '@/components/ClaudeAccounts.vue'
import ConnectIntegrationDialog from '@/components/ConnectIntegrationDialog.vue'
import { scopeLabel } from '@/composables/envScopes'
import { kindLabels, tokenPages, type ClaudeAccount, type Integration, type IntegrationMode, type IntegrationScope } from '@/composables/integrations'
import { useOrg } from '@/composables/useOrg'
import { EmptyState, FilterBar, Meter, Page, ResourceList, ResourceRow, Tag } from '@/ui'

const { t } = useI18n()
const { org } = useOrg()

const items = ref<Integration[]>([])
const scopes = ref<IntegrationScope[]>([])
const tab = ref('all')
const error = ref('')
const busy = ref(false)
const formOpen = ref(false)
const replacing = ref<Integration | null>(null)
const addTarget = ref<Integration | null>(null)
const replaceAccount = ref<ClaudeAccount | null>(null)
const accountRemove = ref<{ integration: Integration; account: ClaudeAccount } | null>(null)
const modeTarget = ref<Integration | null>(null)
const newMode = ref<IntegrationMode>('proxy')
const pendingDelete = ref<Integration | null>(null)
const loaded = ref(false)

const tabs = computed(() => ['all', 'org', 'teams', 'private'].map((v) => ({ value: v, label: t(`env.tab.${v}`) })))
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
const multiAccount = (v: Integration) => v.kind === 'claude' && v.scope === 'user' && v.mode === 'proxy' && !!v.accounts?.length
const soleUsage = (v: Integration) => (v.kind === 'claude' && v.source === 'oauth' && !multiAccount(v) ? v.accounts?.[0]?.usage : undefined)
const needsSignIn = (v: Integration) => v.source === 'oauth'
const statusTone = (v: Integration) => (v.status === 'expired' ? 'danger' : expiringSoon(v) ? 'warning' : 'success')

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const res = await api<{ integrations: Integration[]; writableScopes: IntegrationScope[] }>(`/api/orgs/${id}/integrations`)
    if (id !== org.value?.id) return
    items.value = res.integrations
    scopes.value = res.writableScopes
    loaded.value = true
  } catch (e) {
    if (id === org.value?.id) error.value = e instanceof Error ? e.message : String(e)
  }
}

function openConnect() {
  replacing.value = null
  addTarget.value = null
  replaceAccount.value = null
  formOpen.value = true
}

function openAddAccount(v: Integration) {
  replacing.value = null
  replaceAccount.value = null
  addTarget.value = v
  formOpen.value = true
}

function openReplaceAccount(v: Integration, a: ClaudeAccount) {
  addTarget.value = null
  replacing.value = v
  replaceAccount.value = a
  formOpen.value = true
}

function openReplace(v: Integration) {
  addTarget.value = null
  replaceAccount.value = null
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
    accountRemove.value = null
  }
  await refresh()
}

const saveMode = () =>
  modeTarget.value &&
  newMode.value !== modeTarget.value.mode &&
  mutate(() => api(`/api/orgs/${org.value?.id}/integrations/${modeTarget.value?.id}`, { method: 'PATCH', body: JSON.stringify({ mode: newMode.value }) }))

const setAutoSwitch = (v: Integration, on: boolean) =>
  mutate(() => api(`/api/orgs/${org.value?.id}/integrations/${v.id}`, { method: 'PATCH', body: JSON.stringify({ autoSwitch: on }) }))

const removeAccount = () => {
  const target = accountRemove.value
  if (!target) return
  return mutate(() => api(`/api/orgs/${org.value?.id}/integrations/${target.integration.id}/accounts/${target.account.id}`, { method: 'DELETE' }))
}

function askRemoveAccount(integration: Integration, account: ClaudeAccount) {
  if (integration.accounts?.length === 1) pendingDelete.value = integration
  else accountRemove.value = { integration, account }
}

const remove = () => pendingDelete.value && mutate(() => api(`/api/orgs/${org.value?.id}/integrations/${pendingDelete.value?.id}`, { method: 'DELETE' }))

watch(
  () => org.value?.id,
  () => {
    error.value = ''
    items.value = []
    scopes.value = []
    loaded.value = false
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <Page v-if="org" :icon="Plug" :title="t('nav.integrations')" :subtitle="t('integrations.subtitle')">
    <template #actions>
      <Button v-if="scopes.length" variant="accent" :icon="Plus" @click="openConnect">{{ t('integrations.connect') }}</Button>
    </template>
    <p v-if="error" role="alert">{{ error }}</p>
    <EmptyState v-if="loaded && !items.length" :label="t('nav.integrations')" :title="t('integrations.empty')" :text="t('integrations.emptyText')" command="observatory integration connect github">
      <Button v-if="scopes.length" variant="accent" :icon="Plus" @click="openConnect">{{ t('integrations.connect') }}</Button>
    </EmptyState>
    <template v-else>
      <FilterBar>
        <Tabs v-model="tab" :tabs="tabs" variant="underline" />
      </FilterBar>
      <ResourceList :count="rows.length" :empty="t('integrations.empty')">
        <template v-for="v in rows" :key="v.id">
          <ResourceRow :title="kindLabels[v.kind]" :meta="v.account || '—'" :tone="statusTone(v)">
            <template #tags>
              <Tag>{{ scopeLabel(v, t) }}</Tag>
              <Tag>{{ t(`integrations.mode_${v.mode}`) }}</Tag>
              <Tag :tone="statusTone(v)" :title="v.status === 'expired' ? v.lastError : undefined">
                {{ v.status === 'expired' ? t('integrations.expired') : daysLeft(v) === null ? t('integrations.ok') : t('integrations.okExpires', { n: daysLeft(v) }) }}
              </Tag>
            </template>
            <span v-if="soleUsage(v)" class="usage">
              <Meter :label="t('integrations.fiveHour')" :percent="soleUsage(v)?.fiveHourPct ?? null" />
              <Meter :label="t('integrations.week')" :percent="soleUsage(v)?.weekPct ?? null" />
            </span>
            <template #actions>
              <Switch v-if="v.canEdit && multiAccount(v)" :model-value="v.autoSwitch" :disabled="busy" @update:model-value="(on) => setAutoSwitch(v, on)">{{ t('integrations.autoSwitch') }}</Switch>
              <Button v-if="v.canEdit && multiAccount(v)" size="sm" variant="ghost" :icon="Plus" @click="openAddAccount(v)">{{ t('integrations.addAccount') }}</Button>
              <Button v-if="v.canEdit && !multiAccount(v)" size="sm" variant="ghost" @click="openReplace(v)">{{ needsSignIn(v) ? t('integrations.signInAgain') : t('integrations.replace') }}</Button>
              <Button v-if="v.canEdit" size="sm" variant="ghost" :disabled="(v.accounts?.length ?? 0) > 1" :title="(v.accounts?.length ?? 0) > 1 ? t('integrations.modeMultiHint') : undefined" @click="openMode(v)">{{ t('integrations.mode') }}</Button>
              <Button v-if="v.canDelete" size="sm" variant="ghost" @click="pendingDelete = v">{{ t('integrations.disconnect') }}</Button>
            </template>
          </ResourceRow>
          <li v-if="multiAccount(v)">
            <ClaudeAccounts :org-id="org.id" :integration="v" @changed="refresh" @sign-in="openReplaceAccount(v, $event)" @remove="askRemoveAccount(v, $event)" />
          </li>
        </template>
      </ResourceList>
    </template>

    <ConnectIntegrationDialog v-model:open="formOpen" :org-id="org.id" :scopes="scopes" :replacing="replacing" :add-to="addTarget" :replace-account="replaceAccount" @saved="refresh" @conflict="refresh" />
    <Dialog :open="!!modeTarget" :title="t('integrations.modeTitle')" @update:open="(o) => !o && !busy && (modeTarget = null)">
      <RadioGroup v-model="newMode" :options="modeOptions" :aria-label="t('integrations.delivery')" />
      <p v-if="teamVmWarning" role="alert">{{ t('integrations.teamVmWarning') }}</p>
      <p v-if="revokeNeeded" role="alert">
        {{ t('integrations.revokeBody') }}
        <a v-if="modeTarget" :href="tokenPages[modeTarget.kind]" target="_blank" rel="noopener noreferrer">{{ t('integrations.revokeLink', { kind: kindLabels[modeTarget.kind] }) }}</a>
      </p>
      <template #actions>
        <Button :disabled="busy" @click="modeTarget = null">{{ t('profile.cancel') }}</Button>
        <Button variant="accent" :disabled="busy || newMode === modeTarget?.mode" @click="saveMode">{{ t('env.save') }}</Button>
      </template>
    </Dialog>
    <Dialog
      :open="!!accountRemove"
      :title="t('integrations.removeAccountTitle')"
      :description="t('integrations.removeAccountBody', { account: accountRemove?.account.account || t('integrations.setupToken') })"
      @update:open="(o) => !o && !busy && (accountRemove = null)"
    >
      <template #actions>
        <Button :disabled="busy" @click="accountRemove = null">{{ t('profile.cancel') }}</Button>
        <Button variant="danger" :disabled="busy" @click="removeAccount">{{ t('integrations.remove') }}</Button>
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
        <a :href="tokenPages[pendingDelete.kind]" target="_blank" rel="noopener noreferrer">{{ t('integrations.revokeLink', { kind: kindLabels[pendingDelete.kind] }) }}</a>
      </p>
      <template #actions>
        <Button :disabled="busy" @click="pendingDelete = null">{{ t('profile.cancel') }}</Button>
        <Button variant="danger" :disabled="busy" @click="remove">{{ t('integrations.disconnect') }}</Button>
      </template>
    </Dialog>
  </Page>
</template>

<style scoped>
.usage {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 12px;
}
</style>

