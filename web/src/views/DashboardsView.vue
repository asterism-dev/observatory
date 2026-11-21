<script setup lang="ts">
import { Button, Dialog, Select, Tabs, TextInput } from '@asterism-dev/lumen'
import { ChartLine, Plus } from '@lucide/vue'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'

import { api } from '@/api'
import { creatableTeams, dashboardScope, dashboardScopes, nameValid, type Dashboard, type Team } from '@/composables/dashboards'
import { scopeKey, scopeLabel, scopeRef } from '@/composables/envScopes'
import { useMe } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'
import { EmptyState, FilterBar, Page, ResourceList, ResourceRow, Tag } from '@/ui'

const { t } = useI18n()
const { org, can } = useOrg()
const { me } = useMe()
const router = useRouter()

const items = ref<Dashboard[]>([])
const teams = ref<Team[]>([])
const tab = ref('all')
const search = ref('')
const error = ref('')
const loaded = ref(false)
const createOpen = ref(false)
const name = ref('')
const scope = ref('user')
const own = ref<Team[]>([])
const busy = ref(false)
const createError = ref('')

const tabs = computed(() => ['all', 'org', 'teams', 'private'].map((v) => ({ value: v, label: t(`env.tab.${v}`) })))
const rows = computed(() => {
  const s = { all: null, org: 'org', teams: 'team', private: 'user' }[tab.value]
  const q = search.value.trim().toLowerCase()
  return items.value.filter((d) => (!s || d.scope === s) && (!q || d.name.toLowerCase().includes(q)))
})
const scopes = computed(() => dashboardScopes(own.value, can('telemetry.dashboards.create'), true))
const scopeOptions = computed(() => scopes.value.map((s) => ({ value: scopeKey(s), label: scopeLabel(s, t) })))

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const [list, ts] = await Promise.all([api<Dashboard[]>(`/api/orgs/${id}/dashboards`), api<Team[]>(`/api/orgs/${id}/teams`).catch(() => [])])
    if (id !== org.value?.id) return
    items.value = list
    teams.value = ts
    loaded.value = true
  } catch (e) {
    if (id === org.value?.id) error.value = e instanceof Error ? e.message : String(e)
  }
}

async function openCreate() {
  name.value = ''
  scope.value = 'user'
  createError.value = ''
  createOpen.value = true
  if (org.value && me.value) own.value = await creatableTeams(api, org.value.id, me.value.user.id, can('telemetry.dashboards.create'), teams.value)
}

async function create() {
  const target = scopes.value.find((s) => scopeKey(s) === scope.value)
  if (!target || !org.value) return
  busy.value = true
  createError.value = ''
  try {
    const d = await api<Dashboard>(`/api/orgs/${org.value.id}/dashboards`, {
      method: 'POST',
      body: JSON.stringify({ name: name.value.trim(), panels: [], ...scopeRef(target) }),
    })
    createOpen.value = false
    router.push({ path: `/orgs/${org.value.id}/dashboards/${d.id}`, query: { edit: '1' } })
  } catch (e) {
    createError.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}

watch(
  () => org.value?.id,
  () => {
    error.value = ''
    items.value = []
    loaded.value = false
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <Page v-if="org" :icon="ChartLine" :title="t('nav.dashboards')" :subtitle="t('dashboards.subtitle')">
    <template #actions>
      <Button variant="accent" :icon="Plus" @click="openCreate">{{ t('dashboards.create') }}</Button>
    </template>
    <p v-if="error" role="alert">{{ error }}</p>
    <EmptyState v-if="loaded && !items.length" :label="t('nav.dashboards')" :title="t('dashboards.empty')" :text="t('dashboards.emptyText')">
      <Button variant="accent" :icon="Plus" @click="openCreate">{{ t('dashboards.create') }}</Button>
    </EmptyState>
    <template v-else>
      <FilterBar>
        <Tabs v-model="tab" :tabs="tabs" variant="underline" />
        <template #end>
          <TextInput v-model="search" type="search" :placeholder="t('dashboards.search')" :aria-label="t('dashboards.search')" />
        </template>
      </FilterBar>
      <ResourceList :count="rows.length" :empty="t('dashboards.noMatch')">
        <ResourceRow v-for="d in rows" :key="d.id" :title="d.name" :to="`/orgs/${org.id}/dashboards/${d.id}`">
          <template #tags>
            <Tag>{{ scopeLabel(dashboardScope(d, teams), t) }}</Tag>
          </template>
          <span>{{ t('dashboards.panelCount', { n: d.panels.length }, d.panels.length) }}</span>
        </ResourceRow>
      </ResourceList>
    </template>
    <Dialog :open="createOpen" :title="t('dashboards.createTitle')" @update:open="(o) => !busy && (createOpen = o)">
      <div class="form">
        <label>
          {{ t('dashboards.name') }}
          <TextInput v-model="name" :aria-label="t('dashboards.name')" :invalid="!!name && !nameValid(name)" />
        </label>
        <label>
          {{ t('dashboards.visibility') }}
          <Select v-model="scope" :options="scopeOptions" :aria-label="t('dashboards.visibility')" />
        </label>
      </div>
      <p v-if="createError" role="alert">{{ createError }}</p>
      <template #actions>
        <Button :disabled="busy" @click="createOpen = false">{{ t('profile.cancel') }}</Button>
        <Button variant="accent" :disabled="busy || !nameValid(name)" @click="create">{{ t('dashboards.create') }}</Button>
      </template>
    </Dialog>
  </Page>
</template>

<style scoped>
.form {
  display: grid;
  gap: 8px;
}
label {
  display: grid;
  gap: 4px;
}
</style>
