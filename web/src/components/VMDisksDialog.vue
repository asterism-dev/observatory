<script setup lang="ts">
import { Button, Dialog, Select, TextInput } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import type { VmInfo } from '@/composables/nodes'

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
const disks = ref<Disk[]>([])
const rows = ref<{ diskId: string; mountPath: string }[]>([])
const toAdd = ref('')
const confirming = ref(false)
const busy = ref(false)
const error = ref('')

watch(
  () => props.open,
  async (o) => {
    if (!o) return
    confirming.value = false
    error.value = ''
    toAdd.value = ''
    rows.value = props.vm.disks.map((d) => ({ diskId: d.id, mountPath: d.mountPath }))
    try {
      disks.value = (await api<{ disks: Disk[] }>(`/api/orgs/${props.orgId}/disks`)).disks
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    }
  },
)

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
  return ''
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

const submit = () => (props.vm.state === 'running' ? (confirming.value = true) : save(false))
</script>

<template>
  <Dialog
    :open="open"
    :title="confirming ? t('vmDisks.restartTitle') : t('vmDisks.changeTitle')"
    :description="confirming ? t('vmDisks.restartBody', { name: vm.name }) : undefined"
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
    </template>
    <p v-if="error" role="alert">{{ error }}</p>
    <template #actions>
      <Button :disabled="busy" @click="confirming ? (confirming = false) : emit('update:open', false)">{{ t('profile.cancel') }}</Button>
      <Button v-if="confirming" variant="danger" :disabled="busy" @click="save(true)">{{ t('vmDisks.restart') }}</Button>
      <Button v-else variant="accent" :disabled="busy || invalid" @click="submit">{{ t('vmDisks.save') }}</Button>
    </template>
  </Dialog>
</template>
