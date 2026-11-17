<script setup lang="ts">
import { Badge, Button, Dialog, Table, TextInput, type TableColumn } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'

import { api, ApiError } from '@/api'
import InstallCommand from '@/components/InstallCommand.vue'
import { gb, stateTones, vmTones, type NodeInfo, type VmInfo } from '@/composables/nodes'
import { useOrg } from '@/composables/useOrg'

const { t, d } = useI18n()
const route = useRoute()
const router = useRouter()
const { org } = useOrg()

const nodeId = computed(() => String(route.params.nodeId))
const node = ref<NodeInfo | null>(null)
const vms = ref<VmInfo[]>([])
const name = ref('')
const error = ref('')
const busy = ref(false)
const confirmRegen = ref(false)
const command = ref('')
const confirmDelete = ref(false)

const vmColumns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('vms.name') },
  { key: 'state', label: t('vms.state') },
  { key: 'cpus', label: t('vms.cpu') },
  { key: 'memory', label: t('vms.memory') },
])
const machine = computed(() => Object.entries(node.value?.machine ?? {}))
const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

async function load() {
  if (!org.value) return
  const id = org.value.id
  try {
    const n = await api<NodeInfo>(`/api/orgs/${id}/nodes/${nodeId.value}`)
    if (id !== org.value?.id) return
    node.value = n
    name.value = n.name
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return router.replace(`/orgs/${id}/nodes`)
    error.value = message(e)
    return
  }
  try {
    const res = await api<{ vms: VmInfo[] }>(`/api/orgs/${id}/vms`)
    vms.value = res.vms.filter((v) => v.node.id === nodeId.value)
  } catch {
    // Listing VMs needs its own permission; the section just stays empty.
    vms.value = []
  }
}

async function run(fn: () => Promise<void>) {
  busy.value = true
  error.value = ''
  try {
    await fn()
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = false
  }
}

const base = () => `/api/orgs/${org.value?.id}/nodes/${nodeId.value}`

const rename = () =>
  run(async () => {
    await api(base(), { method: 'PATCH', body: JSON.stringify({ name: name.value.trim() }) })
    await load()
  })

const regenerate = () =>
  run(async () => {
    const res = await api<{ installCommand: string }>(`${base()}/credential`, { method: 'POST' })
    confirmRegen.value = false
    command.value = res.installCommand
    await load()
  })

const remove = () =>
  run(async () => {
    try {
      await api(base(), { method: 'DELETE' })
    } catch (e) {
      confirmDelete.value = false
      throw e
    }
    router.replace(`/orgs/${org.value?.id}/nodes`)
  })

watch(
  () => [org.value?.id, nodeId.value],
  () => {
    node.value = null
    vms.value = []
    error.value = ''
    load()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <p><router-link :to="`/orgs/${org.id}/nodes`">{{ t('nav.nodes') }}</router-link></p>
    <p v-if="error" role="alert">{{ error }}</p>
    <template v-if="node">
      <h1>{{ node.name }} <Badge :tone="stateTones[node.state]">{{ t(`nodes.states.${node.state}`) }}</Badge></h1>
      <form v-if="node.canUpdate" class="row" @submit.prevent="rename">
        <TextInput v-model="name" :aria-label="t('nodes.name')" />
        <Button type="submit" :disabled="busy || !name.trim() || name.trim() === node.name">{{ t('nodes.rename') }}</Button>
      </form>
      <dl>
        <dt>{{ t('nodes.version') }}</dt>
        <dd>{{ node.version || '–' }}</dd>
        <dt>{{ t('nodes.cpu') }}</dt>
        <dd>{{ node.usedCPUs }} / {{ node.cpus }}</dd>
        <dt>{{ t('nodes.memory') }}</dt>
        <dd>{{ gb(node.usedMemoryMB) }} / {{ gb(node.memoryMB) }} GB</dd>
        <dt>{{ t('nodes.lastSeen') }}</dt>
        <dd>{{ node.lastSeen ? d(new Date(node.lastSeen), { dateStyle: 'short', timeStyle: 'short' }) : '–' }}</dd>
        <template v-if="node.underlay">
          <dt>{{ t('nodes.underlay') }}</dt>
          <dd>{{ node.underlay.ip }}</dd>
          <dt>{{ t('nodes.registered') }}</dt>
          <dd>{{ node.underlay.registered ? t('nodes.yes') : t('nodes.no') }}</dd>
          <dt>{{ t('nodes.handshake') }}</dt>
          <dd>{{ node.underlay.lastHandshake ? d(new Date(node.underlay.lastHandshake), { dateStyle: 'short', timeStyle: 'short' }) : t('nodes.never') }}</dd>
        </template>
        <template v-for="[k, v] in machine" :key="k">
          <dt>{{ k }}</dt>
          <dd>{{ v }}</dd>
        </template>
      </dl>

      <h2>{{ t('nodes.vms') }}</h2>
      <Table :columns="vmColumns" :rows="vms" row-key="id">
        <template #cell-name="{ row }"><router-link :to="`/orgs/${org.id}/vms/${row.id}`">{{ row.name }}</router-link></template>
        <template #cell-state="{ row }"><Badge :tone="vmTones[row.state as VmInfo['state']]">{{ t(`vms.states.${row.state}`) }}</Badge></template>
        <template #cell-memory="{ row }">{{ gb(row.memoryMB) }} GB</template>
        <template #empty>{{ t('nodes.noVms') }}</template>
      </Table>

      <p v-if="node.canUpdate || node.canDelete" class="row">
        <Button v-if="node.canUpdate" :disabled="busy" @click="confirmRegen = true">{{ t('nodes.regenerate') }}</Button>
        <Button v-if="node.canDelete" variant="danger" :disabled="busy" @click="confirmDelete = true">{{ t('nodes.delete') }}</Button>
      </p>

      <Dialog :open="confirmRegen" :title="t('nodes.regenerateTitle')" :description="t('nodes.regenerateBody')" @update:open="(o) => !o && !busy && (confirmRegen = false)">
        <template #actions>
          <Button :disabled="busy" @click="confirmRegen = false">{{ t('profile.cancel') }}</Button>
          <Button variant="danger" :disabled="busy" @click="regenerate">{{ t('nodes.regenerate') }}</Button>
        </template>
      </Dialog>
      <Dialog :open="!!command" :title="t('nodes.installTitle')" @update:open="(o) => !o && (command = '')">
        <InstallCommand :command="command" />
        <template #actions><Button @click="command = ''">{{ t('nodes.done') }}</Button></template>
      </Dialog>
      <Dialog
        :open="confirmDelete"
        :title="t('nodes.deleteTitle')"
        :description="t('nodes.deleteBody', { name: node.name })"
        @update:open="(o) => !o && !busy && (confirmDelete = false)"
      >
        <template #actions>
          <Button :disabled="busy" @click="confirmDelete = false">{{ t('profile.cancel') }}</Button>
          <Button variant="danger" :disabled="busy" @click="remove">{{ t('nodes.delete') }}</Button>
        </template>
      </Dialog>
    </template>
  </template>
</template>

<style scoped>
.row {
  display: flex;
  gap: 8px;
  margin-bottom: 12px;
}
dl {
  display: grid;
  grid-template-columns: max-content 1fr;
  gap: 4px 16px;
}
dd {
  margin: 0;
}
</style>
