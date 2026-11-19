<script setup lang="ts">
import { Button, Dialog } from '@asterism-dev/lumen'
import { Network as NetworkIcon, Plus } from '@lucide/vue'
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import CreateNetworkDialog from '@/components/CreateNetworkDialog.vue'
import { useOrg } from '@/composables/useOrg'
import { EmptyState, Page, ResourceList, ResourceRow, Tag } from '@/ui'

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
const loaded = ref(false)

const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const res = await api<Network[]>(`/api/orgs/${id}/networks`)
    if (id !== org.value?.id) return
    networks.value = res
    loaded.value = true
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
    loaded.value = false
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <Page v-if="org" :icon="NetworkIcon" :title="t('nav.networks')" :subtitle="t('networks.subtitle')">
    <template #actions>
      <Button variant="accent" :icon="Plus" @click="addOpen = true">{{ t('networks.create') }}</Button>
    </template>
    <p v-if="error" role="alert">{{ error }}</p>
    <EmptyState v-if="loaded && !networks.length" :label="t('nav.networks')" :title="t('networks.empty')" :text="t('networks.emptyText')" command="observatory network create backend">
      <Button variant="accent" :icon="Plus" @click="addOpen = true">{{ t('networks.create') }}</Button>
    </EmptyState>
    <ResourceList v-else :count="networks.length" :empty="t('networks.empty')">
      <ResourceRow v-for="n in networks" :key="n.id" :title="n.name" :meta="n.subnet">
        <template #tags>
          <Tag>{{ n.vmCount }} {{ t('networks.vmCount') }}</Tag>
        </template>
        <small v-if="n.canDelete && n.vmCount > 0">{{ t('networks.deleteBlocked') }}</small>
        <template v-if="n.canDelete" #actions>
          <Button
            size="sm"
            variant="ghost"
            :disabled="busy || n.vmCount > 0"
            :title="n.vmCount > 0 ? t('networks.deleteBlocked') : undefined"
            @click="pendingDelete = n"
          >{{ t('networks.delete') }}</Button>
        </template>
      </ResourceRow>
    </ResourceList>
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
  </Page>
</template>

