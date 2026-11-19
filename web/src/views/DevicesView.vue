<script setup lang="ts">
import { Button, Dialog, TextInput } from '@asterism-dev/lumen'
import { Laptop, Plus } from '@lucide/vue'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import AddDeviceDialog from '@/components/AddDeviceDialog.vue'
import { useMe } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'
import { EmptyState, FilterBar, Page, ResourceList, ResourceRow, Tag } from '@/ui'

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
const loaded = ref(false)
const search = ref('')

const showOwner = computed(() => devices.value.some((x) => x.owner.id !== me.value?.user.id))
const rows = computed(() => {
  const q = search.value.trim().toLowerCase()
  return devices.value.filter((x) => !q || x.name.toLowerCase().includes(q))
})
const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const res = await api<Device[]>(`/api/orgs/${id}/devices`)
    if (id !== org.value?.id) return
    devices.value = res
    loaded.value = true
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
    loaded.value = false
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <Page v-if="org" :icon="Laptop" :title="t('nav.devices')" :subtitle="t('devices.subtitle')">
    <template #actions>
      <Button variant="accent" :icon="Plus" @click="addOpen = true">{{ t('devices.add') }}</Button>
    </template>
    <p v-if="error" role="alert">{{ error }}</p>
    <EmptyState v-if="loaded && !devices.length" :label="t('nav.devices')" :title="t('devices.empty')" :text="t('devices.emptyText')" command="observatory device create laptop">
      <Button variant="accent" :icon="Plus" @click="addOpen = true">{{ t('devices.add') }}</Button>
    </EmptyState>
    <template v-else>
      <FilterBar>
        <template #end>
          <TextInput v-model="search" type="search" :placeholder="t('devices.search')" :aria-label="t('devices.search')" />
        </template>
      </FilterBar>
      <ResourceList :count="rows.length" :empty="devices.length ? t('devices.noMatch') : t('devices.empty')">
        <ResourceRow v-for="x in rows" :key="x.id" :title="x.name" :meta="x.ip">
          <template v-if="showOwner" #tags>
            <Tag>{{ x.owner.name }}</Tag>
          </template>
          <span>{{ t('devices.handshake') }}: {{ x.lastHandshake ? d(new Date(x.lastHandshake), { dateStyle: 'short', timeStyle: 'short' }) : t('devices.never') }}</span>
          <template v-if="x.canDelete" #actions>
            <Button size="sm" variant="ghost" :disabled="busy" @click="pendingDelete = x">{{ t('devices.delete') }}</Button>
          </template>
        </ResourceRow>
      </ResourceList>
    </template>
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
  </Page>
</template>

