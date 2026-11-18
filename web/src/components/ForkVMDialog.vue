<script setup lang="ts">
import { Button, Checkbox, Dialog, TextInput } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'

import { api } from '@/api'
import type { VmInfo } from '@/composables/nodes'
import type { Snapshot } from '@/components/VMSnapshots.vue'

const props = defineProps<{ open: boolean; orgId: string; vmId: string; snapshot: Snapshot | null }>()
const emit = defineEmits<{ 'update:open': [boolean] }>()

const { t } = useI18n()
const router = useRouter()
const name = ref('')
const memory = ref(false)
const busy = ref(false)
const error = ref('')
const nameInvalid = computed(() => !/^[a-z][a-z0-9-]{0,30}$/.test(name.value))

watch(
  () => props.open,
  () => {
    name.value = ''
    memory.value = false
    error.value = ''
  },
)

async function fork() {
  if (!props.snapshot) return
  busy.value = true
  error.value = ''
  try {
    const vm = await api<VmInfo>(`/api/orgs/${props.orgId}/vms/${props.vmId}/snapshots/${props.snapshot.id}/fork`, {
      method: 'POST',
      body: JSON.stringify({ name: name.value, memory: memory.value }),
    })
    emit('update:open', false)
    router.push(`/orgs/${props.orgId}/vms/${vm.id}`)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <Dialog :open="open" :title="t('snapshots.forkTitle', { name: snapshot?.name })" @update:open="(o) => !busy && emit('update:open', o)">
    <div class="form">
      <label>
        {{ t('snapshots.forkName') }}
        <TextInput v-model="name" :aria-label="t('snapshots.forkName')" :invalid="!!name && nameInvalid" />
      </label>
      <small v-if="name && nameInvalid" role="alert">{{ t('snapshots.invalidName') }}</small>
      <Checkbox v-if="snapshot?.memory" v-model="memory">{{ t('snapshots.forkMemory') }}</Checkbox>
    </div>
    <p v-if="error" role="alert">{{ error }}</p>
    <template #actions>
      <Button :disabled="busy" @click="emit('update:open', false)">{{ t('profile.cancel') }}</Button>
      <Button variant="accent" :disabled="busy || nameInvalid" @click="fork">{{ t('snapshots.fork') }}</Button>
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
