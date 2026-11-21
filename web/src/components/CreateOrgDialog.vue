<script setup lang="ts">
import { Button, Dialog, TextInput } from '@asterism-dev/lumen'
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { errorMessage, iamClient, isUnauthenticated } from '@/composables/iamClient'
import { useMe, type Org } from '@/composables/useMe'

const props = defineProps<{ open: boolean }>()
const emit = defineEmits<{ 'update:open': [boolean]; created: [Org] }>()

const { t } = useI18n()
const { expire } = useMe()
const name = ref('')
const busy = ref(false)
const error = ref('')

watch(
  () => props.open,
  (o) => {
    if (o) name.value = error.value = ''
  },
)

async function create() {
  busy.value = true
  error.value = ''
  try {
    const res = await iamClient.createOrg({ name: name.value.trim() })
    emit('created', res.org!)
    emit('update:open', false)
  } catch (e) {
    if (isUnauthenticated(e)) return expire()
    error.value = errorMessage(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <Dialog :open="open" :title="t('org.createTitle')" @update:open="(o) => !busy && emit('update:open', o)">
    <TextInput v-model="name" :aria-label="t('org.name')" :placeholder="t('org.name')" @keydown.enter="name.trim() && create()" />
    <p v-if="error" role="alert">{{ error }}</p>
    <template #actions>
      <Button :disabled="busy" @click="emit('update:open', false)">{{ t('profile.cancel') }}</Button>
      <Button variant="accent" :disabled="busy || !name.trim()" @click="create">{{ t('org.create') }}</Button>
    </template>
  </Dialog>
</template>
