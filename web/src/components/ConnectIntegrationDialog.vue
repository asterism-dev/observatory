<script setup lang="ts">
import { Button, Dialog, RadioGroup, Select, TextInput } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api, ApiError } from '@/api'
import { scopeKey, scopeLabel, scopeRef } from '@/composables/envScopes'
import { defaultSource, kindLabels, type ClaudeAccount, type Integration, type IntegrationKind, type IntegrationMode, type IntegrationScope } from '@/composables/integrations'

const props = defineProps<{ open: boolean; orgId: string; scopes: IntegrationScope[]; replacing: Integration | null; addTo?: Integration | null; replaceAccount?: ClaudeAccount | null; initialKind?: IntegrationKind }>()
const emit = defineEmits<{ 'update:open': [boolean]; saved: []; conflict: [] }>()

const { t } = useI18n()
const kind = ref<IntegrationKind>('github')
const scope = ref('')
const claudeSource = ref<'oauth' | 'setup-token'>('oauth')
const mode = ref<IntegrationMode>('proxy')
const token = ref('')
const code = ref('')
const checked = ref('')
const signIn = ref<{ url: string; state: string } | null>(null)
const busy = ref(false)
const error = ref('')

const kindOptions = (Object.keys(kindLabels) as IntegrationKind[]).map((k) => ({ value: k, label: kindLabels[k] }))
const freeScopes = computed(() => props.scopes.filter((s) => !s.connected.includes(kind.value)))
const scopeOptions = computed(() => freeScopes.value.map((s) => ({ value: scopeKey(s), label: scopeLabel(s, t) })))
const target = computed(() => freeScopes.value.find((s) => scopeKey(s) === scope.value))
const signingIn = computed(() => kind.value === 'claude' && claudeSource.value === 'oauth')
const source = computed(() => (kind.value === 'claude' ? claudeSource.value : defaultSource[kind.value]))
const modeOptions = computed(() => [
  { value: 'proxy' as const, label: t('integrations.modeProxy') },
  { value: 'vm' as const, label: t('integrations.modeVm') },
])
const sourceOptions = computed(() => [
  { value: 'oauth' as const, label: t('integrations.sourceSignIn') },
  { value: 'setup-token' as const, label: t('integrations.sourceToken') },
])
const teamVmWarning = computed(() => !props.replacing && mode.value === 'vm' && target.value?.scope === 'team')
const ready = computed(() => (props.replacing || props.addTo || !!target.value) && !busy.value)

watch(
  () => props.open,
  (o) => {
    token.value = code.value = checked.value = ''
    signIn.value = null
    if (!o) return
    error.value = ''
    mode.value = 'proxy'
    claudeSource.value = (props.replaceAccount?.source ?? props.replacing?.source) === 'setup-token' ? 'setup-token' : 'oauth'
    kind.value = props.replacing?.kind ?? props.addTo?.kind ?? props.initialKind ?? 'github'
    scope.value = scopeOptions.value[0]?.value ?? ''
  },
)

watch(kind, () => {
  scope.value = scopeOptions.value[0]?.value ?? ''
})

watch([kind, scope, token, claudeSource], () => {
  checked.value = ''
  error.value = ''
})

async function call<T>(fn: () => Promise<T>): Promise<T | undefined> {
  busy.value = true
  error.value = ''
  try {
    return await fn()
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
    if (e instanceof ApiError && e.status === 409) emit('conflict')
  } finally {
    busy.value = false
  }
}

const base = () => `/api/orgs/${props.orgId}/integrations`

async function check() {
  if (!target.value) return
  const body = { kind: kind.value, source: source.value, ...scopeRef(target.value), token: token.value, dryRun: true }
  const res = await call(() => api<{ account: string }>(base(), { method: 'POST', body: JSON.stringify(body) }))
  if (res) checked.value = res.account
}

async function connect() {
  if (!target.value) return
  const body = { kind: kind.value, source: source.value, ...scopeRef(target.value), mode: mode.value, token: token.value }
  const res = await call(() => api(base(), { method: 'POST', body: JSON.stringify(body) }))
  token.value = ''
  if (res !== undefined) done()
}

async function replace() {
  if (!props.replacing) return
  const body = { token: token.value, source: source.value }
  const path = props.replaceAccount ? `${base()}/${props.replacing.id}/accounts/${props.replaceAccount.id}/token` : `${base()}/${props.replacing.id}/token`
  const res = await call(() => api(path, { method: 'PUT', body: JSON.stringify(body) }))
  token.value = ''
  if (res !== undefined) done()
}

async function addAccount() {
  if (!props.addTo) return
  const body = { source: 'setup-token', token: token.value }
  const res = await call(() => api(`${base()}/${props.addTo?.id}/accounts`, { method: 'POST', body: JSON.stringify(body) }))
  token.value = ''
  if (res !== undefined) done()
}

async function startSignIn() {
  const body = props.replaceAccount
    ? { replaceAccountId: props.replaceAccount.id }
    : props.addTo
      ? { addToId: props.addTo.id }
      : props.replacing
        ? { replaceId: props.replacing.id }
        : { ...(target.value ? scopeRef(target.value) : {}), mode: mode.value }
  const res = await call(() => api<{ url: string; state: string }>(`${base()}/claude/sign-in`, { method: 'POST', body: JSON.stringify(body) }))
  if (!res) return
  signIn.value = res
  window.open(res.url, '_blank', 'noopener,noreferrer')
}

async function finishSignIn() {
  if (!signIn.value) return
  const body = { state: signIn.value.state, code: code.value.trim() }
  const res = await call(() => api(`${base()}/claude/sign-in/finish`, { method: 'POST', body: JSON.stringify(body) }))
  code.value = ''
  if (res !== undefined) done()
  else if (error.value) signIn.value = null
}

function done() {
  emit('saved')
  emit('update:open', false)
}
</script>

<template>
  <Dialog :open="open" :title="addTo ? t('integrations.addAccountTitle') : replacing ? t('integrations.replaceTitle', { kind: kindLabels[replacing.kind] }) : t('integrations.connectTitle')" @update:open="(o) => !busy && emit('update:open', o)">
    <template v-if="!replacing && !addTo">
      <Select v-model="kind" :options="kindOptions" :aria-label="t('integrations.kind')" />
      <Select v-if="scopeOptions.length" v-model="scope" :options="scopeOptions" :aria-label="t('integrations.scope')" />
      <p v-else role="status">{{ t('integrations.noScope') }}</p>
    </template>
    <RadioGroup v-if="kind === 'claude' && !replaceAccount" v-model="claudeSource" :options="sourceOptions" :aria-label="t('integrations.source')" />
    <template v-if="signingIn">
      <template v-if="signIn">
        <p>
          {{ t('integrations.signInHint') }}
          <a :href="signIn.url" target="_blank" rel="noopener noreferrer">{{ t('integrations.signInOpen') }}</a>
        </p>
        <TextInput v-model="code" type="password" autocomplete="off" :aria-label="t('integrations.code')" :placeholder="t('integrations.code')" />
      </template>
    </template>
    <template v-else>
      <TextInput v-model="token" type="password" autocomplete="off" spellcheck="false" :aria-label="t('integrations.token')" :placeholder="t('integrations.token')" />
      <p v-if="checked" role="status">{{ t('integrations.checked', { account: checked || '–' }) }}</p>
    </template>
    <template v-if="!replacing && !addTo">
      <RadioGroup v-model="mode" :options="modeOptions" :aria-label="t('integrations.delivery')" />
      <p v-if="teamVmWarning" role="alert">{{ t('integrations.teamVmWarning') }}</p>
    </template>
    <p v-if="error" role="alert">{{ error }}</p>
    <template #actions>
      <Button :disabled="busy" @click="emit('update:open', false)">{{ t('profile.cancel') }}</Button>
      <template v-if="signingIn">
        <Button v-if="!signIn" variant="accent" :disabled="!ready" @click="startSignIn">{{ t('integrations.signIn') }}</Button>
        <Button v-else variant="accent" :disabled="busy || !code.trim()" @click="finishSignIn">{{ t('integrations.finish') }}</Button>
      </template>
      <template v-else-if="replacing">
        <Button variant="accent" :disabled="!ready || !token" @click="replace">{{ t('integrations.replace') }}</Button>
      </template>
      <template v-else-if="addTo">
        <Button variant="accent" :disabled="!ready || !token" @click="addAccount">{{ t('integrations.addAccount') }}</Button>
      </template>
      <template v-else>
        <Button :disabled="!ready || !token" @click="check">{{ t('integrations.check') }}</Button>
        <Button variant="accent" :disabled="!ready || !token || !checked" @click="connect">{{ t('integrations.connect') }}</Button>
      </template>
    </template>
  </Dialog>
</template>
