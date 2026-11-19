<script setup lang="ts">
import { Button } from '@asterism-dev/lumen'
import { HardDrive, Plus } from '@lucide/vue'
import { onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import AddNodeDialog from '@/components/AddNodeDialog.vue'
import { gb, imageLine, stateTones, type NodeInfo } from '@/composables/nodes'
import { useOrg } from '@/composables/useOrg'
import { Meter, Page, ResourceList, ResourceRow, Tag } from '@/ui'

const { t, locale } = useI18n()
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
  <Page v-if="org" :icon="HardDrive" :title="t('nav.nodes')" :subtitle="t('nodes.subtitle')">
    <template #actions>
      <Button v-if="can('node:create')" variant="accent" :icon="Plus" @click="addOpen = true">{{ t('nodes.add') }}</Button>
    </template>
    <p v-if="error" role="alert">{{ error }}</p>
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
          <Tag>{{ n.vmCount }} {{ t('nodes.vms') }}</Tag>
        </template>
        <Meter :label="t('usage.cpu')" :percent="n.state === 'pending' ? null : pct(n.usedCPUs, n.cpus)" />
        <Meter :label="t('usage.mem')" :percent="n.state === 'pending' ? null : pct(n.usedMemoryMB, n.memoryMB)" />
        <span class="mono image">
          <i18n-t v-if="n.image?.state === 'custom'" keypath="nodes.image.custom" tag="span"><template #key><code>imageURL</code></template></i18n-t>
          <template v-else>{{ imageLine(n.image, t, locale) }}</template>
        </span>
      </ResourceRow>
    </ResourceList>
    <AddNodeDialog v-model:open="addOpen" :org-id="org.id" @created="refresh" />
  </Page>
</template>

<style scoped>
.image {
  overflow: hidden;
  font-size: 0.72rem;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>
