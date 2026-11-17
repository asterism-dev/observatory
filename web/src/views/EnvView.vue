<script setup lang="ts">
import { Badge, Button, Dialog, Table, Tabs, type TableColumn } from '@asterism-dev/lumen'
import { Copy } from '@lucide/vue'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import EnvMoveDialog from '@/components/EnvMoveDialog.vue'
import EnvVarDialog from '@/components/EnvVarDialog.vue'
import { scopeKey, scopeLabel, type EnvVar, type WritableScope } from '@/composables/envScopes'
import { useOrg } from '@/composables/useOrg'

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

const tabs = computed(() => ['all', 'org', 'teams', 'private'].map((v) => ({ value: v, label: t(`env.tab.${v}`) })))
const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('env.name') },
  { key: 'kind', label: t('env.type') },
  { key: 'scope', label: t('env.scope') },
  { key: 'value', label: t('env.value') },
  { key: 'actions', label: '' },
])
const rows = computed(() => {
  const scope = { all: null, org: 'org', teams: 'team', private: 'user' }[tab.value]
  return scope ? vars.value.filter((v) => v.scope === scope) : vars.value
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
  } catch (e) {
    if (id === org.value?.id) error.value = e instanceof Error ? e.message : String(e)
  }
}

const canMove = (v: EnvVar) => v.canEdit && writableScopes.value.some((s) => scopeKey(s) !== scopeKey(v))

const copy = (v?: string) => navigator.clipboard.writeText(v ?? '')

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
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <div class="head">
      <div>
        <h1>{{ t('nav.env') }}</h1>
        <p>{{ t('env.subtitle') }}</p>
      </div>
      <Button v-if="writableScopes.length" variant="accent" @click="openAdd">{{ t('env.add') }}</Button>
    </div>
    <p v-if="error" role="alert">{{ error }}</p>
    <Tabs v-model="tab" :tabs="tabs" />
    <Table :columns="columns" :rows="rows" row-key="id">
      <template #cell-kind="{ row }"><Badge>{{ row.kind }}</Badge></template>
      <template #cell-scope="{ row }">{{ scopeLabel(row, t) }}</template>
      <template #cell-value="{ row }">
        <span v-if="row.kind === 'secret'">••••••</span>
        <span v-else class="cell">
          {{ row.value }}
          <Button size="sm" :aria-label="t('env.copy')" @click="copy(row.value)"><Copy :size="14" /></Button>
        </span>
      </template>
      <template #cell-actions="{ row }">
        <span class="cell">
          <Button v-if="row.canEdit" size="sm" @click="openEdit(row)">{{ t('env.edit') }}</Button>
          <Button v-if="canMove(row)" size="sm" @click="openMove(row)">{{ t('env.move') }}</Button>
          <Button v-if="row.canDelete" size="sm" variant="danger" @click="pendingDelete = row">{{ t('env.delete') }}</Button>
        </span>
      </template>
      <template #empty>{{ t('env.empty') }}</template>
    </Table>

    <EnvVarDialog v-model:open="formOpen" :org-id="org.id" :scopes="writableScopes" :editing="editing" @saved="refresh" @conflict="refresh" />
    <EnvMoveDialog v-model:open="moveOpen" :org-id="org.id" :scopes="writableScopes" :variable="moving" @saved="refresh" @conflict="refresh" />
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
  </template>
</template>

<style scoped>
.head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 8px;
}
.cell {
  display: inline-flex;
  align-items: center;
  gap: 8px;
}
</style>
