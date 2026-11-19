<script setup lang="ts">
import { Button, Dialog, Tabs, TextInput } from '@asterism-dev/lumen'
import { KeyRound, Plus } from '@lucide/vue'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import EnvMoveDialog from '@/components/EnvMoveDialog.vue'
import EnvVarDialog from '@/components/EnvVarDialog.vue'
import { scopeKey, scopeLabel, type EnvVar, type WritableScope } from '@/composables/envScopes'
import { useOrg } from '@/composables/useOrg'
import { CopyButton, EmptyState, FilterBar, Page, ResourceList, ResourceRow, Tag } from '@/ui'

const { t } = useI18n()
const { org } = useOrg()

const vars = ref<EnvVar[]>([])
const writableScopes = ref<WritableScope[]>([])
const tab = ref('all')
const error = ref('')
const busy = ref(false)
const formOpen = ref(false)
const editing = ref<EnvVar | null>(null)
const moveOpen = ref(false)
const moving = ref<EnvVar | null>(null)
const pendingDelete = ref<EnvVar | null>(null)
const loaded = ref(false)
const search = ref('')

const tabs = computed(() => ['all', 'org', 'teams', 'private'].map((v) => ({ value: v, label: t(`env.tab.${v}`) })))
const rows = computed(() => {
  const scope = { all: null, org: 'org', teams: 'team', private: 'user' }[tab.value]
  const q = search.value.trim().toLowerCase()
  return vars.value.filter((v) => (!scope || v.scope === scope) && (!q || v.name.toLowerCase().includes(q)))
})

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const res = await api<{ vars: EnvVar[]; writableScopes: WritableScope[] }>(`/api/orgs/${id}/env`)
    // Org switched while loading.
    if (id !== org.value?.id) return
    vars.value = res.vars
    writableScopes.value = res.writableScopes
    loaded.value = true
  } catch (e) {
    if (id === org.value?.id) error.value = e instanceof Error ? e.message : String(e)
  }
}

const canMove = (v: EnvVar) => v.canEdit && writableScopes.value.some((s) => scopeKey(s) !== scopeKey(v))

function openAdd() {
  editing.value = null
  formOpen.value = true
}

function openEdit(v: EnvVar) {
  editing.value = v
  formOpen.value = true
}

function openMove(v: EnvVar) {
  moving.value = v
  moveOpen.value = true
}

async function remove() {
  if (!pendingDelete.value) return
  busy.value = true
  error.value = ''
  try {
    await api(`/api/orgs/${org.value?.id}/env/${pendingDelete.value.id}`, { method: 'DELETE' })
    pendingDelete.value = null
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
    pendingDelete.value = null
  } finally {
    busy.value = false
  }
  await refresh()
}

watch(
  () => org.value?.id,
  () => {
    error.value = ''
    vars.value = []
    writableScopes.value = []
    loaded.value = false
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <Page v-if="org" :icon="KeyRound" :title="t('nav.env')" :subtitle="t('env.subtitle')">
    <template #actions>
      <Button v-if="writableScopes.length" variant="accent" :icon="Plus" @click="openAdd">{{ t('env.add') }}</Button>
    </template>
    <p v-if="error" role="alert">{{ error }}</p>
    <EnvVarDialog v-model:open="formOpen" :org-id="org.id" :scopes="writableScopes" :editing="editing" @saved="refresh" @conflict="refresh" />
    <EnvMoveDialog v-model:open="moveOpen" :org-id="org.id" :scopes="writableScopes" :variable="moving" @saved="refresh" @conflict="refresh" />
    <EmptyState v-if="loaded && !vars.length" :label="t('nav.env')" :title="t('env.empty')" :text="t('env.emptyText')" command="observatory var set API_URL --value https://api.example.com">
      <Button v-if="writableScopes.length" variant="accent" :icon="Plus" @click="openAdd">{{ t('env.add') }}</Button>
    </EmptyState>
    <template v-else>
      <FilterBar>
        <Tabs v-model="tab" :tabs="tabs" variant="underline" />
        <template #end>
          <TextInput v-model="search" type="search" :placeholder="t('env.search')" :aria-label="t('env.search')" />
        </template>
      </FilterBar>
      <ResourceList :count="rows.length" :empty="vars.length ? t('env.noMatch') : t('env.empty')">
        <ResourceRow v-for="v in rows" :key="v.id" :title="v.name" :meta="v.hosts.length ? v.hosts.join(' · ') : undefined">
          <template #tags>
            <Tag :tone="v.kind === 'secret' ? 'warning' : 'default'">{{ v.kind }}</Tag>
            <Tag>{{ scopeLabel(v, t) }}</Tag>
          </template>
          <span v-if="v.kind === 'secret'" class="mono">••••••</span>
          <span v-else class="mono value">{{ v.value }}<CopyButton :text="v.value ?? ''" /></span>
          <template #actions>
            <Button v-if="v.canEdit" size="sm" variant="ghost" @click="openEdit(v)">{{ t('env.edit') }}</Button>
            <Button v-if="canMove(v)" size="sm" variant="ghost" @click="openMove(v)">{{ t('env.move') }}</Button>
            <Button v-if="v.canDelete" size="sm" variant="ghost" @click="pendingDelete = v">{{ t('env.delete') }}</Button>
          </template>
        </ResourceRow>
      </ResourceList>
    </template>
    <Dialog
      :open="!!pendingDelete"
      :title="t('env.deleteTitle')"
      :description="t('env.deleteBody', { name: pendingDelete?.name })"
      @update:open="(o) => !o && !busy && (pendingDelete = null)"
    >
      <template #actions>
        <Button :disabled="busy" @click="pendingDelete = null">{{ t('profile.cancel') }}</Button>
        <Button variant="danger" :disabled="busy" @click="remove">{{ t('env.delete') }}</Button>
      </template>
    </Dialog>
  </Page>
</template>

<style scoped>
.value {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  min-width: 0;
  overflow-wrap: anywhere;
}
</style>
