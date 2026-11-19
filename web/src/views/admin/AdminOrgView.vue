<script setup lang="ts">
import { Button, type TableColumn } from '@asterism-dev/lumen'
import { computed, ref, watch, watchEffect } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute } from 'vue-router'

import { api } from '@/api'
import LimitFields, { type Limits } from '@/components/LimitFields.vue'
import { useCrumbs } from '@/composables/useCrumbs'
import { Card, DataTable, Section, Tag, ValueField } from '@/ui'

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
const { setCrumbs } = useCrumbs()
watchEffect(() => {
  if (org.value) setCrumbs(route.path, [{ label: org.value.name, to: route.path }])
})
const limits = ref<Limits>({ vcpu: null, ramMb: null, diskGb: null, vmCount: null, snapshotCount: null })
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
    <Section :label="t('org.identity')">
      <Card>
        <ValueField :label="t('org.displayName')" :value="org.name">
          <template v-if="org.personal" #actions><Tag>{{ t('admin.personal') }}</Tag></template>
        </ValueField>
        <ValueField :label="t('org.id')" :value="org.id" mono copy />
      </Card>
    </Section>
    <Section :label="t('nav.members')">
      <DataTable :columns="columns" :rows="org.members" row-key="id">
        <template #cell-role="{ row }"><Tag>{{ t(`role.${row.role}`) }}</Tag></template>
      </DataTable>
    </Section>
    <Section :label="t('limits.org')">
      <Card>
        <LimitFields v-model="limits" v-model:valid="valid" :disabled="busy" />
        <p><Button variant="accent" :disabled="busy || !valid" @click="save">{{ t('org.save') }}</Button></p>
      </Card>
    </Section>
  </template>
</template>
