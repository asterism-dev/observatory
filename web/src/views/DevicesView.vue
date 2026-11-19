<script setup lang="ts">
import { Button, Dialog, Table, type TableColumn } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import AddDeviceDialog from '@/components/AddDeviceDialog.vue'
import { useMe } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'

interface Device {
  id: string
  name: string
  ip: string
  owner: { id: string; name: string }
  lastHandshake: string | null
  canDelete: boolean
}

const { t, d } = useI18n()
const { org } = useOrg()
const { me } = useMe()

const devices = ref<Device[]>([])
const error = ref('')
const addOpen = ref(false)
const pendingDelete = ref<Device | null>(null)
const busy = ref(false)

const showOwner = computed(() => devices.value.some((x) => x.owner.id !== me.value?.user.id))
const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('devices.name') },
  { key: 'ip', label: t('devices.address') },
  ...(showOwner.value ? [{ key: 'owner', label: t('devices.owner') }] : []),
  { key: 'handshake', label: t('devices.handshake') },
  { key: 'actions', label: '' },
])
const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const res = await api<Device[]>(`/api/orgs/${id}/devices`)
    if (id === org.value?.id) devices.value = res
  } catch (e) {
    if (id === org.value?.id) error.value = message(e)
  }
}

async function remove(x: Device) {
  busy.value = true
  error.value = ''
  try {
    await api(`/api/orgs/${org.value?.id}/devices/${x.id}`, { method: 'DELETE' })
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
    devices.value = []
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <div class="head">
      <div>
        <h1>{{ t('nav.devices') }}</h1>
        <p>{{ t('devices.subtitle') }}</p>
      </div>
      <Button variant="accent" @click="addOpen = true">{{ t('devices.add') }}</Button>
    </div>
    <p v-if="error" role="alert">{{ error }}</p>
    <Table :columns="columns" :rows="devices" row-key="id">
      <template #cell-owner="{ row }">{{ row.owner.name }}</template>
      <template #cell-handshake="{ row }">{{ row.lastHandshake ? d(new Date(row.lastHandshake), { dateStyle: 'short', timeStyle: 'short' }) : t('devices.never') }}</template>
      <template #cell-actions="{ row }">
        <Button v-if="row.canDelete" size="sm" variant="danger" :disabled="busy" @click="pendingDelete = row">{{ t('devices.delete') }}</Button>
      </template>
      <template #empty>{{ t('devices.empty') }}</template>
    </Table>
    <AddDeviceDialog v-model:open="addOpen" :org-id="org.id" @created="refresh" />
    <Dialog
      :open="!!pendingDelete"
      :title="t('devices.deleteTitle')"
      :description="t('devices.deleteBody', { name: pendingDelete?.name })"
      @update:open="(o) => !o && !busy && (pendingDelete = null)"
    >
      <template #actions>
        <Button :disabled="busy" @click="pendingDelete = null">{{ t('profile.cancel') }}</Button>
        <Button variant="danger" :disabled="busy" @click="pendingDelete && remove(pendingDelete)">{{ t('devices.delete') }}</Button>
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
