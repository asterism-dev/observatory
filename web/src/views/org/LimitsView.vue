<script setup lang="ts">
import { Button } from '@asterism-dev/lumen'
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import LimitFields, { type Limits } from '@/components/LimitFields.vue'
import { errorText } from '@/composables/apiError'
import { iamClient } from '@/composables/iamClient'
import { quotaClient } from '@/composables/quotaClient'
import { fromQuota, toQuota } from '@/composables/quotaLimits'
import { useOrg } from '@/composables/useOrg'
import { Card, Section } from '@/ui'

interface Named {
  id: string
  name: string
}
const { t } = useI18n()
const { org, can } = useOrg()

const orgLimits = ref<Limits>(fromQuota(undefined))
const teams = ref<(Named & { limits: Limits; valid: boolean })[]>([])
const members = ref<(Named & { limits: Limits; valid: boolean })[]>([])
const busy = ref('')
const error = ref('')

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const [ls, ts, ms] = await Promise.all([
      quotaClient.getLimits({ org: id }).then((r) => r.limits),
      iamClient.listTeams({ org: id }).then((r) => r.teams, () => []),
      iamClient.listMembers({ org: id }).then((r) => r.members, () => []),
    ])
    // Org switched while loading.
    if (id !== org.value?.id) return
    orgLimits.value = fromQuota(ls?.org)
    teams.value = ts.map((x) => ({ id: x.id, name: x.name, limits: fromQuota(ls?.teams[x.id]), valid: true }))
    members.value = ms.map((x) => ({ id: x.id, name: x.name || x.email, limits: fromQuota(ls?.members[x.id]), valid: true }))
  } catch (e) {
    if (id === org.value?.id) error.value = errorText(e)
  }
}

async function save(scope: string, limits: Limits) {
  busy.value = scope
  error.value = ''
  try {
    await quotaClient.updateLimits({ org: org.value?.id, scope, limits: toQuota(limits) })
    await refresh()
  } catch (e) {
    error.value = errorText(e)
  } finally {
    busy.value = ''
  }
}

watch(
  () => org.value?.id,
  () => {
    error.value = ''
    teams.value = members.value = []
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <p v-if="error" role="alert">{{ error }}</p>
    <Section :label="t('limits.org')">
      <Card><LimitFields :model-value="orgLimits" disabled image-storage /></Card>
    </Section>

    <template v-for="group in [{ key: 'teams', prefix: 'team', rows: teams }, { key: 'members', prefix: 'user', rows: members }]" :key="group.key">
      <Section v-if="group.rows.length" :label="t(`limits.${group.key}`)">
        <Card v-for="row in group.rows" :key="row.id">
          <strong>{{ row.name }}</strong>
          <LimitFields v-model="row.limits" v-model:valid="row.valid" :disabled="!can('quota.limits.update') || !!busy" />
          <p v-if="can('quota.limits.update')">
            <Button variant="accent" :disabled="!!busy || !row.valid" @click="save(`${group.prefix}:${row.id}`, row.limits)">
              {{ t('org.save') }}
            </Button>
          </p>
        </Card>
      </Section>
    </template>
  </template>
</template>
