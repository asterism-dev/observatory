<script setup lang="ts">
import { Badge, Button, Dialog, Table, Tabs, type TableColumn } from '@asterism-dev/lumen'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'

import { api } from '@/api'
import CreateVMDialog from '@/components/CreateVMDialog.vue'
import { gb, powerActions, vmTones, type VmInfo } from '@/composables/nodes'
import { useMe } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'

interface Listing {
  vms: VmInfo[]
  creatableOwners: { kind: 'user' | 'team'; id: string; name: string }[]
  nodes: { id: string; name: string; online: boolean }[]
}

const { t } = useI18n()
const router = useRouter()
const { org } = useOrg()
const { me } = useMe()

const data = ref<Listing>({ vms: [], creatableOwners: [], nodes: [] })
const error = ref('')
const createOpen = ref(false)
const tab = ref('all')
const pendingDelete = ref<VmInfo | null>(null)
const busy = ref(false)

const tabs = computed(() => ['all', 'mine', 'teams'].map((v) => ({ value: v, label: t(`vms.tab.${v}`) })))
const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('vms.name') },
  { key: 'owner', label: t('vms.owner') },
  { key: 'node', label: t('vms.node') },
  { key: 'state', label: t('vms.state') },
  { key: 'cpu', label: t('vms.cpu') },
  { key: 'memory', label: t('vms.memory') },
  { key: 'disk', label: t('vms.disk') },
  { key: 'actions', label: '' },
])
const rows = computed(() =>
  data.value.vms.filter((v) => (tab.value === 'mine' ? v.owner.kind === 'user' && v.owner.id === me.value?.user.id : tab.value === 'teams' ? v.owner.kind === 'team' : true)),
)
const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const res = await api<Listing>(`/api/orgs/${id}/vms`)
    if (id === org.value?.id) data.value = res
  } catch (e) {
    if (id === org.value?.id) error.value = message(e)
  }
}

async function act(fn: () => Promise<unknown>) {
  busy.value = true
  error.value = ''
  try {
    await fn()
    pendingDelete.value = null
    await refresh()
  } catch (e) {
    error.value = message(e)
    pendingDelete.value = null
  } finally {
    busy.value = false
  }
}

const power = (v: VmInfo, action: string) => act(() => api(`/api/orgs/${org.value?.id}/vms/${v.id}/${action}`, { method: 'POST' }))
const remove = (v: VmInfo) => act(() => api(`/api/orgs/${org.value?.id}/vms/${v.id}`, { method: 'DELETE' }))

const timer = window.setInterval(() => document.visibilityState === 'visible' && refresh(), 5000)
onBeforeUnmount(() => window.clearInterval(timer))

watch(
  () => org.value?.id,
  () => {
    error.value = ''
    data.value = { vms: [], creatableOwners: [], nodes: [] }
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <div class="head">
      <div>
        <h1>{{ t('nav.vms') }}</h1>
        <p>{{ t('vms.subtitle') }}</p>
      </div>
      <Button v-if="data.creatableOwners.length" variant="accent" @click="createOpen = true">{{ t('vms.create') }}</Button>
    </div>
    <p v-if="error" role="alert">{{ error }}</p>
    <Tabs v-model="tab" :tabs="tabs" />
    <Table :columns="columns" :rows="rows" row-key="id">
      <template #cell-name="{ row }"><router-link :to="`/orgs/${org.id}/vms/${row.id}`">{{ row.name }}</router-link></template>
      <template #cell-owner="{ row }">{{ row.owner.name }}</template>
      <template #cell-node="{ row }">{{ row.node.name }}</template>
      <template #cell-state="{ row }"><Badge :tone="vmTones[row.state as VmInfo['state']]">{{ t(`vms.states.${row.state}`) }}</Badge></template>
      <template #cell-cpu="{ row }">{{ row.cpus }}</template>
      <template #cell-memory="{ row }">{{ gb(row.memoryMB) }} GB</template>
      <template #cell-disk="{ row }">{{ row.diskGB }} GB</template>
      <template #cell-actions="{ row }">
        <span class="row">
          <template v-if="row.canPower">
            <Button v-for="a in powerActions(row.state)" :key="a" size="sm" :disabled="busy" @click="power(row, a)">{{ t(`vms.${a}`) }}</Button>
          </template>
          <Button v-if="row.canDelete" size="sm" variant="danger" :disabled="busy" @click="pendingDelete = row">{{ t('vms.delete') }}</Button>
          <Button size="sm" @click="router.push(`/orgs/${org.id}/vms/${row.id}`)">{{ t('nodes.open') }}</Button>
        </span>
      </template>
      <template #empty>{{ t('vms.empty') }}</template>
    </Table>
    <CreateVMDialog v-model:open="createOpen" :org-id="org.id" :owners="data.creatableOwners" :nodes="data.nodes" @created="refresh" />
    <Dialog
      :open="!!pendingDelete"
      :title="t('vms.deleteTitle')"
      :description="t('vms.deleteBody', { name: pendingDelete?.name })"
      @update:open="(o) => !o && !busy && (pendingDelete = null)"
    >
      <template #actions>
        <Button :disabled="busy" @click="pendingDelete = null">{{ t('profile.cancel') }}</Button>
        <Button variant="danger" :disabled="busy" @click="pendingDelete && remove(pendingDelete)">{{ t('vms.delete') }}</Button>
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
