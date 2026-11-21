<script setup lang="ts">
import type { TableColumn } from '@asterism-dev/lumen'
import { Select } from '@asterism-dev/lumen'
import { ScrollText } from '@lucide/vue'
import { computed, ref, watch, watchEffect } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute } from 'vue-router'

import { api } from '@/api'
import TelemetryLogs from '@/components/TelemetryLogs.vue'
import { entriesUrl, groupName, logRange, rangeOptions, retentionSpan, type GroupKind, type LogGroup, type LogGroupList, type LogStream } from '@/composables/logs'
import { presets, rangeFor, type UsageRange } from '@/composables/telemetry'
import { useCrumbs } from '@/composables/useCrumbs'
import { useOrg } from '@/composables/useOrg'
import { DataTable, EmptyState, Page, Section, Tag } from '@/ui'

const { t, d } = useI18n()
const { org, can } = useOrg()
const route = useRoute()
const { setCrumbs } = useCrumbs()

const kind = computed(() => String(route.params.kind) as GroupKind)
const id = computed(() => String(route.params.id))
const group = ref<LogGroup | null>(null)
const streams = ref<LogStream[]>([])
const retention = ref(0)
const error = ref('')
const preset = ref('1h')
const range = ref<UsageRange>({ ...rangeFor('1h', new Date()), now: true })

const title = computed(() => (group.value ? groupName(group.value) : ''))
const filter = computed(() => ({ key: 'stream' as const, options: streams.value.map((s) => s.name) }))
const retentionLabel = computed(() => {
  const { unit, n } = retentionSpan(retention.value)
  return t(`logs.${unit}`, { n }, n)
})
const keptFor = computed(() => (retention.value ? t('logs.retention', { for: retentionLabel.value }) : ''))
const presetOptions = computed(() => rangeOptions(presets, retention.value, retentionLabel.value))
const when = (iso: string) => d(new Date(iso), { dateStyle: 'short', timeStyle: 'medium' })
const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('logs.stream') },
  { key: 'lastAt', label: t('logs.lastEntryLabel') },
  ...(kind.value === 'image' ? [{ key: 'build', label: t('logs.build') }] : []),
])

watch(preset, (p) => (range.value = { ...logRange(p, retention.value, new Date(), rangeFor), now: true }))
watchEffect(() => {
  if (group.value && org.value) setCrumbs(route.path, [{ label: title.value }])
})

watch(
  [() => org.value?.id, kind, id],
  async ([orgId, k, i]) => {
    group.value = null
    streams.value = []
    error.value = ''
    if (!orgId || !can('telemetry.logEntries.list')) return
    try {
      const [list, ss] = await Promise.all([
        api<LogGroupList>(`/api/orgs/${orgId}/logs/groups?kind=${k}`),
        api<LogStream[]>(`/api/orgs/${orgId}/logs/groups/${k}/${i}/streams`),
      ])
      if (orgId !== org.value?.id || k !== kind.value || i !== id.value) return
      group.value = list.groups.find((g) => g.id === i) ?? null
      retention.value = list.retentionSeconds
      streams.value = ss
      if (!group.value) error.value = t('logs.groupGone')
    } catch (e) {
      if (orgId === org.value?.id && k === kind.value && i === id.value) error.value = e instanceof Error ? e.message : String(e)
    }
  },
  { immediate: true },
)
</script>

<template>
  <Page v-if="org" :icon="ScrollText" :title="title || t('nav.logs')" mono>
    <template v-if="group" #tags>
      <Tag>{{ t(`logs.kind.${group.kind}`) }}</Tag>
      <Tag v-if="group.deleted" tone="warning">{{ t('logs.deleted') }}</Tag>
    </template>
    <EmptyState v-if="!can('telemetry.logEntries.list')" :label="t('nav.logs')" :title="t('logs.forbidden')" :text="t('logs.forbiddenText')" />
    <template v-else>
      <p v-if="error" role="alert">{{ error }}</p>
      <template v-if="group">
        <Section :label="t('logs.streams')">
          <DataTable :columns="columns" :rows="streams as unknown as Record<string, unknown>[]" row-key="name">
            <template #cell-lastAt="{ row }">{{ when(row.lastAt as string) }}</template>
            <template #cell-build="{ row }">
              <template v-if="(row as LogStream).build">
                {{ (row as LogStream).build!.startedAt ? when((row as LogStream).build!.startedAt!) : '' }} · {{ (row as LogStream).build!.status }}
              </template>
              <span v-else class="muted">–</span>
            </template>
            <template #empty>{{ t('logs.noStreams') }}</template>
          </DataTable>
        </Section>
        <div class="range">
          <Select v-model="preset" :options="presetOptions" :aria-label="t('logs.range')" />
        </div>
        <TelemetryLogs :url="entriesUrl(org.id, group)" :range="range" :hover-at="null" :filter="filter" />
        <p v-if="keptFor" class="muted">{{ keptFor }}</p>
      </template>
    </template>
  </Page>
</template>

<style scoped>
.range > * {
  width: auto;
}
p {
  margin: 0;
}
</style>
