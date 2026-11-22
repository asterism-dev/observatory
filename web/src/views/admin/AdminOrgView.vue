<script setup lang="ts">
import { Button, type TableColumn } from '@asterism-dev/lumen'
import { computed, ref, watch, watchEffect } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute } from 'vue-router'

import LimitFields, { type Limits } from '@/components/LimitFields.vue'
import { errorMessage, iamClient, isUnauthenticated } from '@/composables/iamClient'
import { quotaClient } from '@/composables/quotaClient'
import { fromQuota, toQuota } from '@/composables/quotaLimits'
import { useCrumbs } from '@/composables/useCrumbs'
import { useMe } from '@/composables/useMe'
import { Card, DataTable, Section, Tag, ValueField } from '@/ui'

type AdminOrg = Awaited<ReturnType<typeof iamClient.getAnyOrg>>

const { t } = useI18n()
const route = useRoute()
const { expire } = useMe()
const org = ref<{ id: string; name: string; personal: boolean; members: AdminOrg['members'] } | null>(null)
const { setCrumbs } = useCrumbs()
watchEffect(() => {
  if (org.value) setCrumbs(route.path, [{ label: org.value.name, to: route.path }])
})
const limits = ref<Limits>(fromQuota(undefined))
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
    const [res, orgLimits] = await Promise.all([iamClient.getAnyOrg({ orgId: id }), quotaClient.getOrgLimits({ orgId: id })])
    if (id !== route.params.orgId) return
    org.value = { id: res.org!.id, name: res.org!.name, personal: res.org!.personal, members: res.members }
    limits.value = fromQuota(orgLimits.limits?.org)
    error.value = ''
  } catch (e) {
    if (isUnauthenticated(e)) return expire()
    if (id === route.params.orgId) error.value = errorMessage(e)
  }
}

async function save() {
  if (!org.value) return
  busy.value = true
  error.value = ''
  try {
    await quotaClient.updateOrgLimits({ orgId: org.value.id, limits: toQuota(limits.value) })
    await load()
  } catch (e) {
    if (isUnauthenticated(e)) return expire()
    error.value = errorMessage(e)
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
        <LimitFields v-model="limits" v-model:valid="valid" :disabled="busy" image-storage />
        <p><Button variant="accent" :disabled="busy || !valid" @click="save">{{ t('org.save') }}</Button></p>
      </Card>
    </Section>
  </template>
</template>
