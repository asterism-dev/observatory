<script setup lang="ts">
import { Button, Dialog, TextInput } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'

const props = defineProps<{ open: boolean; orgId: string }>()
const emit = defineEmits<{ 'update:open': [boolean]; created: [] }>()

const { t } = useI18n()
const name = ref('')
const subnet = ref('')
const busy = ref(false)
const error = ref('')
const nameInvalid = computed(() => !/^[a-z][a-z0-9-]{0,30}$/.test(name.value))

watch(
  () => props.open,
  () => {
    name.value = ''
    subnet.value = ''
    error.value = ''
  },
)

async function create() {
  busy.value = true
  error.value = ''
  try {
    await api(`/api/orgs/${props.orgId}/networks`, {
      method: 'POST',
      body: JSON.stringify({ name: name.value, subnet: subnet.value.trim() }),
    })
    emit('created')
    emit('update:open', false)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <Dialog :open="open" :title="t('networks.createTitle')" @update:open="(o) => !busy && emit('update:open', o)">
    <div class="form">
      <label>
        {{ t('networks.name') }}
        <TextInput v-model="name" :aria-label="t('networks.name')" :invalid="!!name && nameInvalid" />
      </label>
      <small v-if="name && nameInvalid" role="alert">{{ t('networks.invalidName') }}</small>
      <label>
        {{ t('networks.subnet') }}
        <TextInput v-model="subnet" :aria-label="t('networks.subnet')" placeholder="10.20.0.0/24" />
      </label>
      <small>{{ t('networks.subnetHint') }}</small>
    </div>
    <p v-if="error" role="alert">{{ error }}</p>
    <template #actions>
      <Button :disabled="busy" @click="emit('update:open', false)">{{ t('profile.cancel') }}</Button>
      <Button variant="accent" :disabled="busy || nameInvalid" @click="create">{{ t('devices.create') }}</Button>
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
