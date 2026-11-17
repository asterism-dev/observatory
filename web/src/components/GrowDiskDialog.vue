<script setup lang="ts">
import { Button, Dialog, NumberInput } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'

const props = defineProps<{ open: boolean; orgId: string; diskId: string; diskName: string; currentGB: number; attached: boolean }>()
const emit = defineEmits<{ 'update:open': [boolean]; grown: [] }>()

const { t } = useI18n()
const sizeGB = ref<number | null>(null)
const restartAllowed = ref(false)
const busy = ref(false)
const error = ref('')

const invalid = computed(() => sizeGB.value === null || !Number.isInteger(sizeGB.value) || sizeGB.value <= props.currentGB || sizeGB.value > 4096)

watch(
  () => props.open,
  (o) => {
    if (!o) return
    sizeGB.value = props.currentGB + 1
    restartAllowed.value = false
    error.value = ''
  },
)

async function grow() {
  busy.value = true
  error.value = ''
  try {
    await api(`/api/orgs/${props.orgId}/disks/${props.diskId}/grow`, {
      method: 'POST',
      body: JSON.stringify({ sizeGB: sizeGB.value, restartAllowed: restartAllowed.value }),
    })
    emit('grown')
    emit('update:open', false)
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <Dialog :open="open" :title="t('disks.growTitle', { name: diskName })" @update:open="(o) => !busy && emit('update:open', o)">
    <div class="form">
      <label>
        {{ t('disks.size') }}
        <NumberInput v-model="sizeGB" :aria-label="t('disks.size')" />
      </label>
      <small :role="invalid ? 'alert' : undefined">{{ t('disks.growHint', { size: currentGB }) }}</small>
      <label v-if="attached" class="check">
        <input v-model="restartAllowed" type="checkbox" />
        {{ t('disks.growRestart') }}
      </label>
    </div>
    <p v-if="error" role="alert">{{ error }}</p>
    <template #actions>
      <Button :disabled="busy" @click="emit('update:open', false)">{{ t('profile.cancel') }}</Button>
      <Button variant="accent" :disabled="busy || invalid" @click="grow">{{ t('disks.grow') }}</Button>
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
.check {
  display: flex;
  align-items: center;
  gap: 8px;
}
</style>
