<script setup lang="ts">
import { Button, Checkbox, Dialog, Select, Tabs, Textarea, TextInput, type TableColumn } from '@asterism-dev/lumen'
import { Hammer, Layers, X } from '@lucide/vue'
import { computed, onBeforeUnmount, ref, watch, watchEffect } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'

import { api, ApiError } from '@/api'
import { iamClient } from '@/composables/iamClient'
import BuildLog from '@/components/BuildLog.vue'
import RecipeEditor from '@/components/RecipeEditor.vue'
import { scopeKey, scopeLabel, scopeRef } from '@/composables/envScopes'
import {
  buildActive,
  buildDuration,
  autoSettingsBody,
  buildTones,
  formatBytes,
  imageScope,
  imageScopes,
  shortDigest,
  type Build,
  type Image,
  type ImageVersion,
} from '@/composables/images'
import { useCrumbs } from '@/composables/useCrumbs'
import { useMe } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'
import { Card, CommandBox, CopyButton, DataTable, KeyValue, KeyValueList, PageHeader, Section, Tag } from '@/ui'

const { t, d } = useI18n()
const route = useRoute()
const router = useRouter()
const { org } = useOrg()
const { me } = useMe()
const { setCrumbs } = useCrumbs()

const imageId = computed(() => String(route.params.imageId))
const tabIds = ['recipe', 'builds', 'versions', 'settings']
const tabs = computed(() => tabIds.map((v) => ({ value: v, label: t(`images.tabs.${v}`) })))
const tab = computed({
  get: () => (tabIds.includes(String(route.query.tab)) ? String(route.query.tab) : 'recipe'),
  set: (v) => router.replace({ query: { tab: v === 'recipe' ? undefined : v } }),
})
const buildId = computed(() => (route.query.build ? String(route.query.build) : ''))

const image = ref<Image | null>(null)
const builds = ref<Build[]>([])
const versions = ref<ImageVersion[]>([])
const people = ref<Record<string, string>>({})
const teams = ref<{ id: string; name: string }[]>([])
const error = ref('')
const busy = ref(false)

const name = ref('')
const description = ref('')
const scope = ref('')
const autoSchedule = ref('off')
const autoOnBaseChange = ref(false)
const confirmWiden = ref('')
const confirmDelete = ref(false)
const tagFor = ref<ImageVersion | null>(null)
const tagName = ref('')
const versionPending = ref<ImageVersion | null>(null)

const base = () => `/api/orgs/${org.value?.id}/images/${imageId.value}`
const message = (e: unknown) => (e instanceof Error ? e.message : String(e))
const fmt = (v: string) => d(new Date(v), { dateStyle: 'short', timeStyle: 'short' })
const person = (id: string) => people.value[id] ?? (id === me.value?.user.id ? me.value.user.name : id.slice(0, 8))

const selectedBuild = computed(() => builds.value.find((b) => b.id === buildId.value) ?? null)
const scopes = computed(() => imageScopes(teams.value))
const autoOptions = computed(() => ['off', 'daily', 'weekly'].map((v) => ({ value: v, label: t(`images.autoSchedules.${v}`) })))
const autoChanged = computed(() => !!image.value && (autoSchedule.value !== (image.value.autoSchedule || 'off') || autoOnBaseChange.value !== image.value.autoOnBaseChange))
const scopeOptions = computed(() => scopes.value.map((s) => ({ value: scopeKey(s), label: scopeLabel(s, t) })))
const buildColumns = computed<TableColumn[]>(() => [
  { key: 'state', label: t('images.state') },
  { key: 'revision', label: t('images.revisionCol') },
  { key: 'startedBy', label: t('images.startedBy') },
  { key: 'queuedAt', label: t('images.queued') },
  { key: 'duration', label: t('images.duration') },
  { key: 'digest', label: t('images.version') },
])
const versionColumns = computed<TableColumn[]>(() => [
  { key: 'digest', label: t('images.digest') },
  { key: 'tags', label: t('images.tags') },
  { key: 'source', label: t('images.source.label') },
  { key: 'revision', label: t('images.revisionCol') },
  { key: 'sizeBytes', label: t('images.size') },
  { key: 'createdAt', label: t('vms.created') },
  { key: 'actions', label: '' },
])

watchEffect(() => {
  if (image.value && org.value) setCrumbs(route.path, [{ label: image.value.name, to: route.path }, { label: t(`images.tabs.${tab.value}`) }])
})

async function load() {
  if (!org.value) return
  const id = org.value.id
  try {
    const [img, bs, vs] = await Promise.all([api<Image>(base()), api<Build[]>(`${base()}/builds`), api<ImageVersion[]>(`${base()}/versions`)])
    if (id !== org.value?.id) return
    const first = !image.value
    image.value = img
    builds.value = bs
    versions.value = vs
    if (first) resetSettings()
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) return router.replace(`/orgs/${id}/images`)
    error.value = message(e)
  }
}

async function loadPeople() {
  if (!org.value) return
  const id = org.value.id
  const [ms, ts] = await Promise.all([
    iamClient.listMembers({ org: id }).then((r) => r.members, () => []),
    iamClient.listTeams({ org: id }).then((r) => r.teams, () => []),
  ])
  people.value = Object.fromEntries(ms.map((m) => [m.id, m.name || m.email]))
  teams.value = ts
}

function resetSettings() {
  if (!image.value) return
  name.value = image.value.name
  description.value = image.value.description
  scope.value = scopeKey(imageScope(image.value))
  autoSchedule.value = image.value.autoSchedule || 'off'
  autoOnBaseChange.value = image.value.autoOnBaseChange
}

async function run(fn: () => Promise<unknown>) {
  busy.value = true
  error.value = ''
  try {
    await fn()
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = false
  }
  await load()
}

const openBuild = (id: string) => router.replace({ query: { tab: 'builds', build: id } })

const startBuild = () =>
  run(async () => {
    const res = await api<{ buildId: string }>(`${base()}/builds`, { method: 'POST' })
    openBuild(res.buildId)
  })

const cancelBuild = (b: Build) => run(() => api(`${base()}/builds/${b.id}/cancel`, { method: 'POST' }))

const saveGeneral = () =>
  run(async () => {
    image.value = await api<Image>(base(), { method: 'PATCH', body: JSON.stringify({ name: name.value, description: description.value.trim() }) })
  })

const saveAuto = () =>
  run(async () => {
    if (!image.value) return
    const body = autoSettingsBody(image.value, autoSchedule.value, autoOnBaseChange.value)
    image.value = await api<Image>(base(), { method: 'PATCH', body: JSON.stringify(body) })
  })

const approveRevision = () =>
  run(async () => {
    image.value = await api<Image>(base(), { method: 'PATCH', body: JSON.stringify({ approveRevision: image.value?.recipeRevision }) })
  })

async function saveScope(confirmDeleteVersions = false) {
  const target = scopes.value.find((s) => scopeKey(s) === scope.value)
  if (!target) return
  busy.value = true
  error.value = ''
  try {
    image.value = await api<Image>(base(), { method: 'PATCH', body: JSON.stringify({ ...scopeRef(target), confirmDeleteVersions }) })
    confirmWiden.value = ''
  } catch (e) {
    if (e instanceof ApiError && e.code === 'widen_deletes_versions' && !confirmDeleteVersions) confirmWiden.value = e.message
    else {
      error.value = message(e)
      confirmWiden.value = ''
    }
  } finally {
    busy.value = false
  }
  await load()
}

const remove = () =>
  run(async () => {
    await api(base(), { method: 'DELETE' })
    router.replace(`/orgs/${org.value?.id}/images`)
  })

function openTag(v: ImageVersion) {
  tagFor.value = v
  tagName.value = ''
}

const setTag = () =>
  run(async () => {
    await api(`${base()}/tags/${encodeURIComponent(tagName.value.trim())}`, { method: 'PUT', body: JSON.stringify({ digest: tagFor.value?.digest }) })
    tagFor.value = null
  })

const removeTag = (tag: string) => run(() => api(`${base()}/tags/${encodeURIComponent(tag)}`, { method: 'DELETE' }))

const removeVersion = () =>
  run(async () => {
    await api(`${base()}/versions/${versionPending.value?.digest}`, { method: 'DELETE' })
    versionPending.value = null
  })

const timer = window.setInterval(() => document.visibilityState === 'visible' && !busy.value && load(), 5000)
onBeforeUnmount(() => window.clearInterval(timer))

watch(
  () => [org.value?.id, imageId.value],
  () => {
    image.value = null
    builds.value = []
    versions.value = []
    error.value = ''
    load()
    loadPeople()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <p v-if="error && !image" role="alert" class="load-error">{{ error }}</p>
    <template v-if="image">
      <PageHeader :icon="Layers" :title="image.name" mono :subtitle="image.description || image.ref">
        <template #tags>
          <Tag>{{ scopeLabel(imageScope(image), t) }}</Tag>
          <Tag v-if="image.lastBuildState" :tone="buildTones[image.lastBuildState as keyof typeof buildTones]">{{ t(`images.states.${image.lastBuildState}`) }}</Tag>
          <Tag v-if="image.autoError" tone="warning" :title="image.autoError">{{ t('images.autoFailedTag') }}</Tag>
        </template>
        <Button v-if="image.canBuild" variant="accent" :icon="Hammer" :disabled="busy || image.recipeRevision === null" @click="startBuild">{{ t('images.build') }}</Button>
      </PageHeader>
      <div class="page-body">
        <p v-if="error" role="alert">{{ error }}</p>
        <p v-if="image.pushError" role="alert">{{ t('images.pushErrorBody', { error: image.pushError }) }}</p>
        <p v-if="image.autoError" role="alert">{{ t('images.autoErrorBody', { error: image.autoError }) }}</p>
        <Tabs v-model="tab" :tabs="tabs" variant="underline" />

        <RecipeEditor v-if="tab === 'recipe'" :org-id="org.id" :image="image" :base="base()" @saved="load" />

        <template v-if="tab === 'builds'">
          <Section v-if="selectedBuild" :label="t('images.buildLog', { id: selectedBuild.id.slice(0, 8) })">
            <template #actions>
              <Button size="sm" variant="ghost" :icon="X" @click="router.replace({ query: { tab: 'builds' } })">{{ t('images.close') }}</Button>
            </template>
            <Card>
              <p v-if="selectedBuild.error" role="alert">{{ selectedBuild.error }}</p>
              <BuildLog :base="base()" :build="selectedBuild" @ended="load">
                <Button v-if="image.canBuild && buildActive(selectedBuild)" size="sm" variant="danger" :disabled="busy" @click="cancelBuild(selectedBuild)">{{ t('images.cancelBuild') }}</Button>
              </BuildLog>
            </Card>
          </Section>
          <Section :label="t('images.builds')">
            <DataTable :columns="buildColumns" :rows="builds" row-key="id" :row-clickable="() => true" hover="clickable" @row-click="(b: Build) => openBuild(b.id)">
              <template #cell-state="{ row }"><Tag :tone="buildTones[row.state as Build['state']]" :title="row.error || undefined">{{ t(`images.states.${row.state}`) }}</Tag></template>
              <template #cell-revision="{ row }">r{{ row.revision }}</template>
              <template #cell-startedBy="{ row }">
                <span class="tags">{{ person(row.startedBy) }}<Tag v-if="row.automatic">{{ t('images.automatic') }}</Tag></span>
              </template>
              <template #cell-queuedAt="{ row }">{{ fmt(row.queuedAt) }}</template>
              <template #cell-duration="{ row }">{{ buildDuration(row as Build) || '–' }}</template>
              <template #cell-digest="{ row }"><span class="mono">{{ row.digest ? shortDigest(row.digest) : '–' }}</span></template>
              <template #empty><span class="muted">{{ t('images.noBuilds') }}</span></template>
            </DataTable>
          </Section>
        </template>

        <template v-if="tab === 'versions'">
          <Section :label="t('vms.details')">
            <Card>
              <KeyValueList>
                <KeyValue :label="t('images.reference')"><code>{{ image.ref }}</code><CopyButton :text="image.ref" /></KeyValue>
                <KeyValue :label="t('images.repository')"><code>{{ image.repository }}</code></KeyValue>
                <KeyValue :label="t('images.latest')"><span class="mono">{{ image.latestDigest || '–' }}</span></KeyValue>
                <KeyValue :label="t('images.size')">{{ image.sizeBytes ? formatBytes(image.sizeBytes) : '–' }}</KeyValue>
              </KeyValueList>
            </Card>
          </Section>
          <Section :label="t('images.push')" :description="t('images.pushHint')">
            <CommandBox :command="`observatory image push ${image.ref}:latest my-local-image`" />
          </Section>
          <Section :label="t('images.versions')">
            <DataTable :columns="versionColumns" :rows="versions" row-key="digest">
              <template #cell-digest="{ row }">
                <span class="mono">{{ shortDigest(row.digest) }}</span><CopyButton :text="`${image.ref}@${row.digest}`" />
              </template>
              <template #cell-tags="{ row }">
                <span class="tags">
                  <Tag v-for="tag in row.tags" :key="tag">
                    {{ tag }}
                    <button v-if="image.canEdit && tag !== 'latest'" type="button" class="untag" :aria-label="t('images.removeTag')" :disabled="busy" @click="removeTag(tag)"><X :size="11" /></button>
                  </Tag>
                </span>
              </template>
              <template #cell-source="{ row }">
                <a v-if="row.buildId" href="#" @click.prevent="openBuild(row.buildId)">{{ t(`images.from.${row.source}`) }}</a>
                <template v-else>{{ t(`images.from.${row.source}`) }}</template>
              </template>
              <template #cell-revision="{ row }">{{ row.revision ? `r${row.revision}` : '–' }}</template>
              <template #cell-sizeBytes="{ row }">{{ formatBytes(row.sizeBytes) }}</template>
              <template #cell-createdAt="{ row }">{{ fmt(row.createdAt) }}</template>
              <template #cell-actions="{ row }">
                <span v-if="image.canEdit" class="tags">
                  <Button size="sm" variant="ghost" :disabled="busy" @click="openTag(row as ImageVersion)">{{ t('images.tag') }}</Button>
                  <Button size="sm" variant="ghost" :disabled="busy" @click="versionPending = row as ImageVersion">{{ t('env.delete') }}</Button>
                </span>
              </template>
              <template #empty><span class="muted">{{ t('images.noVersions') }}</span></template>
            </DataTable>
          </Section>
        </template>

        <template v-if="tab === 'settings'">
          <Section :label="t('images.general')">
            <Card>
              <label>
                {{ t('images.name') }}
                <TextInput v-model="name" :disabled="!image.canEdit || busy" :aria-label="t('images.name')" />
              </label>
              <label>
                {{ t('images.description') }}
                <Textarea v-model="description" :disabled="!image.canEdit || busy" :aria-label="t('images.description')" />
              </label>
              <p v-if="image.canEdit">
                <Button variant="accent" :disabled="busy || (name === image.name && description.trim() === image.description)" @click="saveGeneral">{{ t('env.save') }}</Button>
              </p>
            </Card>
          </Section>
          <Section :label="t('images.visibility')" :description="t('images.visibilityHint')">
            <Card>
              <p class="tags">
                <Select v-model="scope" :options="scopeOptions" :disabled="!image.canEdit || busy" :aria-label="t('images.visibility')" />
                <Button v-if="image.canEdit" :disabled="busy || scope === scopeKey(imageScope(image))" @click="saveScope()">{{ t('env.save') }}</Button>
              </p>
            </Card>
          </Section>
          <Section :label="t('images.autoTitle')" :description="t('images.autoHint')">
            <Card>
              <label>
                {{ t('images.autoSchedule') }}
                <Select v-model="autoSchedule" :options="autoOptions" :disabled="!image.canEdit || !image.canBuild || busy" :aria-label="t('images.autoSchedule')" />
              </label>
              <Checkbox v-model="autoOnBaseChange" :disabled="!image.canEdit || !image.canBuild || busy">{{ t('images.autoOnBaseChange') }}</Checkbox>
              <span v-if="image.recipeRevision === null" class="muted">{{ t('images.autoNoRecipe') }}</span>
              <span v-else-if="image.autoLastAt" class="muted">{{ t('images.autoLast', { at: fmt(image.autoLastAt) }) }}</span>
              <template v-if="image.autoNeedsApproval">
                <span role="status">{{ t('images.autoApproveHint', { n: image.recipeRevision }) }}</span>
                <p v-if="image.canEdit && image.canBuild">
                  <Button :disabled="busy" @click="approveRevision">{{ t('images.autoApprove', { n: image.recipeRevision }) }}</Button>
                </p>
              </template>
              <p v-if="image.canEdit && image.canBuild">
                <Button variant="accent" :disabled="busy || !autoChanged" @click="saveAuto">{{ t('env.save') }}</Button>
              </p>
            </Card>
          </Section>
          <Section v-if="image.canEdit" :label="t('images.deleteTitle')">
            <Card tone="danger">
              <span class="muted">{{ t('images.deleteBody', { name: image.ref }) }}</span>
              <p><Button variant="danger" :disabled="busy" @click="confirmDelete = true">{{ t('env.delete') }}</Button></p>
            </Card>
          </Section>
        </template>
      </div>

      <Dialog :open="!!confirmWiden" :title="t('images.widenTitle')" :description="t('images.widenBody')" @update:open="(o) => !o && !busy && (confirmWiden = '')">
        <p role="alert">{{ confirmWiden }}</p>
        <template #actions>
          <Button :disabled="busy" @click="confirmWiden = ''">{{ t('profile.cancel') }}</Button>
          <Button variant="danger" :disabled="busy" @click="saveScope(true)">{{ t('images.widenConfirm') }}</Button>
        </template>
      </Dialog>
      <Dialog :open="confirmDelete" :title="t('images.deleteTitle')" :description="t('images.deleteBody', { name: image.ref })" @update:open="(o) => !o && !busy && (confirmDelete = false)">
        <template #actions>
          <Button :disabled="busy" @click="confirmDelete = false">{{ t('profile.cancel') }}</Button>
          <Button variant="danger" :disabled="busy" @click="remove">{{ t('env.delete') }}</Button>
        </template>
      </Dialog>
      <Dialog :open="!!tagFor" :title="t('images.tagTitle')" :description="tagFor ? shortDigest(tagFor.digest) : ''" @update:open="(o) => !o && !busy && (tagFor = null)">
        <TextInput v-model="tagName" :aria-label="t('images.tag')" :placeholder="t('images.tagPlaceholder')" />
        <template #actions>
          <Button :disabled="busy" @click="tagFor = null">{{ t('profile.cancel') }}</Button>
          <Button variant="accent" :disabled="busy || !tagName.trim() || tagName.trim() === 'latest'" @click="setTag">{{ t('images.tag') }}</Button>
        </template>
      </Dialog>
      <Dialog
        :open="!!versionPending"
        :title="t('images.deleteVersionTitle')"
        :description="t('images.deleteVersionBody', { digest: versionPending ? shortDigest(versionPending.digest) : '' })"
        @update:open="(o) => !o && !busy && (versionPending = null)"
      >
        <template #actions>
          <Button :disabled="busy" @click="versionPending = null">{{ t('profile.cancel') }}</Button>
          <Button variant="danger" :disabled="busy" @click="removeVersion">{{ t('env.delete') }}</Button>
        </template>
      </Dialog>
    </template>
  </template>
</template>

<style scoped>
.load-error {
  margin: var(--page-y) var(--page-x);
}
label {
  display: grid;
  gap: 4px;
}
.tags {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px;
  margin: 0;
}
.untag {
  display: inline-flex;
  padding: 0;
  border: 0;
  background: none;
  color: inherit;
  cursor: pointer;
}
</style>
