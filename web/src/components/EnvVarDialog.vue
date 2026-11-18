<script setup lang="ts">
import { Button, Checkbox, Dialog, Select, Textarea, TextInput } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api, ApiError } from '@/api'
import { invalidHost, parseHosts, scopeKey, scopeLabel, scopeRef, type EnvVar, type WritableScope } from '@/composables/envScopes'

const props = defineProps<{ open: boolean; orgId: string; scopes: WritableScope[]; editing: EnvVar | null }>()
const emit = defineEmits<{ 'update:open': [boolean]; saved: []; conflict: [] }>()

const { t } = useI18n()
const name = ref('')
const value = ref('')
const secret = ref(false)
const scope = ref('')
const busy = ref(false)
const error = ref('')
const hostsText = ref('')
const confirmUnbind = ref(false)

const options = computed(() => props.scopes.map((s) => ({ value: scopeKey(s), label: scopeLabel(s, t) })))
const lockedSecret = computed(() => props.editing?.kind === 'secret')
const hosts = computed(() => parseHosts(hostsText.value))
const wasBound = computed(() => (props.editing?.hosts.length ?? 0) > 0)
const hostsChanged = computed(() => !props.editing || hosts.value.join('\n') !== props.editing.hosts.join('\n'))
const badHost = computed(() => (secret.value ? invalidHost(hosts.value) : undefined))
const unbinding = computed(() => secret.value && wasBound.value && hosts.value.length === 0)
const firstBinding = computed(() => secret.value && !wasBound.value && hosts.value.length > 0)

watch(
  () => props.open,
  (o) => {
    if (!o) return
    error.value = ''
    confirmUnbind.value = false
    hostsText.value = props.editing?.hosts.join('\n') ?? ''
    name.value = props.editing?.name ?? ''
    value.value = props.editing?.value ?? ''
    secret.value = props.editing ? props.editing.kind === 'secret' : false
    scope.value = options.value[0]?.value ?? ''
  },
)

async function save() {
  if (unbinding.value && !confirmUnbind.value) {
    confirmUnbind.value = true
    return
  }
  busy.value = true
  error.value = ''
  const kind = secret.value ? 'secret' : 'env'
  try {
    if (props.editing) {
      const body: Record<string, unknown> = { name: name.value.trim(), kind }
      if (value.value !== '' || !lockedSecret.value) body.value = value.value
      if (secret.value && hostsChanged.value) body.hosts = hosts.value
      await api(`/api/orgs/${props.orgId}/env/${props.editing.id}`, { method: 'PATCH', body: JSON.stringify(body) })
    } else {
      const target = props.scopes.find((s) => scopeKey(s) === scope.value)
      if (!target) return
      await api(`/api/orgs/${props.orgId}/env`, {
        method: 'POST',
        body: JSON.stringify({ name: name.value.trim(), value: value.value, kind, ...(secret.value ? { hosts: hosts.value } : {}), ...scopeRef(target) }),
      })
    }
    emit('saved')
    emit('update:open', false)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
    if (e instanceof ApiError && e.status === 409) emit('conflict')
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <Dialog :open="open" :title="editing ? t('env.editTitle') : t('env.addTitle')" @update:open="(o) => !busy && emit('update:open', o)">
    <TextInput v-model="name" :aria-label="t('env.name')" :placeholder="t('env.name')" />
    <Textarea
      v-model="value"
      :class="{ masked: secret }"
      spellcheck="false"
      :aria-label="t('env.value')"
      :placeholder="lockedSecret ? t('env.unchanged') : t('env.value')"
    />
    <Select v-if="!editing" v-model="scope" :options="options" :aria-label="t('env.scope')" />
    <Checkbox :model-value="secret" :disabled="lockedSecret" @update:model-value="(v) => (secret = v === true)">
      {{ t('env.makeSecret') }}
    </Checkbox>
    <template v-if="secret">
      <Textarea
        v-model="hostsText"
        spellcheck="false"
        :aria-label="t('env.hosts')"
        :placeholder="t('env.hostsPlaceholder')"
        @update:model-value="confirmUnbind = false"
      />
      <small>{{ t('env.hostsHint') }}</small>
      <p v-if="badHost" role="alert">{{ t('env.hostInvalid', { host: badHost }) }}</p>
      <p v-if="firstBinding && editing">{{ t('env.restartNote') }}</p>
      <p v-if="confirmUnbind" role="alert">{{ t('env.unbindBody') }}</p>
    </template>
    <p v-if="error" role="alert">{{ error }}</p>
    <template #actions>
      <Button :disabled="busy" @click="emit('update:open', false)">{{ t('profile.cancel') }}</Button>
      <Button variant="accent" :disabled="busy || !name.trim() || (!editing && !scope) || !!badHost" @click="save">{{ confirmUnbind ? t('env.unbindConfirm') : t('env.save') }}</Button>
    </template>
  </Dialog>
</template>

<style scoped>
.masked {
  -webkit-text-security: disc;
}
</style>
