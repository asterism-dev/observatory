<script setup lang="ts">
import { Button, Dialog, Select } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api, ApiError } from '@/api'
import { scopeKey, scopeLabel, scopeRef, type EnvVar, type WritableScope } from '@/composables/envScopes'

const props = defineProps<{ open: boolean; orgId: string; scopes: WritableScope[]; variable: EnvVar | null }>()
const emit = defineEmits<{ 'update:open': [boolean]; saved: []; conflict: [] }>()

const { t } = useI18n()
const scope = ref('')
const busy = ref(false)
const error = ref('')

const options = computed(() =>
  props.scopes
    .filter((s) => !props.variable || scopeKey(s) !== scopeKey(props.variable))
    .map((s) => ({ value: scopeKey(s), label: scopeLabel(s, t) })),
)

watch(
  () => props.open,
  (o) => {
    if (!o) return
    error.value = ''
    scope.value = options.value[0]?.value ?? ''
  },
)

async function move() {
  const target = props.scopes.find((s) => scopeKey(s) === scope.value)
  if (!props.variable || !target) return
  busy.value = true
  error.value = ''
  try {
    await api(`/api/orgs/${props.orgId}/env/${props.variable.id}/move`, { method: 'POST', body: JSON.stringify(scopeRef(target)) })
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
  <Dialog :open="open" :title="t('env.moveTitle', { name: variable?.name })" @update:open="(o) => !busy && emit('update:open', o)">
    <Select v-model="scope" :options="options" :aria-label="t('env.scope')" />
    <p v-if="error" role="alert">{{ error }}</p>
    <template #actions>
      <Button :disabled="busy" @click="emit('update:open', false)">{{ t('profile.cancel') }}</Button>
      <Button variant="accent" :disabled="busy || !scope" @click="move">{{ t('env.move') }}</Button>
    </template>
  </Dialog>
</template>
