<script lang="ts">
export interface Snapshot {
  id: string
  name: string
  memory: boolean
  state: 'creating' | 'ready' | 'error'
  error?: string
  sizeBytes: number
  disks: { name: string; mountPath: string; sizeGB: number; readyGB: number }[]
  createdAt: string
  canRestoreMemory: boolean
}
</script>

<script setup lang="ts">
import { Badge, Button, Checkbox, Dialog, Table, type TableColumn } from '@asterism-dev/lumen'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import ForkVMDialog from '@/components/ForkVMDialog.vue'
import TakeSnapshotDialog from '@/components/TakeSnapshotDialog.vue'
import { gb, keepsMemory, type VmInfo } from '@/composables/nodes'

const props = defineProps<{ orgId: string; vm: VmInfo }>()
const emit = defineEmits<{ changed: [] }>()

const { t, d } = useI18n()
const snapshots = ref<Snapshot[]>([])
const error = ref('')
const busy = ref(false)
const takeOpen = ref(false)
const restoring = ref<Snapshot | null>(null)
const restoreMemory = ref(false)
const forking = ref<Snapshot | null>(null)
const deleting = ref<Snapshot | null>(null)

const base = () => `/api/orgs/${props.orgId}/vms/${props.vm.id}/snapshots`
const tones = { creating: 'warning', ready: 'success', error: 'danger' } as const
const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('snapshots.name') },
  { key: 'createdAt', label: t('vms.created') },
  { key: 'memory', label: t('snapshots.memory') },
  { key: 'disks', label: t('snapshots.disks') },
  { key: 'size', label: t('snapshots.size') },
  { key: 'state', label: t('vms.state') },
  { key: 'actions', label: '' },
])
const size = (bytes: number) => (bytes ? `${gb(bytes / 1024 / 1024)} GB` : '–')
const restartNeeded = computed(() => props.vm.power !== 'stopped')

async function load() {
  const id = props.vm.id
  try {
    const res = await api<Snapshot[]>(base())
    if (id === props.vm.id) snapshots.value = res
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

async function run(fn: () => Promise<void>) {
  busy.value = true
  error.value = ''
  try {
    await fn()
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    restoring.value = null
    deleting.value = null
    busy.value = false
  }
}

const restore = (sn: Snapshot) =>
  run(async () => {
    await api(`${base()}/${sn.id}/restore`, {
      method: 'POST',
      body: JSON.stringify({ memory: restoreMemory.value, restartAllowed: restartNeeded.value }),
    })
    emit('changed')
  })

const remove = (sn: Snapshot) =>
  run(async () => {
    await api(`${base()}/${sn.id}`, { method: 'DELETE' })
    await load()
  })

function startRestore(sn: Snapshot) {
  restoreMemory.value = false
  restoring.value = sn
}

const timer = window.setInterval(
  () => document.visibilityState === 'visible' && snapshots.value.some((s) => s.state === 'creating') && load(),
  5000,
)
onBeforeUnmount(() => window.clearInterval(timer))

watch(
  () => props.vm.id,
  () => {
    snapshots.value = []
    error.value = ''
    load()
  },
  { immediate: true },
)
</script>

<template>
  <h2>{{ t('snapshots.section') }}</h2>
  <p v-if="vm.canSnapshot"><Button :disabled="busy" @click="takeOpen = true">{{ t('snapshots.take') }}</Button></p>
  <p v-if="error" role="alert">{{ error }}</p>
  <Table :columns="columns" :rows="snapshots" row-key="id">
    <template #cell-createdAt="{ row }">{{ d(new Date(row.createdAt), { dateStyle: 'short', timeStyle: 'short' }) }}</template>
    <template #cell-memory="{ row }">{{ row.memory ? t('snapshots.yes') : t('snapshots.no') }}</template>
    <template #cell-disks="{ row }">{{ row.disks.map((x: Snapshot['disks'][number]) => x.name).join(', ') || '–' }}</template>
    <template #cell-size="{ row }">{{ size(row.sizeBytes) }}</template>
    <template #cell-state="{ row }">
      <Badge :tone="tones[row.state as Snapshot['state']]">{{ t(`snapshots.states.${row.state}`) }}</Badge>
      <small v-if="row.error" role="alert"> {{ row.error }}</small>
    </template>
    <template #cell-actions="{ row }">
      <Button v-if="vm.canRestore && row.state === 'ready'" size="sm" :disabled="busy" @click="startRestore(row)">{{ t('snapshots.restore') }}</Button>
      <Button v-if="vm.canFork && row.state === 'ready'" size="sm" :disabled="busy" @click="forking = row">{{ t('snapshots.fork') }}</Button>
      <Button v-if="vm.canSnapshot && row.state !== 'creating'" size="sm" variant="danger" :disabled="busy" @click="deleting = row">{{ t('snapshots.delete') }}</Button>
    </template>
    <template #empty>{{ t('snapshots.empty') }}</template>
  </Table>
  <TakeSnapshotDialog v-model:open="takeOpen" :org-id="orgId" :vm="vm" @taken="load" />
  <ForkVMDialog :open="!!forking" :org-id="orgId" :vm-id="vm.id" :snapshot="forking" @update:open="(o) => !o && (forking = null)" />
  <Dialog
    :open="!!restoring"
    :title="t('snapshots.restoreTitle', { name: restoring?.name })"
    :description="restartNeeded ? t(keepsMemory(vm) ? 'snapshots.restoreBodySaved' : 'snapshots.restoreBody', { name: vm.name }) : t('snapshots.restoreBodyStopped', { name: vm.name })"
    @update:open="(o) => !o && !busy && (restoring = null)"
  >
    <Checkbox v-if="restoring?.state === 'ready' && restoring.canRestoreMemory && restoring.memory" v-model="restoreMemory">{{ t('snapshots.restoreMemory') }}</Checkbox>
    <template #actions>
      <Button :disabled="busy" @click="restoring = null">{{ t('profile.cancel') }}</Button>
      <Button variant="danger" :disabled="busy" @click="restoring && restore(restoring)">{{ t('snapshots.restore') }}</Button>
    </template>
  </Dialog>
  <Dialog
    :open="!!deleting"
    :title="t('snapshots.deleteTitle')"
    :description="t('snapshots.deleteBody', { name: deleting?.name })"
    @update:open="(o) => !o && !busy && (deleting = null)"
  >
    <template #actions>
      <Button :disabled="busy" @click="deleting = null">{{ t('profile.cancel') }}</Button>
      <Button variant="danger" :disabled="busy" @click="deleting && remove(deleting)">{{ t('snapshots.delete') }}</Button>
    </template>
  </Dialog>
</template>
