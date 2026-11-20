<script setup lang="ts">
import { Button, Dialog, RadioGroup, Switch, TextInput } from '@asterism-dev/lumen'
import { ArrowLeft, Plus } from '@lucide/vue'
import { computed, h, ref, watch, watchEffect } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'

import { api } from '@/api'
import ClaudeAccounts from '@/components/ClaudeAccounts.vue'
import ConnectIntegrationDialog from '@/components/ConnectIntegrationDialog.vue'
import GcloudCommands from '@/components/GcloudCommands.vue'
import IntegrationIcon from '@/components/IntegrationIcon.vue'
import { scopeLabel } from '@/composables/envScopes'
import {
  kindLabels,
  multiAccount,
  quotaProjectPattern,
  shownAccount,
  statusLabel,
  statusTone,
  tokenPages,
  type ClaudeAccount,
  type Integration,
  type IntegrationMode,
  type IntegrationScope,
} from '@/composables/integrations'
import { useCrumbs } from '@/composables/useCrumbs'
import { useOrg } from '@/composables/useOrg'
import { Card, KeyValue, KeyValueList, Meter, PageHeader, Section, Tag } from '@/ui'

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const { org } = useOrg()
const { setCrumbs } = useCrumbs()

const id = computed(() => String(route.params.id))
const item = ref<Integration | null>(null)
const scopes = ref<IntegrationScope[]>([])
const loaded = ref(false)
const error = ref('')
const busy = ref(false)
const formOpen = ref(false)
const replacing = ref<Integration | null>(null)
const addTarget = ref<Integration | null>(null)
const replaceAccount = ref<ClaudeAccount | null>(null)
const accountRemove = ref<ClaudeAccount | null>(null)
const modeTarget = ref<Integration | null>(null)
const newMode = ref<IntegrationMode>('proxy')
const pendingDelete = ref<Integration | null>(null)

const listPath = computed(() => `/orgs/${org.value?.id}/integrations`)
const modeOptions = computed(() => [
  { value: 'proxy' as const, label: t('integrations.modeProxy') },
  { value: 'vm' as const, label: t('integrations.modeVm') },
])
const revokeNeeded = computed(() => !!modeTarget.value && modeTarget.value.mode === 'vm' && newMode.value === 'proxy' && modeTarget.value.wasInVm)
const teamVmWarning = computed(() => modeTarget.value?.scope === 'team' && newMode.value === 'vm')
const soleUsage = computed(() => (item.value?.kind === 'claude' && item.value.source === 'oauth' ? item.value.accounts?.[0]?.usage : undefined))
const headerIcon = computed(() => {
  const kind = item.value?.kind
  return kind ? () => h(IntegrationIcon, { kind }) : undefined
})
const quotaDraft = ref('')
const quotaInvalid = computed(() => !quotaProjectPattern.test(quotaDraft.value.trim()))
const quotaChanged = computed(() => quotaDraft.value.trim() !== (item.value?.quotaProject ?? ''))
const multipleAccounts = computed(() => (item.value?.accounts?.length ?? 0) > 1)

watchEffect(() => {
  if (item.value) setCrumbs(route.path, [{ label: kindLabels[item.value.kind] }])
})

async function refresh() {
  if (!org.value) return
  const orgId = org.value.id
  try {
    const res = await api<{ integrations: Integration[]; writableScopes: IntegrationScope[] }>(`/api/orgs/${orgId}/integrations`)
    if (orgId !== org.value?.id) return
    item.value = res.integrations.find((v) => v.id === id.value) ?? null
    quotaDraft.value = item.value?.quotaProject ?? ''
    scopes.value = res.writableScopes
    loaded.value = true
  } catch (e) {
    if (orgId === org.value?.id) error.value = e instanceof Error ? e.message : String(e)
  }
}

function openDialog(target: { replacing?: Integration; addTo?: Integration; account?: ClaudeAccount }) {
  replacing.value = target.replacing ?? null
  addTarget.value = target.addTo ?? null
  replaceAccount.value = target.account ?? null
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

const base = () => `/api/orgs/${org.value?.id}/integrations/${id.value}`

const saveMode = () =>
  modeTarget.value && newMode.value !== modeTarget.value.mode && mutate(() => api(base(), { method: 'PATCH', body: JSON.stringify({ mode: newMode.value }) }))

const setAutoSwitch = (on: boolean) => mutate(() => api(base(), { method: 'PATCH', body: JSON.stringify({ autoSwitch: on }) }))

const saveQuota = () => mutate(() => api(base(), { method: 'PATCH', body: JSON.stringify({ quotaProject: quotaDraft.value.trim() }) }))

const removeAccount = () => {
  const account = accountRemove.value
  if (!account) return
  return mutate(() => api(`${base()}/accounts/${account.id}`, { method: 'DELETE' }))
}

function askRemoveAccount(account: ClaudeAccount) {
  if (item.value?.accounts?.length === 1) pendingDelete.value = item.value
  else accountRemove.value = account
}

async function remove() {
  busy.value = true
  error.value = ''
  try {
    await api(base(), { method: 'DELETE' })
    router.push(listPath.value)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
    pendingDelete.value = null
  }
}

watch(
  () => [org.value?.id, id.value],
  () => {
    error.value = ''
    item.value = null
    loaded.value = false
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <div v-if="!item" class="page-body">
      <router-link :to="listPath" class="back"><ArrowLeft :size="14" aria-hidden="true" />{{ t('nav.integrations') }}</router-link>
      <p v-if="error" role="alert">{{ error }}</p>
      <p v-else-if="loaded">{{ t('integrations.notFound') }}</p>
    </div>
    <template v-else>
      <PageHeader :icon="headerIcon" :title="kindLabels[item.kind]" :subtitle="shownAccount(item) || undefined">
        <template #tags>
          <Tag>{{ scopeLabel(item, t) }}</Tag>
          <Tag>{{ t(`integrations.mode_${item.mode}`) }}</Tag>
          <Tag :tone="statusTone(item)" :title="item.status === 'expired' ? item.lastError : undefined">{{ statusLabel(item, t) }}</Tag>
        </template>
      </PageHeader>
      <div class="page-body">
        <router-link :to="listPath" class="back"><ArrowLeft :size="14" aria-hidden="true" />{{ t('nav.integrations') }}</router-link>
        <p v-if="error" role="alert">{{ error }}</p>

        <Section v-if="multiAccount(item)" :label="t('integrations.accounts')">
          <template v-if="item.canEdit" #actions>
            <Button size="sm" variant="ghost" :icon="Plus" @click="openDialog({ addTo: item })">{{ t('integrations.addAccount') }}</Button>
          </template>
          <ClaudeAccounts :org-id="org.id" :integration="item" @changed="refresh" @sign-in="openDialog({ replacing: item, account: $event })" @remove="askRemoveAccount" />
        </Section>
        <Section v-else :label="t('integrations.account')">
          <template v-if="item.canEdit" #actions>
            <Button size="sm" variant="ghost" @click="openDialog({ replacing: item })">{{ item.source === 'oauth' && item.kind !== 'gcloud' ? t('integrations.signInAgain') : t('integrations.replace') }}</Button>
          </template>
          <Card>
            <KeyValueList>
              <KeyValue :label="t('integrations.account')"><span class="mono">{{ item.account || '—' }}</span></KeyValue>
              <KeyValue :label="t('integrations.scope')">{{ scopeLabel(item, t) }}</KeyValue>
              <KeyValue :label="t('integrations.status')">{{ statusLabel(item, t) }}</KeyValue>
            </KeyValueList>
            <GcloudCommands v-if="item.kind === 'gcloud' && item.status === 'expired'" />
            <span v-if="soleUsage" class="usage">
              <Meter :label="t('integrations.fiveHour')" :percent="soleUsage.fiveHourPct" />
              <Meter :label="t('integrations.week')" :percent="soleUsage.weekPct" />
            </span>
          </Card>
        </Section>

        <Section :label="t('nav.settings')">
          <Card>
            <template v-if="item.canEdit && multiAccount(item)">
              <Switch :model-value="item.autoSwitch" :disabled="busy" @update:model-value="setAutoSwitch">{{ t('integrations.autoSwitch') }}</Switch>
              <span v-if="!item.autoSwitch" class="muted">{{ t('integrations.autoSwitchOff') }}</span>
            </template>
            <div v-if="item.kind === 'gcloud'" class="row">
              <TextInput v-model="quotaDraft" :disabled="!item.canEdit || busy" spellcheck="false" :aria-label="t('integrations.quotaProject')" :placeholder="t('integrations.quotaProject')" />
              <Button v-if="item.canEdit" size="sm" :disabled="busy || !quotaChanged || quotaInvalid" @click="saveQuota">{{ t('env.save') }}</Button>
            </div>
            <span v-if="item.kind === 'gcloud' && quotaInvalid" role="alert">{{ t('integrations.quotaInvalid') }}</span>
            <span v-if="item.kind === 'gcloud'" class="muted">{{ t('integrations.quotaHint') }}</span>
            <div class="row">
              <KeyValueList>
                <KeyValue :label="t('integrations.delivery')">{{ t(`integrations.mode_${item.mode}`) }}</KeyValue>
              </KeyValueList>
              <Button v-if="item.canEdit && item.kind !== 'gcloud'" size="sm" :disabled="multipleAccounts" :title="multipleAccounts ? t('integrations.modeMultiHint') : undefined" @click="openMode(item)">{{ t('integrations.mode') }}</Button>
            </div>
            <span v-if="item.canEdit && multipleAccounts" class="muted">{{ t('integrations.modeMultiHint') }}</span>
          </Card>
        </Section>

        <Section v-if="item.canDelete" :label="t('integrations.dangerZone')">
          <Card tone="danger">
            <span class="muted">{{ t('integrations.disconnectBody', { kind: kindLabels[item.kind] }) }}</span>
            <p><Button variant="danger" :disabled="busy" @click="pendingDelete = item">{{ t('integrations.disconnect') }}</Button></p>
          </Card>
        </Section>
      </div>
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
      :description="t('integrations.removeAccountBody', { account: accountRemove?.account || t('integrations.setupToken') })"
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
  </template>
</template>

<style scoped>
.back {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  justify-self: start;
  color: var(--muted);
  text-decoration: none;
}
.back:hover {
  color: var(--text);
}
.usage {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 12px;
}
.row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}
</style>
