<script setup lang="ts">
import { Button, Dialog, Table, type TableColumn } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import CreateNetworkDialog from '@/components/CreateNetworkDialog.vue'
import { useOrg } from '@/composables/useOrg'

interface Network {
  id: string
  name: string
  subnet: string
  vmCount: number
  canDelete: boolean
}

const { t } = useI18n()
const { org } = useOrg()

const networks = ref<Network[]>([])
const error = ref('')
const addOpen = ref(false)
const pendingDelete = ref<Network | null>(null)
const busy = ref(false)

const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('networks.name') },
  { key: 'subnet', label: t('networks.subnet') },
  { key: 'vmCount', label: t('networks.vmCount') },
  { key: 'actions', label: '' },
])
const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const res = await api<Network[]>(`/api/orgs/${id}/networks`)
    if (id === org.value?.id) networks.value = res
  } catch (e) {
    if (id === org.value?.id) error.value = message(e)
  }
}

async function remove(n: Network) {
  busy.value = true
  error.value = ''
  try {
    await api(`/api/orgs/${org.value?.id}/networks/${n.id}`, { method: 'DELETE' })
    await refresh()
  } catch (e) {
    error.value = message(e)
  } finally {
    pendingDelete.value = null
    busy.value = false
  }
}

watch(
  () => org.value?.id,
  () => {
    error.value = ''
    networks.value = []
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <div class="head">
      <div>
        <h1>{{ t('nav.networks') }}</h1>
        <p>{{ t('networks.subtitle') }}</p>
      </div>
      <Button variant="accent" @click="addOpen = true">{{ t('networks.create') }}</Button>
    </div>
    <p v-if="error" role="alert">{{ error }}</p>
    <Table :columns="columns" :rows="networks" row-key="id">
      <template #cell-actions="{ row }">
        <Button
          v-if="row.canDelete"
          size="sm"
          variant="danger"
          :disabled="busy || row.vmCount > 0"
          :title="row.vmCount > 0 ? t('networks.deleteBlocked') : undefined"
          @click="pendingDelete = row"
        >{{ t('networks.delete') }}</Button>
        <small v-if="row.canDelete && row.vmCount > 0">{{ t('networks.deleteBlocked') }}</small>
      </template>
      <template #empty>{{ t('networks.empty') }}</template>
    </Table>
    <CreateNetworkDialog v-model:open="addOpen" :org-id="org.id" @created="refresh" />
    <Dialog
      :open="!!pendingDelete"
      :title="t('networks.deleteTitle')"
      :description="t('networks.deleteBody', { name: pendingDelete?.name })"
      @update:open="(o) => !o && !busy && (pendingDelete = null)"
    >
      <template #actions>
        <Button :disabled="busy" @click="pendingDelete = null">{{ t('profile.cancel') }}</Button>
        <Button variant="danger" :disabled="busy" @click="pendingDelete && remove(pendingDelete)">{{ t('networks.delete') }}</Button>
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
</style>
