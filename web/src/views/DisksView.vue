<script setup lang="ts">
import { Badge, Button, Dialog, Table, Tabs, type TableColumn } from '@asterism-dev/lumen'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import CopyDiskDialog from '@/components/CopyDiskDialog.vue'
import CreateDiskDialog from '@/components/CreateDiskDialog.vue'
import GrowDiskDialog from '@/components/GrowDiskDialog.vue'
import { useMe } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'

interface Disk {
  id: string
  name: string
  owner: { kind: 'user' | 'team'; id: string; name: string }
  sizeGB: number
  allocatedBytes: number
  state: 'creating' | 'ready' | 'growing' | 'releasing' | 'missing'
  exportError?: string
  attachment?: { vm: { id: string; name: string }; mountPath: string }
  canUpdate: boolean
  canDelete: boolean
  canUse: boolean
}

const tones = { ready: 'success', creating: 'warning', growing: 'warning', releasing: 'warning', missing: 'danger' } as const

const { t } = useI18n()
const { org } = useOrg()
const { me } = useMe()

const disks = ref<Disk[]>([])
const storageOnline = ref(true)
const error = ref('')
const tab = ref('all')
const createOpen = ref(false)
const copying = ref<Disk | null>(null)
const growing = ref<Disk | null>(null)
const pendingRelease = ref<Disk | null>(null)
const pendingDelete = ref<Disk | null>(null)
const busy = ref(false)

const tabs = computed(() => ['all', 'mine', 'teams'].map((v) => ({ value: v, label: t(`disks.tab.${v}`) })))
const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('disks.name') },
  { key: 'owner', label: t('disks.owner') },
  { key: 'size', label: t('disks.size') },
  { key: 'used', label: t('disks.used') },
  { key: 'state', label: t('disks.state') },
  { key: 'attached', label: t('disks.attachedTo') },
  { key: 'actions', label: '' },
])
const rows = computed(() =>
  disks.value.filter((d) => (tab.value === 'mine' ? d.owner.kind === 'user' && d.owner.id === me.value?.user.id : tab.value === 'teams' ? d.owner.kind === 'team' : true)),
)
const message = (e: unknown) => (e instanceof Error ? e.message : String(e))
const used = (bytes: number) => `${(bytes / 1024 ** 3).toFixed(1)} GB`

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const res = await api<{ disks: Disk[]; storageOnline: boolean }>(`/api/orgs/${id}/disks`)
    if (id === org.value?.id) {
      disks.value = res.disks
      storageOnline.value = res.storageOnline
    }
  } catch (e) {
    if (id === org.value?.id) error.value = message(e)
  }
}

async function act(fn: () => Promise<unknown>) {
  busy.value = true
  error.value = ''
  try {
    await fn()
    await refresh()
  } catch (e) {
    error.value = message(e)
  } finally {
    pendingRelease.value = null
    pendingDelete.value = null
    busy.value = false
  }
}

const release = (d: Disk) => act(() => api(`/api/orgs/${org.value?.id}/disks/${d.id}/release`, { method: 'POST' }))
const remove = (d: Disk) => act(() => api(`/api/orgs/${org.value?.id}/disks/${d.id}`, { method: 'DELETE' }))

const timer = window.setInterval(() => document.visibilityState === 'visible' && refresh(), 5000)
onBeforeUnmount(() => window.clearInterval(timer))

watch(
  () => org.value?.id,
  () => {
    error.value = ''
    disks.value = []
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <div class="head">
      <div>
        <h1>{{ t('nav.disks') }}</h1>
        <p>{{ t('disks.subtitle') }}</p>
      </div>
      <Button variant="accent" @click="createOpen = true">{{ t('disks.create') }}</Button>
    </div>
    <p v-if="!storageOnline" role="alert">{{ t('disks.storageOffline') }}</p>
    <p v-if="error" role="alert">{{ error }}</p>
    <Tabs v-model="tab" :tabs="tabs" />
    <Table :columns="columns" :rows="rows" row-key="id">
      <template #cell-owner="{ row }">{{ row.owner.name }}</template>
      <template #cell-size="{ row }">{{ row.sizeGB }} GB</template>
      <template #cell-used="{ row }">{{ used(row.allocatedBytes) }}</template>
      <template #cell-state="{ row }">
        <Badge :tone="tones[row.state as Disk['state']]">{{ t(`disks.states.${row.state}`) }}</Badge>
        <small v-if="row.exportError" role="alert">{{ row.exportError }}</small>
      </template>
      <template #cell-attached="{ row }">
        <router-link v-if="row.attachment" :to="`/orgs/${org.id}/vms/${row.attachment.vm.id}`">{{ row.attachment.vm.name }}</router-link>
      </template>
      <template #cell-actions="{ row }">
        <span class="row">
          <Button v-if="row.canUse" size="sm" :disabled="busy" @click="copying = row">{{ t('disks.copy') }}</Button>
          <Button v-if="row.canUpdate" size="sm" :disabled="busy" @click="growing = row">{{ t('disks.grow') }}</Button>
          <Button v-if="row.canUpdate && row.state === 'releasing'" size="sm" variant="danger" :disabled="busy" @click="pendingRelease = row">{{ t('disks.release') }}</Button>
          <Button v-if="row.canDelete" size="sm" variant="danger" :disabled="busy" @click="pendingDelete = row">{{ t('disks.delete') }}</Button>
        </span>
      </template>
      <template #empty>{{ t('disks.empty') }}</template>
    </Table>
    <CreateDiskDialog v-model:open="createOpen" :org-id="org.id" @created="refresh" />
    <CopyDiskDialog
      :open="!!copying"
      :org-id="org.id"
      :disk-id="copying?.id ?? ''"
      :disk-name="copying?.name ?? ''"
      @update:open="(o) => !o && (copying = null)"
      @copied="refresh"
    />
    <GrowDiskDialog
      :open="!!growing"
      :org-id="org.id"
      :disk-id="growing?.id ?? ''"
      :disk-name="growing?.name ?? ''"
      :current-g-b="growing?.sizeGB ?? 0"
      :attached="!!growing?.attachment"
      @update:open="(o) => !o && (growing = null)"
      @grown="refresh"
    />
    <Dialog
      :open="!!pendingRelease"
      :title="t('disks.releaseTitle')"
      :description="t('disks.releaseBody', { name: pendingRelease?.name })"
      @update:open="(o) => !o && !busy && (pendingRelease = null)"
    >
      <template #actions>
        <Button :disabled="busy" @click="pendingRelease = null">{{ t('profile.cancel') }}</Button>
        <Button variant="danger" :disabled="busy" @click="pendingRelease && release(pendingRelease)">{{ t('disks.release') }}</Button>
      </template>
    </Dialog>
    <Dialog
      :open="!!pendingDelete"
      :title="t('disks.deleteTitle')"
      :description="t('disks.deleteBody', { name: pendingDelete?.name })"
      @update:open="(o) => !o && !busy && (pendingDelete = null)"
    >
      <template #actions>
        <Button :disabled="busy" @click="pendingDelete = null">{{ t('profile.cancel') }}</Button>
        <Button variant="danger" :disabled="busy" @click="pendingDelete && remove(pendingDelete)">{{ t('disks.delete') }}</Button>
      </template>
    </Dialog>
  </template>
</template>

<style scoped>
.head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 8px;
}
.row {
  display: flex;
  gap: 8px;
}
</style>
