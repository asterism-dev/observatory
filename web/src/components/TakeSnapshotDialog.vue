<script setup lang="ts">
import { Button, Checkbox, Dialog, TextInput } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import type { VmInfo } from '@/composables/nodes'

const props = defineProps<{ open: boolean; orgId: string; vm: VmInfo }>()
const emit = defineEmits<{ 'update:open': [boolean]; taken: [] }>()

const { t } = useI18n()
const name = ref('')
const memory = ref(false)
const busy = ref(false)
const error = ref('')
const nameInvalid = computed(() => !/^[a-z][a-z0-9-]{0,30}$/.test(name.value))
const live = computed(() => ['running', 'paused', 'hibernated', 'suspended'].includes(props.vm.state))

watch(
  () => props.open,
  () => {
    name.value = ''
    memory.value = false
    error.value = ''
  },
)

async function take() {
  busy.value = true
  error.value = ''
  try {
    await api(`/api/orgs/${props.orgId}/vms/${props.vm.id}/snapshots`, {
      method: 'POST',
      body: JSON.stringify({ name: name.value, memory: live.value && memory.value }),
    })
    emit('taken')
    emit('update:open', false)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <Dialog :open="open" :title="t('snapshots.takeTitle')" @update:open="(o) => !busy && emit('update:open', o)">
    <div class="form">
      <label>
        {{ t('snapshots.name') }}
        <TextInput v-model="name" :aria-label="t('snapshots.name')" :invalid="!!name && nameInvalid" />
      </label>
      <small v-if="name && nameInvalid" role="alert">{{ t('snapshots.invalidName') }}</small>
      <Checkbox v-if="live" v-model="memory">{{ t('snapshots.includeMemory') }}</Checkbox>
    </div>
    <p v-if="error" role="alert">{{ error }}</p>
    <template #actions>
      <Button :disabled="busy" @click="emit('update:open', false)">{{ t('profile.cancel') }}</Button>
      <Button variant="accent" :disabled="busy || nameInvalid" @click="take">{{ t('snapshots.take') }}</Button>
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
