<script setup lang="ts">
import { Button, Dialog, NumberInput, Select, TextInput } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'

interface Owner {
  kind: 'user' | 'team'
  id: string
  name: string
}
interface NodeRef {
  id: string
  name: string
  online: boolean
}

const props = defineProps<{ open: boolean; orgId: string; owners: Owner[]; nodes: NodeRef[] }>()
const emit = defineEmits<{ 'update:open': [boolean]; created: [] }>()

const { t } = useI18n()
const namePattern = /^[a-z0-9][a-z0-9-]{0,62}$/
const name = ref('')
const owner = ref('')
const nodeId = ref('')
const cpus = ref<number | null>(2)
const memoryGB = ref<number | null>(4)
const diskGB = ref<number | null>(20)
const busy = ref(false)
const error = ref('')

const ownerKey = (o: Owner) => `${o.kind}:${o.id}`
const ownerOptions = computed(() => props.owners.map((o) => ({ value: ownerKey(o), label: o.kind === 'user' ? t('vms.ownerMe') : t('vms.ownerTeam', { name: o.name }) })))
const nodeOptions = computed(() => props.nodes.map((n) => ({ value: n.id, label: n.online ? n.name : `${n.name} (${t('nodes.states.offline')})` })))
const selected = computed(() => props.owners.find((o) => ownerKey(o) === owner.value))
const memoryMB = computed(() => (memoryGB.value === null ? null : Math.round(memoryGB.value * 1024)))

const inRange = (v: number | null, min: number, max: number) => v !== null && Number.isInteger(v) && v >= min && v <= max
const problems = computed(() => ({
  name: namePattern.test(name.value) ? '' : t('vms.invalidName'),
  cpus: inRange(cpus.value, 1, 32) ? '' : t('vms.invalidRange', { min: 1, max: 32 }),
  memory: inRange(memoryMB.value, 512, 131072) ? '' : t('vms.invalidRange', { min: '0.5', max: 128 }),
  disk: inRange(diskGB.value, 10, 2048) ? '' : t('vms.invalidRange', { min: 10, max: 2048 }),
}))
const valid = computed(() => !Object.values(problems.value).some(Boolean) && !!selected.value && !!nodeId.value)

watch(
  () => props.open,
  (o) => {
    if (!o) return
    name.value = ''
    owner.value = ownerOptions.value[0]?.value ?? ''
    nodeId.value = props.nodes.find((n) => n.online)?.id ?? props.nodes[0]?.id ?? ''
    cpus.value = 2
    memoryGB.value = 4
    diskGB.value = 20
    error.value = ''
  },
)

async function create() {
  const o = selected.value
  if (!o) return
  busy.value = true
  error.value = ''
  try {
    await api(`/api/orgs/${props.orgId}/vms`, {
      method: 'POST',
      body: JSON.stringify({
        name: name.value,
        owner: o.kind === 'team' ? { kind: 'team', teamId: o.id } : { kind: 'user' },
        nodeId: nodeId.value,
        cpus: cpus.value,
        memoryMB: memoryMB.value,
        diskGB: diskGB.value,
      }),
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
  <Dialog :open="open" :title="t('vms.createTitle')" @update:open="(o) => !busy && emit('update:open', o)">
    <div class="form">
      <label>
        {{ t('vms.name') }}
        <TextInput v-model="name" :aria-label="t('vms.name')" :invalid="!!name && !!problems.name" />
      </label>
      <small v-if="name && problems.name" role="alert">{{ problems.name }}</small>
      <label>
        {{ t('vms.owner') }}
        <Select v-model="owner" :options="ownerOptions" :aria-label="t('vms.owner')" />
      </label>
      <small>{{ selected?.kind === 'team' ? t('vms.hintTeam') : t('vms.hintUser') }}</small>
      <label>
        {{ t('vms.node') }}
        <Select v-model="nodeId" :options="nodeOptions" :aria-label="t('vms.node')" />
      </label>
      <label>
        {{ t('vms.cpu') }}
        <NumberInput v-model="cpus" :aria-label="t('vms.cpu')" :invalid="!!problems.cpus" />
      </label>
      <small v-if="problems.cpus" role="alert">{{ problems.cpus }}</small>
      <label>
        {{ t('vms.memory') }} (GB)
        <NumberInput v-model="memoryGB" :aria-label="t('vms.memory')" :invalid="!!problems.memory" />
      </label>
      <small v-if="problems.memory" role="alert">{{ problems.memory }}</small>
      <small v-else>{{ t('vms.memoryHint') }}</small>
      <label>
        {{ t('vms.disk') }} (GB)
        <NumberInput v-model="diskGB" :aria-label="t('vms.disk')" :invalid="!!problems.disk" />
      </label>
      <small v-if="problems.disk" role="alert">{{ problems.disk }}</small>
    </div>
    <p v-if="error" role="alert">{{ error }}</p>
    <template #actions>
      <Button :disabled="busy" @click="emit('update:open', false)">{{ t('profile.cancel') }}</Button>
      <Button variant="accent" :disabled="busy || !valid" @click="create">{{ t('nodes.create') }}</Button>
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
