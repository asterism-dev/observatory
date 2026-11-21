<script setup lang="ts">
import { Switch, type TableColumn } from '@asterism-dev/lumen'
import { timestampDate } from '@bufbuild/protobuf/wkt'
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'

import { errorMessage, iamClient, isUnauthenticated } from '@/composables/iamClient'
import { useMe } from '@/composables/useMe'
import { DataTable } from '@/ui'

type AdminUser = Awaited<ReturnType<typeof iamClient.listUsers>>['users'][number]

const { t, d } = useI18n()
const router = useRouter()
const { me, load: loadMe, expire } = useMe()
const users = ref<AdminUser[]>([])
const busy = ref(false)
const error = ref('')

const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('members.name') },
  { key: 'email', label: t('members.email') },
  { key: 'createdAt', label: t('admin.created') },
  { key: 'platformAdmin', label: t('admin.platformAdmin') },
])

async function load() {
  users.value = (await iamClient.listUsers({})).users
}

async function setAdmin(u: AdminUser, admin: boolean) {
  busy.value = true
  error.value = ''
  try {
    await iamClient.setPlatformAdmin({ userId: u.id, admin })
    if (u.id === me.value?.user.id) {
      await loadMe()
      if (!admin) return router.replace('/profile')
    }
    await load()
  } catch (e) {
    if (isUnauthenticated(e)) return expire()
    error.value = errorMessage(e)
    // Re-sync so the switch reflects the server state after a rejected change.
    await load().catch(() => {})
  } finally {
    busy.value = false
  }
}

onMounted(() => load().catch((e) => (isUnauthenticated(e) ? expire() : (error.value = errorMessage(e)))))
</script>

<template>
  <p v-if="error" role="alert">{{ error }}</p>
  <DataTable :columns="columns" :rows="users" row-key="id">
    <template #cell-createdAt="{ row }">{{ row.createdAt ? d(timestampDate(row.createdAt), { dateStyle: 'short' }) : '–' }}</template>
    <template #cell-platformAdmin="{ row }">
      <Switch :model-value="row.platformAdmin" :disabled="busy" :aria-label="t('admin.platformAdmin')" @update:model-value="(v) => setAdmin(row, v)" />
    </template>
  </DataTable>
</template>
