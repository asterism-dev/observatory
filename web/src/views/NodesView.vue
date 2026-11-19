<script setup lang="ts">
import { Button, Checkbox } from '@asterism-dev/lumen'
import { HardDrive, Plus } from '@lucide/vue'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api, ApiError } from '@/api'
import AddNodeDialog from '@/components/AddNodeDialog.vue'
import UpdateNodesDialog from '@/components/UpdateNodesDialog.vue'
import { bannerRun, runSummary, selectable } from '@/composables/nodeUpdates'
import { gb, imageLine, stateTones, type NodeInfo, type UpdateRun } from '@/composables/nodes'
import { useMe } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'
import { Meter, Page, ResourceList, ResourceRow, Tag } from '@/ui'

const { t, locale } = useI18n()
const { org, can } = useOrg()
const { me } = useMe()

const nodes = ref<NodeInfo[]>([])
const error = ref('')
const addOpen = ref(false)
const updateOpen = ref(false)
const run = ref<UpdateRun | null>(null)
const selected = ref<string[]>([])
const cancelling = ref(false)
const seenRunning = new Set<string>()

const runActive = computed(() => run.value?.state === 'running')
const selectableIds = computed(() => nodes.value.filter((n) => selectable(n, runActive.value)).map((n) => n.id))
const selectedNodes = computed(() => nodes.value.filter((n) => selected.value.includes(n.id)))
const allSelected = computed<boolean | 'indeterminate'>(() =>
  !selected.value.length ? false : selected.value.length === selectableIds.value.length ? true : 'indeterminate',
)
const summary = computed(() => (run.value ? runSummary(run.value) : null))
const failed = computed(() => run.value?.nodes.find((n) => n.status === 'failed'))

function toggle(id: string, on: boolean | 'indeterminate') {
  selected.value = on === true ? [...selected.value, id] : selected.value.filter((s) => s !== id)
}

const pct = (used: number, total: number) => (total ? (used / total) * 100 : null)

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const res = await api<NodeInfo[]>(`/api/orgs/${id}/nodes`)
    // Org switched while loading.
    if (id === org.value?.id) {
      nodes.value = res
      selected.value = selected.value.filter((s) => res.some((n) => n.id === s && n.updatable))
    }
  } catch (e) {
    if (id === org.value?.id) error.value = e instanceof Error ? e.message : String(e)
  }
}

async function refreshRun() {
  if (!org.value) return
  const id = org.value.id
  try {
    const res = await api<UpdateRun>(`/api/orgs/${id}/node-updates/current`)
    if (id !== org.value?.id) return
    if (res.state === 'running') seenRunning.add(res.id)
    run.value = bannerRun(res, seenRunning)
  } catch (e) {
    if (id !== org.value?.id) return
    if (e instanceof ApiError && e.status === 404) run.value = null
    else error.value = e instanceof Error ? e.message : String(e)
  }
}

const reload = () => Promise.all([refresh(), refreshRun()])

async function cancelRun() {
  cancelling.value = true
  try {
    await api(`/api/orgs/${org.value?.id}/node-updates/current/cancel`, { method: 'POST' })
    await reload()
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    cancelling.value = false
  }
}

const timer = window.setInterval(() => document.visibilityState === 'visible' && reload(), 5000)
onBeforeUnmount(() => window.clearInterval(timer))

watch(
  () => org.value?.id,
  () => {
    error.value = ''
    nodes.value = []
    run.value = null
    seenRunning.clear()
    selected.value = []
    reload()
  },
  { immediate: true },
)
</script>

<template>
  <Page v-if="org" :icon="HardDrive" :title="t('nav.nodes')" :subtitle="t('nodes.subtitle')">
    <template #actions>
      <Button v-if="can('node:update') && selectableIds.length" :disabled="!selected.length" @click="updateOpen = true">
        {{ t('nodes.update.selected', { n: selected.length }) }}
      </Button>
      <Button v-if="can('node:create')" variant="accent" :icon="Plus" @click="addOpen = true">{{ t('nodes.add') }}</Button>
    </template>
    <p v-if="error" role="alert">{{ error }}</p>
    <p v-if="run && summary" class="banner" role="status">
      <template v-if="run.state === 'running'">{{ t('nodes.update.running', summary) }}</template>
      <template v-else-if="run.state === 'failed'">
        {{ failed ? t('nodes.update.failed', { name: failed.name, reason: failed.reason }) : t('nodes.update.failedGeneric') }}
      </template>
      <template v-else>{{ t(`nodes.update.${run.state === 'cancelled' ? 'cancelled' : 'done'}`) }}</template>
      <Button v-if="run.state === 'running' && can('node:update')" :disabled="cancelling" @click="cancelRun">{{ t('nodes.update.cancel') }}</Button>
    </p>
    <div v-if="can('node:update') && selectableIds.length" class="select-all">
      <Checkbox :model-value="allSelected" :aria-label="t('nodes.update.selectAll')" @update:model-value="selected = allSelected === true ? [] : [...selectableIds]" />
    </div>
    <ResourceList :count="nodes.length" :empty="t('nodes.empty')">
      <ResourceRow
        v-for="n in nodes"
        :key="n.id"
        :title="n.name"
        :meta="`${n.state === 'pending' ? '–' : `${n.cpus} vCPU · ${gb(n.memoryMB)} GB`} · ${n.version || '–'}`"
        :to="`/orgs/${org.id}/nodes/${n.id}`"
        :tone="stateTones[n.state]"
      >
        <template #tags>
          <Tag :tone="stateTones[n.state]">{{ t(`nodes.states.${n.state}`) }}</Tag>
          <Tag v-if="n.quarantined" tone="danger">{{ t('nodes.quarantined') }}</Tag>
          <Tag v-if="n.updating" tone="warning">{{ t('nodes.updating') }}</Tag>
          <Tag v-if="n.outdated" tone="warning" :title="n.updatable ? t('nodes.outdatedHint', { hub: me?.version }) : t('nodes.update.manual')">{{ t('nodes.outdated') }}</Tag>
          <Tag>{{ n.vmCount }} {{ t('nodes.vms') }}</Tag>
        </template>
        <Meter :label="t('usage.cpu')" :percent="n.state === 'pending' ? null : pct(n.usedCPUs, n.cpus)" />
        <Meter :label="t('usage.mem')" :percent="n.state === 'pending' ? null : pct(n.usedMemoryMB, n.memoryMB)" />
        <span class="mono image">
          <i18n-t v-if="n.image?.state === 'custom'" keypath="nodes.image.custom" tag="span"><template #key><code>imageURL</code></template></i18n-t>
          <template v-else>{{ imageLine(n.image, t, locale) }}</template>
        </span>
        <template v-if="can('node:update') && selectableIds.length" #actions>
          <Checkbox
            v-if="selectable(n, runActive)"
            :model-value="selected.includes(n.id)"
            :aria-label="t('nodes.update.select', { name: n.name })"
            @update:model-value="(on) => toggle(n.id, on)"
          />
        </template>
      </ResourceRow>
    </ResourceList>
    <AddNodeDialog v-model:open="addOpen" :org-id="org.id" @created="refresh" />
    <UpdateNodesDialog v-model:open="updateOpen" :org-id="org.id" :nodes="selectedNodes" @started="selected = []; reload()" />
  </Page>
</template>

<style scoped>
.banner {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 12px;
  margin: 0 0 12px;
}
.select-all {
  display: flex;
  justify-content: flex-end;
  padding: 0 18px 8px;
}
.image {
  overflow: hidden;
  font-size: 0.72rem;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>
