<script setup lang="ts">
import { Button, Dialog, Tabs, TextInput } from '@asterism-dev/lumen'
import { HardDrive, Plus } from '@lucide/vue'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import CopyDiskDialog from '@/components/CopyDiskDialog.vue'
import CreateDiskDialog from '@/components/CreateDiskDialog.vue'
import GrowDiskDialog from '@/components/GrowDiskDialog.vue'
import { useMe } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'
import { EmptyState, FilterBar, Meter, Page, ResourceList, ResourceRow, Tag } from '@/ui'

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
const loaded = ref(false)
const search = ref('')

const tabs = computed(() => ['all', 'mine', 'teams'].map((v) => ({ value: v, label: t(`disks.tab.${v}`) })))
const isMine = (d: Disk) => d.owner.kind === 'user' && d.owner.id === me.value?.user.id
const rows = computed(() => {
  const q = search.value.trim().toLowerCase()
  return disks.value.filter((d) => (tab.value === 'mine' ? isMine(d) : tab.value === 'teams' ? d.owner.kind === 'team' : true) && (!q || d.name.includes(q)))
})
const message = (e: unknown) => (e instanceof Error ? e.message : String(e))
const usedGB = (d: Disk) => d.allocatedBytes / 1024 ** 3

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const res = await api<{ disks: Disk[]; storageOnline: boolean }>(`/api/orgs/${id}/disks`)
    if (id === org.value?.id) {
      disks.value = res.disks
      storageOnline.value = res.storageOnline
      loaded.value = true
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
    loaded.value = false
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <Page v-if="org" :icon="HardDrive" :title="t('nav.disks')" :subtitle="t('disks.subtitle')">
    <template #actions>
      <Button variant="accent" :icon="Plus" @click="createOpen = true">{{ t('disks.create') }}</Button>
    </template>
    <p v-if="!storageOnline" role="alert">{{ t('disks.storageOffline') }}</p>
    <p v-if="error" role="alert">{{ error }}</p>
    <EmptyState v-if="loaded && !disks.length" :label="t('nav.disks')" :title="t('disks.empty')" :text="t('disks.emptyText')" command="observatory disk create data --size 20">
      <Button variant="accent" :icon="Plus" @click="createOpen = true">{{ t('disks.create') }}</Button>
    </EmptyState>
    <template v-else>
      <FilterBar>
        <Tabs v-model="tab" :tabs="tabs" variant="underline" />
        <template #end>
          <TextInput v-model="search" type="search" :placeholder="t('disks.search')" :aria-label="t('disks.search')" />
        </template>
      </FilterBar>
      <ResourceList :count="rows.length" :empty="disks.length ? t('disks.noMatch') : t('disks.empty')">
        <ResourceRow v-for="d in rows" :key="d.id" :title="d.name" :meta="`${usedGB(d).toFixed(1)} / ${d.sizeGB} GB`" :tone="tones[d.state]">
          <template #tags>
            <Tag>{{ isMine(d) ? t('disks.tab.mine') : d.owner.name }}</Tag>
            <Tag :tone="tones[d.state]">{{ t(`disks.states.${d.state}`) }}</Tag>
          </template>
          <Meter :label="t('usage.dsk')" :percent="d.sizeGB ? (usedGB(d) / d.sizeGB) * 100 : null" />
          <span v-if="d.attachment">{{ t('disks.attachedTo') }} <router-link :to="`/orgs/${org.id}/vms/${d.attachment.vm.id}`" class="mono">{{ d.attachment.vm.name }}</router-link></span>
          <small v-if="d.exportError" role="alert">{{ d.exportError }}</small>
          <template #actions>
            <Button v-if="d.canUse" size="sm" variant="ghost" :disabled="busy" @click="copying = d">{{ t('disks.copy') }}</Button>
            <Button v-if="d.canUpdate" size="sm" variant="ghost" :disabled="busy" @click="growing = d">{{ t('disks.grow') }}</Button>
            <Button v-if="d.canUpdate && d.state === 'releasing'" size="sm" variant="ghost" :disabled="busy" @click="pendingRelease = d">{{ t('disks.release') }}</Button>
            <Button v-if="d.canDelete" size="sm" variant="ghost" :disabled="busy" @click="pendingDelete = d">{{ t('disks.delete') }}</Button>
          </template>
        </ResourceRow>
      </ResourceList>
    </template>
    <CreateDiskDialog v-model:open="createOpen" :org-id="org.id" @created="refresh" />
    <CopyDiskDialog
      :open="!!copying"
      :org-id="org.id"
      :disk-id="copying?.id ?? ''"
      :disk-name="copying?.name ?? ''"
      :unsettled="!!copying && (!!copying.attachment || ['releasing', 'creating', 'growing'].includes(copying.state))"
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
  </Page>
</template>

