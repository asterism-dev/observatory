<script setup lang="ts">
import { Switch, Table, type TableColumn } from '@asterism-dev/lumen'
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'

import { api } from '@/api'
import { useMe } from '@/composables/useMe'

interface AdminUser {
  id: string
  email: string
  name: string
  platformAdmin: boolean
  createdAt: string
}

const { t, d } = useI18n()
const router = useRouter()
const { me, load: loadMe } = useMe()
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
  users.value = await api<AdminUser[]>('/api/admin/users')
}

async function setAdmin(u: AdminUser, admin: boolean) {
  busy.value = true
  error.value = ''
  try {
    await api(`/api/admin/users/${u.id}/platform-admin`, { method: 'PUT', body: JSON.stringify({ admin }) })
    if (u.id === me.value?.user.id) {
      await loadMe()
      if (!admin) return router.replace('/profile')
    }
    await load()
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
    // Re-sync so the switch reflects the server state after a rejected change.
    await load().catch(() => {})
  } finally {
    busy.value = false
  }
}

onMounted(() => load().catch((e) => (error.value = e instanceof Error ? e.message : String(e))))
</script>

<template>
  <p v-if="error" role="alert">{{ error }}</p>
  <Table :columns="columns" :rows="users" row-key="id">
    <template #cell-createdAt="{ row }">{{ d(new Date(row.createdAt), { dateStyle: 'short' }) }}</template>
    <template #cell-platformAdmin="{ row }">
      <Switch :model-value="row.platformAdmin" :disabled="busy" :aria-label="t('admin.platformAdmin')" @update:model-value="(v) => setAdmin(row, v)" />
    </template>
  </Table>
</template>
