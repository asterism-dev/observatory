<script setup lang="ts">
import { Button, Dialog, SegmentedControl, Select, TextInput } from '@asterism-dev/lumen'
import { ArrowDown, ArrowUp, ChartLine, Pencil, Plus, Trash2 } from '@lucide/vue'
import { computed, onBeforeUnmount, ref, toRaw, watch, watchEffect } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'

import { api, ApiError } from '@/api'
import DashboardPanel from '@/components/DashboardPanel.vue'
import PanelEditor from '@/components/PanelEditor.vue'
import { copyPanel, creatableTeams, dashboardScope, dashboardScopes, exampleQuery, nameValid, newPanel, panelKey, type Dashboard, type Panel, type Team } from '@/composables/dashboards'
import { scopeKey, scopeLabel, scopeRef } from '@/composables/envScopes'
import { presets, rangeFor, type Preset } from '@/composables/telemetry'
import { useCrumbs } from '@/composables/useCrumbs'
import { useMe } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'
import { EmptyState, PageHeader, Tag } from '@/ui'

const { t } = useI18n()
const { org, can } = useOrg()
const { me } = useMe()
const route = useRoute()
const router = useRouter()
const { setCrumbs } = useCrumbs()

const dash = ref<Dashboard | null>(null)
const teams = ref<Team[]>([])
const ownTeams = ref<Team[]>([])
const error = ref('')
const preset = ref<Preset>('6h')
const zoomed = ref<{ from: Date; to: Date } | null>(null)
const range = ref(rangeFor('6h', new Date()))
const editing = ref(false)
const draft = ref<{ name: string; scope: string; panels: Panel[] }>({ name: '', scope: '', panels: [] })
const editorOpen = ref(false)
const editIndex = ref<number | null>(null)
const editorPanel = ref<Panel | null>(null)
const busy = ref(false)
const saveError = ref('')
const conflict = ref(false)
const deleteOpen = ref(false)

const id = computed(() => String(route.params.dashboardId))
const presetOptions = presets.map((p) => ({ value: p, label: p.replace(/(\d+)/, '$1 ') }))
const panels = computed(() => (editing.value ? draft.value.panels : (dash.value?.panels ?? [])))
const scopes = computed(() => {
  if (!dash.value) return []
  const list = dashboardScopes(ownTeams.value, can('dashboard:create'), dash.value.createdBy === me.value?.user.id)
  const current = dashboardScope(dash.value, teams.value)
  return list.some((s) => scopeKey(s) === scopeKey(current)) ? list : [current, ...list]
})
const scopeOptions = computed(() => scopes.value.map((s) => ({ value: scopeKey(s), label: scopeLabel(s, t) })))

function setRange() {
  range.value = zoomed.value ?? rangeFor(preset.value, new Date())
}
watch([preset, zoomed], setRange)

function pick(p: Preset | '') {
  if (p) preset.value = p
  zoomed.value = null
}

async function load() {
  if (!org.value) return
  const orgId = org.value.id
  const want = id.value
  try {
    const [d, ts] = await Promise.all([api<Dashboard>(`/api/orgs/${orgId}/dashboards/${want}`), api<Team[]>(`/api/orgs/${orgId}/teams`).catch(() => [])])
    if (want !== id.value) return
    dash.value = d
    teams.value = ts
    error.value = ''
    if (route.query.edit && d.canEdit && !editing.value) startEdit()
  } catch (e) {
    if (want === id.value) error.value = e instanceof Error ? e.message : String(e)
  }
}

async function startEdit() {
  const d = dash.value
  if (!d || !org.value || !me.value) return
  draft.value = { name: d.name, scope: scopeKey(dashboardScope(d, teams.value)), panels: d.panels.map((p) => copyPanel(toRaw(p))) }
  saveError.value = ''
  conflict.value = false
  editing.value = true
  ownTeams.value = await creatableTeams(api, org.value.id, me.value.user.id, can('dashboard:create'), teams.value)
}

function stopEdit() {
  editing.value = false
  if (route.query.edit) router.replace({ query: {} })
}

function openEditor(i: number | null, p?: Panel) {
  editIndex.value = i
  editorPanel.value = i === null ? (p ?? null) : (draft.value.panels[i] ?? null)
  editorOpen.value = true
}

function applyPanel(p: Panel) {
  if (editIndex.value === null) draft.value.panels.push(p)
  else draft.value.panels[editIndex.value] = p
}

function move(i: number, by: number) {
  const ps = draft.value.panels
  const [p] = ps.splice(i, 1)
  if (p) ps.splice(i + by, 0, p)
}

async function addExample() {
  await startEdit()
  openEditor(null, { ...newPanel(exampleQuery), title: t('dashboards.exampleTitle') })
}

async function save() {
  const d = dash.value
  const target = scopes.value.find((s) => scopeKey(s) === draft.value.scope)
  if (!d || !target || !org.value) return
  busy.value = true
  saveError.value = ''
  try {
    dash.value = await api<Dashboard>(`/api/orgs/${org.value.id}/dashboards/${d.id}`, {
      method: 'PUT',
      body: JSON.stringify({ name: draft.value.name.trim(), panels: draft.value.panels, updatedAt: d.updatedAt, ...scopeRef(target) }),
    })
    stopEdit()
  } catch (e) {
    if (e instanceof ApiError && e.status === 409) conflict.value = true
    else saveError.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}

async function reload() {
  conflict.value = false
  stopEdit()
  await load()
}

// Debounced: synced charts may each report a zoom, and quick re-zooms shouldn't re-query every panel each time.
let zoomTimer = 0
function zoomTo(a: number, b: number) {
  window.clearTimeout(zoomTimer)
  zoomTimer = window.setTimeout(() => (zoomed.value = { from: new Date(a * 1000), to: new Date(b * 1000) }), 300)
}

async function remove() {
  if (!dash.value || !org.value) return
  busy.value = true
  try {
    await api(`/api/orgs/${org.value.id}/dashboards/${dash.value.id}`, { method: 'DELETE' })
    router.push(`/orgs/${org.value.id}/dashboards`)
  } catch (e) {
    saveError.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
    deleteOpen.value = false
  }
}

watchEffect(() => {
  if (dash.value) setCrumbs(route.path, [{ label: dash.value.name }])
})

watch(
  () => [org.value?.id, id.value],
  () => {
    dash.value = null
    editing.value = false
    zoomed.value = null
    setRange()
    load()
  },
  { immediate: true },
)

// Moving ranges follow now; a zoomed range and an open editor stay put.
const refresh = () => document.visibilityState === 'visible' && !zoomed.value && !editing.value && setRange()
const timer = window.setInterval(refresh, 30000)
document.addEventListener('visibilitychange', refresh)
onBeforeUnmount(() => {
  window.clearInterval(timer)
  window.clearTimeout(zoomTimer)
  document.removeEventListener('visibilitychange', refresh)
})
</script>

<template>
  <template v-if="org">
    <p v-if="error && !dash" role="alert" class="load-error">{{ error }}</p>
    <template v-if="dash">
      <PageHeader :icon="ChartLine" :title="dash.name">
        <template #tags>
          <Tag>{{ scopeLabel(dashboardScope(dash, teams), t) }}</Tag>
        </template>
        <Button v-if="dash.canEdit && !editing" :icon="Pencil" @click="startEdit">{{ t('dashboards.edit') }}</Button>
      </PageHeader>
      <div class="page-body">
        <div v-if="editing" class="editbar">
          <label>
            {{ t('dashboards.name') }}
            <TextInput v-model="draft.name" :aria-label="t('dashboards.name')" :invalid="!nameValid(draft.name)" />
          </label>
          <label>
            {{ t('dashboards.visibility') }}
            <Select v-model="draft.scope" :options="scopeOptions" :aria-label="t('dashboards.visibility')" />
          </label>
          <div class="actions">
            <Button :icon="Plus" :disabled="draft.panels.length >= 20" @click="openEditor(null)">{{ t('dashboards.addPanel') }}</Button>
            <Button variant="danger" :icon="Trash2" :disabled="busy" @click="deleteOpen = true">{{ t('dashboards.delete') }}</Button>
            <Button :disabled="busy" @click="stopEdit">{{ t('profile.cancel') }}</Button>
            <Button variant="accent" :disabled="busy || !nameValid(draft.name)" @click="save">{{ t('dashboards.save') }}</Button>
          </div>
        </div>
        <div v-if="conflict" role="alert" class="conflict">
          <span>{{ t('dashboards.conflict') }}</span>
          <Button size="sm" @click="reload">{{ t('dashboards.reload') }}</Button>
        </div>
        <p v-if="saveError" role="alert">{{ saveError }}</p>
        <div class="toolbar">
          <span class="muted small">{{ t('telemetry.zoomHint') }}</span>
          <SegmentedControl :model-value="zoomed ? '' : preset" :options="presetOptions" size="sm" @update:model-value="pick" />
        </div>
        <EmptyState v-if="!panels.length" :title="t('dashboards.noPanels')" :text="t('dashboards.noPanelsText')">
          <div class="example">
            <code class="mono">{{ exampleQuery }}</code>
            <div v-if="dash.canEdit">
              <Button variant="accent" :icon="Plus" @click="addExample">{{ t('dashboards.addExample') }}</Button>
            </div>
          </div>
        </EmptyState>
        <div class="grid">
          <div v-for="(p, i) in panels" :key="panelKey(toRaw(p))" :class="['cell', p.width]">
            <div v-if="editing" class="cell-tools">
              <Button size="sm" variant="ghost" :icon="ArrowUp" :disabled="i === 0" :aria-label="t('dashboards.moveUp')" :title="t('dashboards.moveUp')" @click="move(i, -1)" />
              <Button
                size="sm"
                variant="ghost"
                :icon="ArrowDown"
                :disabled="i === panels.length - 1"
                :aria-label="t('dashboards.moveDown')"
                :title="t('dashboards.moveDown')"
                @click="move(i, 1)"
              />
              <Button size="sm" variant="ghost" :icon="Pencil" :aria-label="t('dashboards.editPanel')" :title="t('dashboards.editPanel')" @click="openEditor(i)" />
              <Button size="sm" variant="ghost" :icon="Trash2" :aria-label="t('dashboards.removePanel')" :title="t('dashboards.removePanel')" @click="draft.panels.splice(i, 1)" />
            </div>
            <DashboardPanel
              :org-id="org.id"
              :panel="p"
              :from="range.from"
              :to="range.to"
              :sync-key="`dashboard:${dash.id}`"
              @zoom="zoomTo"
              @reset="zoomed = null"
            />
          </div>
        </div>
      </div>
      <PanelEditor v-model:open="editorOpen" :org-id="org.id" :panel="editorPanel" :adding="editIndex === null" :from="range.from" :to="range.to" @save="applyPanel" />
      <Dialog :open="deleteOpen" :title="t('dashboards.deleteTitle')" @update:open="(o) => !busy && (deleteOpen = o)">
        <p>{{ t('dashboards.deleteText', { name: dash.name }) }}</p>
        <template #actions>
          <Button :disabled="busy" @click="deleteOpen = false">{{ t('profile.cancel') }}</Button>
          <Button variant="danger" :disabled="busy" @click="remove">{{ t('dashboards.delete') }}</Button>
        </template>
      </Dialog>
    </template>
  </template>
</template>

<style scoped>
.editbar {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(min(220px, 100%), 1fr));
  align-items: end;
  gap: 8px;
}
.editbar label {
  display: grid;
  gap: 4px;
  min-width: 0;
}
.actions {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  grid-column: 1 / -1;
}
.conflict {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  color: var(--warning);
}
.toolbar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}
.small {
  font-size: 0.75rem;
}
.example {
  display: grid;
  gap: 8px;
  min-width: 0;
}
.example code {
  font-size: 0.75rem;
  overflow-wrap: anywhere;
}
.grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 12px;
}
.cell {
  display: grid;
  gap: 4px;
  min-width: 0;
  align-content: start;
}
.cell.full {
  grid-column: 1 / -1;
}
.cell-tools {
  display: flex;
  justify-content: flex-end;
  gap: 2px;
}
@media (max-width: 760px) {
  .grid {
    grid-template-columns: minmax(0, 1fr);
  }
}
p {
  margin: 0;
}
</style>
