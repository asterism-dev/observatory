<script setup lang="ts">
import { Button, Checkbox, Select, Tabs, Textarea, TextInput } from '@asterism-dev/lumen'
import { Play, ScrollText } from '@lucide/vue'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'

import { api } from '@/api'
import LogQueryResult from '@/components/LogQueryResult.vue'
import { filterGroups, groupKey, groupKinds, groupName, groupPath, queryBody, retentionSpan, type GroupKind, type LogGroup, type LogGroupList, type LogQueryResult as QueryResult } from '@/composables/logs'
import { presets, rangeFor, type Preset } from '@/composables/telemetry'
import { useOrg } from '@/composables/useOrg'
import { Card, EmptyState, FilterBar, Page, ResourceList, ResourceRow, Section, Tag } from '@/ui'

const { t, d } = useI18n()
const { org, can } = useOrg()
const route = useRoute()
const router = useRouter()

const tabs = computed(() => ['groups', 'query'].map((v) => ({ value: v, label: t(`logs.tab.${v}`) })))
const tab = computed({
  get: () => (route.query.tab === 'query' ? 'query' : 'groups'),
  set: (v) => router.replace({ query: { ...route.query, tab: v === 'groups' ? undefined : v } }),
})
const groups = ref<LogGroup[]>([])
const retention = ref(0)
const loaded = ref(false)
const error = ref('')
const kind = ref<GroupKind | 'all'>('all')
const search = ref('')

const kindTabs = computed(() => ['all', ...groupKinds].map((v) => ({ value: v, label: t(`logs.kind.${v}`) })))
const rows = computed(() => filterGroups(groups.value, kind.value, search.value))
const keptFor = computed(() => {
  const { unit, n } = retentionSpan(retention.value)
  return t('logs.retention', { for: t(`logs.${unit}`, { n }, n) })
})
const when = (iso: string) => d(new Date(iso), { dateStyle: 'short', timeStyle: 'medium' })

const picked = ref(new Set<string>())
const pickFilter = ref('')
const preset = ref<Preset>('1h')
const query = ref('')
const running = ref(false)
const queryError = ref('')
const result = ref<QueryResult | null>(null)

const presetOptions = presets.map((p) => ({ value: p, label: p }))
const pickable = computed(() => filterGroups(groups.value, 'all', pickFilter.value))

function toggle(g: LogGroup) {
  const next = new Set(picked.value)
  if (!next.delete(groupKey(g))) next.add(groupKey(g))
  picked.value = next
}

async function run() {
  if (!org.value) return
  running.value = true
  queryError.value = ''
  try {
    const chosen = groups.value.filter((g) => picked.value.has(groupKey(g)))
    result.value = await api<QueryResult>(`/api/orgs/${org.value.id}/logs/query`, {
      method: 'POST',
      body: JSON.stringify(queryBody(query.value, chosen, rangeFor(preset.value, new Date()))),
    })
  } catch (e) {
    result.value = null
    queryError.value = e instanceof Error ? e.message : String(e)
  } finally {
    running.value = false
  }
}

watch(
  () => org.value?.id,
  async (id) => {
    groups.value = []
    loaded.value = false
    error.value = ''
    if (!id || !can('logs:read')) return
    try {
      const list = await api<LogGroupList>(`/api/orgs/${id}/logs/groups`)
      if (id === org.value?.id) {
        groups.value = list.groups
        retention.value = list.retentionSeconds
      }
    } catch (e) {
      if (id === org.value?.id) error.value = e instanceof Error ? e.message : String(e)
    }
    loaded.value = true
  },
  { immediate: true },
)
</script>

<template>
  <Page v-if="org" :icon="ScrollText" :title="t('nav.logs')" :subtitle="t('logs.subtitle')">
    <EmptyState v-if="!can('logs:read')" :label="t('nav.logs')" :title="t('logs.forbidden')" :text="t('logs.forbiddenText')" />
    <template v-else>
      <Tabs v-model="tab" :tabs="tabs" variant="underline" />
      <p v-if="error" role="alert">{{ error }}</p>
      <template v-if="tab === 'groups'">
        <EmptyState v-if="loaded && !groups.length && !error" :label="t('nav.logs')" :title="t('logs.empty')" :text="t('logs.emptyText')" />
        <template v-else>
          <FilterBar>
            <Tabs v-model="kind" :tabs="kindTabs" variant="underline" />
            <template #end>
              <TextInput v-model="search" type="search" :placeholder="t('logs.search')" :aria-label="t('logs.search')" />
            </template>
          </FilterBar>
          <ResourceList :count="rows.length" :empty="t('logs.noMatch')">
            <ResourceRow v-for="g in rows" :key="groupKey(g)" :title="groupName(g)" :to="groupPath(org.id, g)">
              <template #tags>
                <Tag>{{ t(`logs.kind.${g.kind}`) }}</Tag>
                <Tag v-if="g.deleted" tone="warning">{{ t('logs.deleted') }}</Tag>
              </template>
              <span>{{ t('logs.streamCount', { n: g.streams }, g.streams) }}</span>
              <span>{{ t('logs.lastEntry', { at: when(g.lastAt) }) }}</span>
            </ResourceRow>
          </ResourceList>
        </template>
      </template>
      <template v-else>
        <Section :label="t('logs.groupsPick')">
          <Card>
            <TextInput v-model="pickFilter" type="search" :placeholder="t('logs.search')" :aria-label="t('logs.search')" />
            <div class="pick">
              <Checkbox v-for="g in pickable" :key="groupKey(g)" :model-value="picked.has(groupKey(g))" @update:model-value="toggle(g)">
                {{ groupName(g) }}<template v-if="g.deleted"> ({{ t('logs.deleted') }})</template>
              </Checkbox>
            </div>
            <span class="muted">{{ picked.size ? t('logs.picked', { n: picked.size }) : t('logs.pickedNone') }}</span>
          </Card>
        </Section>
        <Section :label="t('logs.query')">
          <Card>
            <Textarea v-model="query" class="mono" :rows="3" placeholder="error" :aria-label="t('logs.query')" />
            <div class="run">
              <Select v-model="preset" :options="presetOptions" :aria-label="t('logs.range')" />
              <Button variant="accent" :icon="Play" :disabled="running" @click="run">{{ t('logs.run') }}</Button>
            </div>
            <span class="muted">{{ keptFor }}</span>
            <p v-if="queryError" role="alert">{{ queryError }}</p>
          </Card>
        </Section>
        <Section v-if="result" :label="t('logs.result', { n: result.rows.length })">
          <LogQueryResult :result="result" />
        </Section>
      </template>
      <p v-if="tab === 'groups'" class="muted">{{ keptFor }}</p>
    </template>
  </Page>
</template>

<style scoped>
.pick {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(220px, 1fr));
  gap: 6px;
  max-height: 200px;
  overflow: auto;
}
.run {
  display: flex;
  gap: 8px;
  align-items: center;
}
.run > :first-child {
  width: auto;
}
p {
  margin: 0;
}
</style>
