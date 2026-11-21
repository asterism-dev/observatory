<script setup lang="ts">
import { Button, Tabs, TextInput } from '@asterism-dev/lumen'
import { Layers, Plus } from '@lucide/vue'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'

import { api } from '@/api'
import { iamClient } from '@/composables/iamClient'
import CreateImageDialog from '@/components/CreateImageDialog.vue'
import { scopeLabel } from '@/composables/envScopes'
import { buildTones, formatBytes, imageScope, imageScopes, shortDigest, type Image } from '@/composables/images'
import { useOrg } from '@/composables/useOrg'
import { EmptyState, FilterBar, Page, ResourceList, ResourceRow, Tag } from '@/ui'

const { t } = useI18n()
const { org } = useOrg()
const router = useRouter()

const items = ref<Image[]>([])
const teams = ref<{ id: string; name: string }[]>([])
const tab = ref('all')
const search = ref('')
const error = ref('')
const loaded = ref(false)
const createOpen = ref(false)

const tabs = computed(() => ['all', 'org', 'teams', 'private'].map((v) => ({ value: v, label: t(`env.tab.${v}`) })))
const rows = computed(() => {
  const scope = { all: null, org: 'org', teams: 'team', private: 'user' }[tab.value]
  const q = search.value.trim().toLowerCase()
  return items.value.filter((v) => (!scope || (!v.platform && v.scope === scope)) && (!q || v.ref.toLowerCase().includes(q)))
})
const own = computed(() => items.value.filter((v) => !v.platform))

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const [list, ts] = await Promise.all([
      api<Image[]>(`/api/orgs/${id}/images`),
      iamClient.listTeams({ org: id }).then((r) => r.teams, () => []),
    ])
    if (id !== org.value?.id) return
    items.value = list
    teams.value = ts
    loaded.value = true
  } catch (e) {
    if (id === org.value?.id) error.value = e instanceof Error ? e.message : String(e)
  }
}

const created = (id: string) => router.push(`/orgs/${org.value?.id}/images/${id}`)

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
  <Page v-if="org" :icon="Layers" :title="t('nav.images')" :subtitle="t('images.subtitle')">
    <template #actions>
      <Button variant="accent" :icon="Plus" @click="createOpen = true">{{ t('images.create') }}</Button>
    </template>
    <p v-if="error" role="alert">{{ error }}</p>
    <EmptyState v-if="loaded && !own.length" :label="t('nav.images')" :title="t('images.empty')" :text="t('images.emptyText')" command="observatory image create my-tools">
      <Button variant="accent" :icon="Plus" @click="createOpen = true">{{ t('images.create') }}</Button>
    </EmptyState>
    <template v-else>
      <FilterBar>
        <Tabs v-model="tab" :tabs="tabs" variant="underline" />
        <template #end>
          <TextInput v-model="search" type="search" :placeholder="t('images.search')" :aria-label="t('images.search')" />
        </template>
      </FilterBar>
      <ResourceList :count="rows.length" :empty="t('images.noMatch')">
        <ResourceRow
          v-for="v in rows"
          :key="v.id"
          :title="v.ref"
          :meta="v.description || undefined"
          :to="v.platform ? undefined : `/orgs/${org.id}/images/${v.id}`"
          :tone="v.pushError ? 'danger' : v.lastBuildState ? buildTones[v.lastBuildState as keyof typeof buildTones] : 'muted'"
        >
          <template #tags>
            <Tag v-if="v.platform" strong>{{ t('images.platform') }}</Tag>
            <Tag v-else>{{ scopeLabel(imageScope(v), t) }}</Tag>
            <Tag v-for="tag in Object.keys(v.tags).sort()" :key="tag">{{ tag }}</Tag>
            <Tag v-if="v.pushError" tone="danger" :title="v.pushError">{{ t('images.pushError') }}</Tag>
          </template>
          <span v-if="v.latestDigest" class="mono">{{ shortDigest(v.latestDigest) }}</span>
          <span v-if="v.sizeBytes">{{ formatBytes(v.sizeBytes) }}</span>
          <span v-if="v.lastBuildState">{{ t('images.lastBuild') }}: {{ t(`images.states.${v.lastBuildState}`) }}</span>
        </ResourceRow>
      </ResourceList>
    </template>
    <CreateImageDialog v-model:open="createOpen" :org-id="org.id" :scopes="imageScopes(teams)" @created="created" />
  </Page>
</template>
