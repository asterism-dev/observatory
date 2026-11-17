<script setup lang="ts">
import { Button, Checkbox, Dialog } from '@asterism-dev/lumen'
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import type { VmInfo } from '@/composables/nodes'

interface Network {
  id: string
  name: string
  subnet: string
  canUse: boolean
}

const props = defineProps<{ open: boolean; orgId: string; vm: VmInfo }>()
const emit = defineEmits<{ 'update:open': [boolean]; saved: [] }>()

const { t } = useI18n()
const networks = ref<Network[]>([])
const selected = ref<string[]>([])
const confirming = ref(false)
const busy = ref(false)
const error = ref('')

watch(
  () => props.open,
  async (o) => {
    if (!o) return
    confirming.value = false
    error.value = ''
    selected.value = props.vm.networks.map((n) => n.id)
    try {
      networks.value = await api<Network[]>(`/api/orgs/${props.orgId}/networks`)
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    }
  },
)

// Already attached networks stay listed so they can be detached even without use permission.
const available = () => networks.value.filter((n) => n.canUse || selected.value.includes(n.id))

function toggle(id: string, on: boolean | 'indeterminate') {
  selected.value = on === true ? [...selected.value, id] : selected.value.filter((x) => x !== id)
}

async function save(restartAllowed: boolean) {
  busy.value = true
  error.value = ''
  try {
    await api(`/api/orgs/${props.orgId}/vms/${props.vm.id}/networks`, {
      method: 'PUT',
      body: JSON.stringify({ networkIds: selected.value, restartAllowed }),
    })
    emit('saved')
    emit('update:open', false)
  } catch (e) {
    confirming.value = false
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}

const submit = () => (props.vm.state === 'running' ? (confirming.value = true) : save(false))
</script>

<template>
  <Dialog
    :open="open"
    :title="confirming ? t('networks.restartTitle') : t('networks.changeTitle')"
    :description="confirming ? t('networks.restartBody', { name: vm.name }) : undefined"
    @update:open="(o) => !busy && emit('update:open', o)"
  >
    <template v-if="!confirming">
      <p v-if="!available().length">{{ t('networks.noneAvailable') }}</p>
      <div v-for="n in available()" :key="n.id">
        <Checkbox :model-value="selected.includes(n.id)" @update:model-value="(v) => toggle(n.id, v)">{{ n.name }} ({{ n.subnet }})</Checkbox>
      </div>
    </template>
    <p v-if="error" role="alert">{{ error }}</p>
    <template #actions>
      <Button :disabled="busy" @click="confirming ? (confirming = false) : emit('update:open', false)">{{ t('profile.cancel') }}</Button>
      <Button v-if="confirming" variant="danger" :disabled="busy" @click="save(true)">{{ t('networks.restart') }}</Button>
      <Button v-else variant="accent" :disabled="busy" @click="submit">{{ t('networks.save') }}</Button>
    </template>
  </Dialog>
</template>
