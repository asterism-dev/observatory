<script setup lang="ts">
import { Button, Tabs } from '@asterism-dev/lumen'
import { Plug, Plus } from '@lucide/vue'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import ConnectIntegrationDialog from '@/components/ConnectIntegrationDialog.vue'
import IntegrationIcon from '@/components/IntegrationIcon.vue'
import { scopeLabel } from '@/composables/envScopes'
import { kindLabels, shownAccount, statusLabel, statusTone, type Integration, type IntegrationKind, type IntegrationScope } from '@/composables/integrations'
import { useOrg } from '@/composables/useOrg'
import { FilterBar, Page, ResourceList, ResourceRow, Section, Tag } from '@/ui'

const { t } = useI18n()
const { org } = useOrg()

const items = ref<Integration[]>([])
const scopes = ref<IntegrationScope[]>([])
const tab = ref('all')
const error = ref('')
const formOpen = ref(false)
const loaded = ref(false)
const connectKind = ref<IntegrationKind>('github')
const kinds = Object.keys(kindLabels) as IntegrationKind[]

const tabs = computed(() => ['all', 'org', 'teams', 'private'].map((v) => ({ value: v, label: t(`env.tab.${v}`) })))
const rows = computed(() => {
  const scope = { all: null, org: 'org', teams: 'team', private: 'user' }[tab.value]
  return scope ? items.value.filter((v) => v.scope === scope) : items.value
})

function openConnect(k: IntegrationKind) {
  connectKind.value = k
  formOpen.value = true
}

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const res = await api<{ integrations: Integration[]; writableScopes: IntegrationScope[] }>(`/api/orgs/${id}/integrations`)
    if (id !== org.value?.id) return
    items.value = res.integrations
    scopes.value = res.writableScopes
    loaded.value = true
  } catch (e) {
    if (id === org.value?.id) error.value = e instanceof Error ? e.message : String(e)
  }
}

watch(
  () => org.value?.id,
  () => {
    error.value = ''
    items.value = []
    scopes.value = []
    loaded.value = false
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <Page v-if="org" :icon="Plug" :title="t('nav.integrations')" :subtitle="t('integrations.subtitle')">
    <p v-if="error" role="alert">{{ error }}</p>
    <Section :label="`${t('integrations.connectedTitle')} · ${items.length}`">
      <FilterBar>
        <Tabs v-model="tab" :tabs="tabs" variant="underline" />
      </FilterBar>
      <ResourceList :count="rows.length" :empty="loaded ? t(items.length ? 'integrations.empty' : 'integrations.emptyText') : ''">
        <ResourceRow v-for="v in rows" :key="v.id" :title="kindLabels[v.kind]" :meta="shownAccount(v) || '—'" :tone="statusTone(v)" :to="`/orgs/${org.id}/integrations/${v.id}`">
          <template #title><IntegrationIcon :kind="v.kind" /> {{ kindLabels[v.kind] }}</template>
          <template #tags>
            <Tag>{{ scopeLabel(v, t) }}</Tag>
            <Tag>{{ t(`integrations.mode_${v.mode}`) }}</Tag>
            <Tag :tone="statusTone(v)" :title="v.status === 'expired' ? v.lastError : undefined">{{ statusLabel(v, t) }}</Tag>
          </template>
        </ResourceRow>
      </ResourceList>
    </Section>
    <Section :label="`${t('integrations.availableTitle')} · ${kinds.length}`">
      <ResourceList :count="kinds.length">
        <ResourceRow v-for="k in kinds" :key="k" :title="kindLabels[k]" :meta="t(`integrations.about_${k}`)">
          <template #title><IntegrationIcon :kind="k" /> {{ kindLabels[k] }}</template>
          <template v-if="items.some((v) => v.kind === k)" #tags>
            <Tag tone="success">{{ t('integrations.connectedTag') }}</Tag>
          </template>
          <template v-if="scopes.some((s) => !s.connected.includes(k))" #actions>
            <Button size="sm" :icon="Plus" @click="openConnect(k)">{{ t('integrations.connect') }}</Button>
          </template>
        </ResourceRow>
      </ResourceList>
    </Section>

    <ConnectIntegrationDialog v-model:open="formOpen" :org-id="org.id" :scopes="scopes" :replacing="null" :initial-kind="connectKind" @saved="refresh" @conflict="refresh" />
  </Page>
</template>
