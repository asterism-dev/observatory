<script setup lang="ts">
import { Badge, Button } from '@asterism-dev/lumen'
import { HardDrive, Plus } from '@lucide/vue'
import { onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'

import { api } from '@/api'
import AddNodeDialog from '@/components/AddNodeDialog.vue'
import Meter from '@/components/Meter.vue'
import PageHead from '@/components/PageHead.vue'
import { gb, imageLine, stateTones, type NodeInfo } from '@/composables/nodes'
import { useOrg } from '@/composables/useOrg'

const { t, locale } = useI18n()
const router = useRouter()
const { org, can } = useOrg()

const nodes = ref<NodeInfo[]>([])
const error = ref('')
const addOpen = ref(false)

const pct = (used: number, total: number) => (total ? (used / total) * 100 : null)

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const res = await api<NodeInfo[]>(`/api/orgs/${id}/nodes`)
    // Org switched while loading.
    if (id === org.value?.id) nodes.value = res
  } catch (e) {
    if (id === org.value?.id) error.value = e instanceof Error ? e.message : String(e)
  }
}

const timer = window.setInterval(() => document.visibilityState === 'visible' && refresh(), 5000)
onBeforeUnmount(() => window.clearInterval(timer))

watch(
  () => org.value?.id,
  () => {
    error.value = ''
    nodes.value = []
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <PageHead :icon="HardDrive" :title="t('nav.nodes')" :subtitle="t('nodes.subtitle')">
      <Button v-if="can('node:create')" variant="accent" :icon="Plus" @click="addOpen = true">{{ t('nodes.add') }}</Button>
    </PageHead>
    <p v-if="error" role="alert">{{ error }}</p>
    <ul class="rows">
      <li v-for="n in nodes" :key="n.id" class="node" @click="router.push(`/orgs/${org.id}/nodes/${n.id}`)">
        <span class="dot" :class="stateTones[n.state]" />
        <div class="ident">
          <router-link :to="`/orgs/${org.id}/nodes/${n.id}`" class="name" @click.stop>{{ n.name }}</router-link>
          <span class="mono muted">{{ n.state === 'pending' ? '–' : `${n.cpus} vCPU · ${gb(n.memoryMB)} GB` }} · {{ n.version || '–' }}</span>
        </div>
        <span class="badges">
          <Badge :tone="stateTones[n.state]">{{ t(`nodes.states.${n.state}`) }}</Badge>
          <Badge v-if="n.quarantined" tone="danger">{{ t('nodes.quarantined') }}</Badge>
          <Badge variant="outline">{{ n.vmCount }} {{ t('nodes.vms') }}</Badge>
        </span>
        <span class="mono muted image">
          <i18n-t v-if="n.image?.state === 'custom'" keypath="nodes.image.custom" tag="span"><template #key><code>imageURL</code></template></i18n-t>
          <template v-else>{{ imageLine(n.image, t, locale) }}</template>
        </span>
        <span class="meters">
          <Meter :label="t('usage.cpu')" :percent="n.state === 'pending' ? null : pct(n.usedCPUs, n.cpus)" />
          <Meter :label="t('usage.mem')" :percent="n.state === 'pending' ? null : pct(n.usedMemoryMB, n.memoryMB)" />
        </span>
      </li>
      <li v-if="!nodes.length" class="empty">{{ t('nodes.empty') }}</li>
    </ul>
    <AddNodeDialog v-model:open="addOpen" :org-id="org.id" @created="refresh" />
  </template>
</template>

<style scoped>
.rows {
  list-style: none;
  margin: 0;
  padding: 0;
  display: grid;
  gap: 8px;
}
.node {
  display: grid;
  grid-template-columns: auto minmax(140px, 1fr) auto minmax(0, 1.2fr) auto;
  align-items: center;
  gap: 16px;
  padding: 12px 16px;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  background: var(--panel);
  cursor: pointer;
}
.node:hover {
  border-color: var(--accent);
}
.dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: var(--muted);
}
.dot.success {
  background: var(--success);
}
.dot.warning {
  background: var(--warning);
}
.dot.danger {
  background: var(--danger);
}
.ident {
  display: flex;
  flex-direction: column;
  min-width: 0;
}
.name {
  color: var(--text);
  font-family: var(--font-mono, ui-monospace, monospace);
  font-weight: 600;
  text-decoration: none;
}
.mono {
  font-family: var(--font-mono, ui-monospace, monospace);
  font-size: 0.75rem;
}
.muted {
  color: var(--muted);
}
.image {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.badges,
.meters {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px 16px;
}
.badges {
  gap: 6px;
}
.empty {
  padding: 24px;
  text-align: center;
  color: var(--muted);
  border: 1px dashed var(--border);
  border-radius: var(--radius);
}
@media (max-width: 900px) {
  .node {
    grid-template-columns: auto 1fr;
  }
  .node > :nth-child(n + 3) {
    grid-column: 2;
    justify-self: start;
  }
}
</style>
