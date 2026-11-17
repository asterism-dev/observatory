<script setup lang="ts">
import { Badge, Button, Table, type TableColumn } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute } from 'vue-router'

import { api } from '@/api'
import LimitFields, { type Limits } from '@/components/LimitFields.vue'

interface AdminOrg {
  id: string
  name: string
  personal: boolean
  members: { id: string; email: string; name: string; role: string }[]
  limits: { org: Limits }
}

const { t } = useI18n()
const route = useRoute()
const org = ref<AdminOrg | null>(null)
const limits = ref<Limits>({ vcpu: null, ramMb: null, diskGb: null, vmCount: null })
const valid = ref(true)
const busy = ref(false)
const error = ref('')

const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('members.name') },
  { key: 'email', label: t('members.email') },
  { key: 'role', label: t('members.role') },
])

async function load() {
  const id = route.params.orgId as string
  try {
    const res = await api<AdminOrg>(`/api/admin/orgs/${id}`)
    if (id !== route.params.orgId) return
    org.value = res
    limits.value = res.limits.org
    error.value = ''
  } catch (e) {
    if (id === route.params.orgId) error.value = e instanceof Error ? e.message : String(e)
  }
}

async function save() {
  if (!org.value) return
  busy.value = true
  error.value = ''
  try {
    await api(`/api/admin/orgs/${org.value.id}/limits`, { method: 'PUT', body: JSON.stringify(limits.value) })
    await load()
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}

watch(() => route.params.orgId, () => {
  org.value = null
  load()
}, { immediate: true })
</script>

<template>
  <p v-if="error" role="alert">{{ error }}</p>
  <template v-if="org">
    <h1>{{ org.name }} <Badge v-if="org.personal">{{ t('admin.personal') }}</Badge></h1>
    <h2>{{ t('nav.members') }}</h2>
    <Table :columns="columns" :rows="org.members" row-key="id">
      <template #cell-role="{ row }"><Badge>{{ t(`role.${row.role}`) }}</Badge></template>
    </Table>
    <h2>{{ t('limits.org') }}</h2>
    <LimitFields v-model="limits" v-model:valid="valid" :disabled="busy" />
    <Button variant="accent" :disabled="busy || !valid" @click="save">{{ t('org.save') }}</Button>
  </template>
</template>
