<script setup lang="ts">
import { Button, Dialog, NumberInput, Select, TextInput } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import { keepsMemory, type VmInfo } from '@/composables/nodes'
import { useMe } from '@/composables/useMe'

interface Disk {
  id: string
  name: string
  owner: { kind: 'user' | 'team'; id: string }
  state: string
  canUse: boolean
  attachment?: { vm: { id: string } }
}

const maxDisks = 8
const mountPattern = /^(\/[A-Za-z0-9._-]+)+$/
const reservedTrees = ['/bin', '/boot', '/dev', '/etc', '/lib', '/proc', '/run', '/sbin', '/sys', '/usr', '/var/lib/cloud', '/home/ubuntu/.ssh']
const reservedPaths = ['/home', '/home/ubuntu', '/root', '/var', '/var/lib']

const props = defineProps<{ open: boolean; orgId: string; vm: VmInfo }>()
const emit = defineEmits<{ 'update:open': [boolean]; saved: [] }>()

const { t } = useI18n()
const { me } = useMe()
const disks = ref<Disk[]>([])
const rows = ref<{ diskId: string; mountPath: string }[]>([])
const toAdd = ref('')
const confirming = ref(false)
const busy = ref(false)
const error = ref('')
const newName = ref('')
const newSizeGB = ref<number | null>(10)

watch(
  () => props.open,
  async (o) => {
    if (!o) return
    confirming.value = false
    error.value = ''
    toAdd.value = ''
    newName.value = ''
    newSizeGB.value = 10
    rows.value = props.vm.disks.map((d) => ({ diskId: d.id, mountPath: d.mountPath }))
    await loadDisks()
  },
)

async function loadDisks() {
  try {
    disks.value = (await api<{ disks: Disk[] }>(`/api/orgs/${props.orgId}/disks`)).disks
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

const name = (id: string) => disks.value.find((d) => d.id === id)?.name ?? props.vm.disks.find((d) => d.id === id)?.name ?? id

const addable = computed(() =>
  disks.value.filter(
    (d) =>
      d.owner.kind === props.vm.owner.kind &&
      d.owner.id === props.vm.owner.id &&
      d.canUse &&
      d.state === 'ready' &&
      !d.attachment &&
      !rows.value.some((r) => r.diskId === d.id),
  ),
)
const options = computed(() => addable.value.map((d) => ({ value: d.id, label: d.name })))

function nextPath() {
  for (let i = 1; ; i++) {
    const p = i === 1 ? '/data' : `/data${i}`
    if (!rows.value.some((r) => r.mountPath === p)) return p
  }
}

function add(id: string) {
  if (!id) return
  rows.value.push({ diskId: id, mountPath: nextPath() })
  toAdd.value = ''
}

function pathError(r: { mountPath: string }) {
  const p = r.mountPath
  if (!mountPattern.test(p) || p.split('/').some((s) => s === '.' || s === '..')) return t('vmDisks.invalidPath')
  if (reservedPaths.includes(p) || reservedTrees.some((x) => p === x || p.startsWith(`${x}/`))) return t('vmDisks.reservedPath')
  if (rows.value.filter((x) => x.mountPath === p).length > 1) return t('vmDisks.duplicatePath')
  if (rows.value.some((x) => p.startsWith(`${x.mountPath}/`) || x.mountPath.startsWith(`${p}/`))) return t('vmDisks.nestedPath')
  return ''
}

// A new disk gets the VM's owner; for another user's VM it would be the caller's and could not be attached.
const canCreate = computed(() => props.vm.owner.kind === 'team' || props.vm.owner.id === me.value?.user.id)
const newNameInvalid = computed(() => !/^[a-z][a-z0-9-]{0,30}$/.test(newName.value))
const newSizeInvalid = computed(() => newSizeGB.value === null || !Number.isInteger(newSizeGB.value) || newSizeGB.value < 1 || newSizeGB.value > 4096)

async function createDisk() {
  busy.value = true
  error.value = ''
  try {
    const d = await api<{ id: string }>(`/api/orgs/${props.orgId}/disks`, {
      method: 'POST',
      body: JSON.stringify({
        name: newName.value,
        owner: props.vm.owner.kind === 'team' ? { kind: 'team', teamId: props.vm.owner.id } : { kind: 'user' },
        sizeGB: newSizeGB.value,
      }),
    })
    await loadDisks()
    add(d.id)
    newName.value = ''
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}

const invalid = computed(() => rows.value.some((r) => pathError(r)) || rows.value.length > maxDisks)

async function save(restartAllowed: boolean) {
  busy.value = true
  error.value = ''
  try {
    await api(`/api/orgs/${props.orgId}/vms/${props.vm.id}/disks`, {
      method: 'PUT',
      body: JSON.stringify({ disks: rows.value, restartAllowed }),
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

// The hub requires restartAllowed unless desired power is stopped, even while the VM is still starting or in error.
const submit = () => (props.vm.power !== 'stopped' ? (confirming.value = true) : save(false))
</script>

<template>
  <Dialog
    :open="open"
    :title="confirming ? t('vmDisks.restartTitle') : t('vmDisks.changeTitle')"
    :description="confirming ? t(keepsMemory(vm) ? 'vmDisks.restartBodySaved' : 'vmDisks.restartBody', { name: vm.name }) : undefined"
    @update:open="(o) => !busy && emit('update:open', o)"
  >
    <template v-if="!confirming">
      <p v-if="!rows.length">{{ t('vmDisks.none') }}</p>
      <div v-for="r in rows" :key="r.diskId">
        <strong>{{ name(r.diskId) }}</strong>
        <TextInput v-model="r.mountPath" :aria-label="t('vmDisks.mountPath')" :invalid="!!pathError(r)" />
        <Button size="sm" :disabled="busy" @click="rows = rows.filter((x) => x !== r)">{{ t('vmDisks.remove') }}</Button>
        <small v-if="pathError(r)" role="alert">{{ pathError(r) }}</small>
      </div>
      <label v-if="rows.length < maxDisks">
        {{ t('vmDisks.add') }}
        <Select v-model="toAdd" :options="options" :aria-label="t('vmDisks.add')" @update:model-value="(v: string) => add(v)" />
      </label>
      <small v-if="!options.length">{{ t('vmDisks.noneAvailable') }}</small>
      <div v-if="canCreate && rows.length < maxDisks">
        <TextInput v-model="newName" :placeholder="t('disks.name')" :aria-label="t('disks.name')" :invalid="!!newName && newNameInvalid" />
        <NumberInput v-model="newSizeGB" :aria-label="t('disks.size')" />
        <Button size="sm" :disabled="busy || newNameInvalid || newSizeInvalid" @click="createDisk">{{ t('vmDisks.createAndAdd') }}</Button>
        <small v-if="newName && newNameInvalid" role="alert">{{ t('disks.invalidName') }}</small>
        <small v-if="newSizeInvalid" role="alert">{{ t('disks.invalidSize') }}</small>
      </div>
    </template>
    <p v-if="error" role="alert">{{ error }}</p>
    <template #actions>
      <Button :disabled="busy" @click="confirming ? (confirming = false) : emit('update:open', false)">{{ t('profile.cancel') }}</Button>
      <Button v-if="confirming" variant="danger" :disabled="busy" @click="save(true)">{{ t('vmDisks.restart') }}</Button>
      <Button v-else variant="accent" :disabled="busy || invalid" @click="submit">{{ t('vmDisks.save') }}</Button>
    </template>
  </Dialog>
</template>
