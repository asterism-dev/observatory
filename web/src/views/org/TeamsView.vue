<script setup lang="ts">
import { Button, Table, TextInput, type TableColumn } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'

import { api } from '@/api'
import { useOrg } from '@/composables/useOrg'

interface Team {
  id: string
  name: string
  memberCount: number
}

const { t } = useI18n()
const router = useRouter()
const { org, can, reload } = useOrg()

const teams = ref<Team[]>([])
const name = ref('')
const busy = ref(false)
const error = ref('')

const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('teams.name') },
  { key: 'memberCount', label: t('teams.members') },
  { key: 'actions', label: '' },
])
const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const res = await api<Team[]>(`/api/orgs/${id}/teams`)
    // Org switched while loading.
    if (id === org.value?.id) teams.value = res
  } catch (e) {
    if (id === org.value?.id) error.value = message(e)
  }
}

async function create() {
  busy.value = true
  error.value = ''
  try {
    await api(`/api/orgs/${org.value?.id}/teams`, { method: 'POST', body: JSON.stringify({ name: name.value.trim() }) })
    name.value = ''
    await reload()
    await refresh()
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = false
  }
}

watch(
  () => org.value?.id,
  () => {
    error.value = ''
    teams.value = []
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <h1>{{ t('nav.teams') }}</h1>
    <p v-if="error" role="alert">{{ error }}</p>
    <form v-if="can('team:create')" class="row" @submit.prevent="create">
      <TextInput v-model="name" :placeholder="t('teams.name')" :aria-label="t('teams.name')" />
      <Button type="submit" variant="accent" :disabled="busy || !name.trim()">{{ t('teams.create') }}</Button>
    </form>
    <Table :columns="columns" :rows="teams" row-key="id">
      <template #cell-actions="{ row }">
        <Button size="sm" @click="router.push(`/orgs/${org.id}/teams/${row.id}`)">{{ t('teams.open') }}</Button>
      </template>
      <template #empty>{{ t('teams.empty') }}</template>
    </Table>
  </template>
</template>

<style scoped>
.row {
  display: flex;
  gap: 8px;
  margin-bottom: 12px;
}
</style>
