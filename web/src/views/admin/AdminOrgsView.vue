<script setup lang="ts">
import { Badge, Table, type TableColumn } from '@asterism-dev/lumen'
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'

import { api } from '@/api'

interface OrgSummary {
  id: string
  name: string
  personal: boolean
  memberCount: number
  createdAt: string
}

const { t, d } = useI18n()
const router = useRouter()
const orgs = ref<OrgSummary[]>([])
const error = ref('')

const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('org.name') },
  { key: 'memberCount', label: t('admin.members') },
  { key: 'createdAt', label: t('admin.created') },
])

onMounted(async () => {
  try {
    orgs.value = await api<OrgSummary[]>('/api/admin/orgs')
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
})
</script>

<template>
  <p v-if="error" role="alert">{{ error }}</p>
  <Table :columns="columns" :rows="orgs" row-key="id">
    <template #cell-name="{ row }">
      <span class="cell">
        <a :href="`/admin/orgs/${row.id}`" @click.prevent="router.push(`/admin/orgs/${row.id}`)">{{ row.name }}</a>
        <Badge v-if="row.personal">{{ t('admin.personal') }}</Badge>
      </span>
    </template>
    <template #cell-createdAt="{ row }">{{ d(new Date(row.createdAt), { dateStyle: 'short' }) }}</template>
  </Table>
</template>

<style scoped>
.cell {
  display: inline-flex;
  align-items: center;
  gap: 8px;
}
</style>
