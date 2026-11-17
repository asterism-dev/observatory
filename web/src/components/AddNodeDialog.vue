<script setup lang="ts">
import { Button, Dialog, TextInput } from '@asterism-dev/lumen'
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import InstallCommand from '@/components/InstallCommand.vue'

const props = defineProps<{ open: boolean; orgId: string }>()
const emit = defineEmits<{ 'update:open': [boolean]; created: [] }>()

const { t } = useI18n()
const name = ref('')
const command = ref('')
const busy = ref(false)
const error = ref('')

watch(
  () => props.open,
  (o) => {
    if (!o) return
    name.value = ''
    command.value = ''
    error.value = ''
  },
)

async function create() {
  busy.value = true
  error.value = ''
  try {
    const res = await api<{ installCommand: string }>(`/api/orgs/${props.orgId}/nodes`, {
      method: 'POST',
      body: JSON.stringify({ name: name.value.trim() }),
    })
    command.value = res.installCommand
    emit('created')
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <Dialog :open="open" :title="t('nodes.addTitle')" @update:open="(o) => !busy && emit('update:open', o)">
    <template v-if="command">
      <p>{{ t('nodes.installHint') }}</p>
      <InstallCommand :command="command" />
    </template>
    <TextInput v-else v-model="name" :aria-label="t('nodes.name')" :placeholder="t('nodes.name')" @keydown.enter="name.trim() && !busy && create()" />
    <p v-if="error" role="alert">{{ error }}</p>
    <template #actions>
      <Button :disabled="busy" @click="emit('update:open', false)">{{ command ? t('nodes.done') : t('profile.cancel') }}</Button>
      <Button v-if="!command" variant="accent" :disabled="busy || !name.trim()" @click="create">{{ t('nodes.create') }}</Button>
    </template>
  </Dialog>
</template>
