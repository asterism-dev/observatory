<script setup lang="ts">
import { Button, Dialog } from '@asterism-dev/lumen'
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import { vmImpact } from '@/composables/nodeUpdates'
import type { NodeInfo } from '@/composables/nodes'

const props = defineProps<{ open: boolean; orgId: string; nodes: NodeInfo[] }>()
const emit = defineEmits<{ 'update:open': [boolean]; started: [] }>()

const { t } = useI18n()
const busy = ref(false)
const error = ref('')

watch(
  () => props.open,
  (o) => o && (error.value = ''),
)

async function start() {
  busy.value = true
  error.value = ''
  try {
    await api(`/api/orgs/${props.orgId}/node-updates`, {
      method: 'POST',
      body: JSON.stringify({ nodes: props.nodes.map((n) => n.id) }),
    })
    emit('update:open', false)
    emit('started')
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}
</script>

<template>
  <Dialog :open="open" :title="t('nodes.update.title')" @update:open="(o) => !busy && emit('update:open', o)">
    <ul class="list">
      <li v-for="n in nodes" :key="n.id">
        <strong>{{ n.name }}</strong>
        <span class="muted"> · {{ t('nodes.update.vmCount', { n: n.vmCount }) }} · {{ t(`nodes.update.${vmImpact(n)}`) }}</span>
      </li>
    </ul>
    <p v-if="error" role="alert">{{ error }}</p>
    <template #actions>
      <Button :disabled="busy" @click="emit('update:open', false)">{{ t('profile.cancel') }}</Button>
      <Button variant="accent" :disabled="busy || !nodes.length" @click="start">{{ t('nodes.update.confirm') }}</Button>
    </template>
  </Dialog>
</template>

<style scoped>
.list {
  display: grid;
  gap: 8px;
  margin: 0;
  padding: 0;
  list-style: none;
}
</style>
