<script setup lang="ts">
import { Button, Dialog, Tabs, TextInput } from '@asterism-dev/lumen'
import { HardDrive } from '@lucide/vue'
import { computed, onBeforeUnmount, ref, watch, watchEffect } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'

import { api, ApiError } from '@/api'
import UpdateNodesDialog from '@/components/UpdateNodesDialog.vue'
import InstallCommand from '@/components/InstallCommand.vue'
import TelemetryLogs from '@/components/TelemetryLogs.vue'
import TelemetryUsage from '@/components/TelemetryUsage.vue'
import VMList from '@/components/VMList.vue'
import { gb, imageLine, stateTones, type NodeInfo, type VmInfo } from '@/composables/nodes'
import { useCrumbs } from '@/composables/useCrumbs'
import { useMe } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'
import type { UsageRange } from '@/composables/telemetry'
import { Card, KeyValue, KeyValueList, Meter, PageHeader, Section, Tag } from '@/ui'

const { t, d, locale } = useI18n()
const route = useRoute()
const router = useRouter()
const { org, can } = useOrg()
const { me } = useMe()

const nodeId = computed(() => String(route.params.nodeId))
const tabIds = computed(() => (node.value?.canTelemetry ? ['overview', 'metrics', 'vms', 'settings'] : ['overview', 'vms', 'settings']))
const tabs = computed(() => tabIds.value.map((v) => ({ value: v, label: t(`nodes.tabs.${v}`) })))
const tab = computed({
  get: () => (tabIds.value.includes(String(route.query.tab)) ? String(route.query.tab) : 'overview'),
  set: (v) => router.replace({ query: { ...route.query, tab: v === 'overview' ? undefined : v } }),
})
const pct = (used: number, total: number) => (total ? (used / total) * 100 : null)
const node = ref<NodeInfo | null>(null)
const { setCrumbs } = useCrumbs()
watchEffect(() => {
  if (node.value) setCrumbs(route.path, [{ label: node.value.name, to: route.path }, { label: t(`nodes.tabs.${tab.value}`) }])
})
const vms = ref<VmInfo[]>([])
const name = ref('')
const error = ref('')
const busy = ref(false)
const confirmRegen = ref(false)
const command = ref('')
const confirmDelete = ref(false)
const updateOpen = ref(false)
const usageRange = ref<UsageRange | null>(null)
const hoverAt = ref<number | null>(null)
watch(tab, (v) => {
  if (v !== 'metrics') usageRange.value = hoverAt.value = null
})

const nodeLogFilter = { key: 'source' as const, options: ['observatory-node', 'firecracker'] }
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

const timer = window.setInterval(() => node.value?.updating && document.visibilityState === 'visible' && load(), 5000)
onBeforeUnmount(() => window.clearInterval(timer))

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
    <p v-if="error && !node" role="alert" class="load-error">{{ error }}</p>
    <template v-if="node">
      <PageHeader :icon="HardDrive" :title="node.name" mono :subtitle="`${node.cpus} vCPU · ${gb(node.memoryMB)} GB · ${node.version || '–'}`">
        <template #tags>
          <Tag :tone="stateTones[node.state]">{{ t(`nodes.states.${node.state}`) }}</Tag>
          <Tag v-if="node.quarantined" tone="danger">{{ t('nodes.quarantined') }}</Tag>
          <Tag v-if="node.updating" tone="warning">{{ t('nodes.updating') }}</Tag>
          <Tag v-if="node.outdated" tone="warning" :title="t('nodes.outdatedHint', { hub: me?.version })">{{ t('nodes.outdated') }}</Tag>
        </template>
        <Button v-if="node.canUpdate && node.updatable" @click="updateOpen = true">{{ t('nodes.update.node') }}</Button>
      </PageHeader>
      <div class="page-body">
        <p v-if="error" role="alert">{{ error }}</p>
        <p v-if="node.quarantined" role="alert">{{ t('nodes.quarantinedHelp') }}</p>
        <Tabs v-model="tab" :tabs="tabs" variant="underline" />

        <template v-if="tab === 'overview'">
          <Section v-if="node.state !== 'pending'" :label="t('nodes.allocated')">
            <div class="cards">
              <Card>
                <span class="muted">{{ t('nodes.cpu') }}</span>
                <strong>{{ node.usedCPUs }} / {{ node.cpus }}</strong>
                <Meter :label="t('usage.cpu')" :percent="pct(node.usedCPUs, node.cpus)" />
              </Card>
              <Card>
                <span class="muted">{{ t('nodes.memory') }}</span>
                <strong>{{ gb(node.usedMemoryMB) }} / {{ gb(node.memoryMB) }} GB</strong>
                <Meter :label="t('usage.mem')" :percent="pct(node.usedMemoryMB, node.memoryMB)" />
              </Card>
              <Card>
                <span class="muted">{{ t('nodes.vms') }}</span>
                <strong>{{ node.vmCount }}</strong>
              </Card>
            </div>
          </Section>
          <Section :label="t('nodes.machine')">
            <Card>
              <KeyValueList>
                <KeyValue :label="t('nodes.version')">
                  {{ node.version || '–' }}
                  <span v-if="node.outdated" class="muted"> · {{ t('nodes.outdatedHint', { hub: me?.version }) }}</span>
                </KeyValue>
                <KeyValue :label="t('nodes.guestImage')">
                  {{ imageLine(node.image, t, locale) }}
                </KeyValue>
                <KeyValue :label="t('nodes.lastSeen')">{{ node.lastSeen ? d(new Date(node.lastSeen), { dateStyle: 'short', timeStyle: 'short' }) : '–' }}</KeyValue>
                <template v-if="node.underlay">
                  <KeyValue :label="t('nodes.underlay')">{{ node.underlay.ip }}</KeyValue>
                  <KeyValue :label="t('nodes.registered')">{{ node.underlay.registered ? t('nodes.yes') : t('nodes.no') }}</KeyValue>
                  <KeyValue :label="t('nodes.handshake')">{{ node.underlay.lastHandshake ? d(new Date(node.underlay.lastHandshake), { dateStyle: 'short', timeStyle: 'short' }) : t('nodes.never') }}</KeyValue>
                </template>
                <KeyValue v-for="[k, v] in machine" :key="k" :label="k">{{ v }}</KeyValue>
              </KeyValueList>
            </Card>
          </Section>
        </template>

        <template v-if="tab === 'metrics'">
          <TelemetryUsage :url="`${base()}/usage`" :no-data="t('telemetry.noData')" @range="(r) => (usageRange = r)" @hover="(at) => (hoverAt = at)" />
          <RouterLink v-if="can('logs:read')" class="muted" :to="`/orgs/${org.id}/logs/node/${nodeId}`">{{ t('logs.openIn') }}</RouterLink>
          <TelemetryLogs v-if="usageRange" :url="`${base()}/logs`" :range="usageRange" :hover-at="hoverAt" :filter="nodeLogFilter" />
        </template>

        <VMList v-if="tab === 'vms'" :org-id="org.id" :vms="vms" :empty="t('nodes.noVms')" @changed="load" />

        <template v-if="tab === 'settings'">
          <Section v-if="node.canUpdate" :label="t('nodes.rename')">
            <Card>
              <form class="row" @submit.prevent="rename">
                <TextInput v-model="name" :aria-label="t('nodes.name')" />
                <Button type="submit" :disabled="busy || !name.trim() || name.trim() === node.name">{{ t('nodes.rename') }}</Button>
              </form>
            </Card>
          </Section>
          <Section v-if="node.canUpdate" :label="t('nodes.regenerateTitle')">
            <Card>
              <span class="muted">{{ t('nodes.regenerateBody') }}</span>
              <p><Button :disabled="busy" @click="confirmRegen = true">{{ t('nodes.regenerate') }}</Button></p>
            </Card>
          </Section>
          <Section v-if="node.canDelete" :label="t('nodes.deleteTitle')">
            <Card tone="danger">
              <span class="muted">{{ t('nodes.deleteBody', { name: node.name }) }}</span>
              <p><Button variant="danger" :disabled="busy" @click="confirmDelete = true">{{ t('nodes.delete') }}</Button></p>
            </Card>
          </Section>
        </template>
      </div>

      <UpdateNodesDialog v-model:open="updateOpen" :org-id="org.id" :nodes="[node]" @started="load" />
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
.load-error {
  margin: var(--page-y) var(--page-x);
}
.cards {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(200px, 1fr));
  gap: 16px;
}
.row {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}
</style>
