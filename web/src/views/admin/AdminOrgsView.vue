<script setup lang="ts">
import { type TableColumn } from '@asterism-dev/lumen'
import { timestampDate } from '@bufbuild/protobuf/wkt'
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'

import { errorMessage, iamClient, isUnauthenticated } from '@/composables/iamClient'
import { useMe } from '@/composables/useMe'
import { DataTable, Tag } from '@/ui'

type OrgSummary = Awaited<ReturnType<typeof iamClient.listAllOrgs>>['orgs'][number]

const { t, d } = useI18n()
const router = useRouter()
const { expire } = useMe()
const orgs = ref<OrgSummary[]>([])
const error = ref('')

const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('org.name') },
  { key: 'memberCount', label: t('admin.members') },
  { key: 'createdAt', label: t('admin.created') },
])

onMounted(async () => {
  try {
    orgs.value = (await iamClient.listAllOrgs({})).orgs
  } catch (e) {
    if (isUnauthenticated(e)) return expire()
    error.value = errorMessage(e)
  }
})
</script>

<template>
  <p v-if="error" role="alert">{{ error }}</p>
  <DataTable :columns="columns" :rows="orgs" row-key="id">
    <template #cell-name="{ row }">
      <span class="cell">
        <a :href="`/admin/orgs/${row.id}`" @click.prevent="router.push(`/admin/orgs/${row.id}`)">{{ row.name }}</a>
        <Tag v-if="row.personal">{{ t('admin.personal') }}</Tag>
      </span>
    </template>
    <template #cell-createdAt="{ row }">{{ row.createdAt ? d(timestampDate(row.createdAt), { dateStyle: 'short' }) : '–' }}</template>
  </DataTable>
</template>

<style scoped>
.cell {
  display: inline-flex;
  align-items: center;
  gap: 8px;
}
</style>
