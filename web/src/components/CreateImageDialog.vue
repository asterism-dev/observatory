<script setup lang="ts">
import { Button, Dialog, Select, Textarea, TextInput } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import { scopeKey, scopeLabel, scopeRef, type WritableScope } from '@/composables/envScopes'

const props = defineProps<{ open: boolean; orgId: string; scopes: WritableScope[] }>()
const emit = defineEmits<{ 'update:open': [boolean]; created: [string] }>()

const { t } = useI18n()
const namePattern = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/
const name = ref('')
const description = ref('')
const scope = ref('')
const busy = ref(false)
const error = ref('')

const options = computed(() => props.scopes.map((s) => ({ value: scopeKey(s), label: scopeLabel(s, t) })))
const nameValid = computed(() => namePattern.test(name.value))

watch(
  () => props.open,
  (o) => {
    if (!o) return
    name.value = ''
    description.value = ''
    scope.value = options.value[0]?.value ?? ''
    error.value = ''
  },
)

async function create() {
  const target = props.scopes.find((s) => scopeKey(s) === scope.value)
  if (!target) return
  busy.value = true
  error.value = ''
  try {
    const res = await api<{ id: string }>(`/api/orgs/${props.orgId}/images`, {
      method: 'POST',
      body: JSON.stringify({ name: name.value, description: description.value.trim(), ...scopeRef(target) }),
    })
    emit('update:open', false)
    emit('created', res.id)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <Dialog :open="open" :title="t('images.createTitle')" @update:open="(o) => !busy && emit('update:open', o)">
    <div class="form">
      <label>
        {{ t('images.name') }}
        <TextInput v-model="name" :aria-label="t('images.name')" :invalid="!!name && !nameValid" />
      </label>
      <small v-if="name && !nameValid" role="alert">{{ t('vms.invalidName') }}</small>
      <label>
        {{ t('images.description') }}
        <Textarea v-model="description" :aria-label="t('images.description')" />
      </label>
      <label>
        {{ t('images.visibility') }}
        <Select v-model="scope" :options="options" :aria-label="t('images.visibility')" />
      </label>
    </div>
    <p v-if="error" role="alert">{{ error }}</p>
    <template #actions>
      <Button :disabled="busy" @click="emit('update:open', false)">{{ t('profile.cancel') }}</Button>
      <Button variant="accent" :disabled="busy || !nameValid || !scope" @click="create">{{ t('images.create') }}</Button>
    </template>
  </Dialog>
</template>

<style scoped>
.form {
  display: grid;
  gap: 8px;
}
label {
  display: grid;
  gap: 4px;
}
</style>
