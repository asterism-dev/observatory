<script setup lang="ts">
import { Button } from '@asterism-dev/lumen'
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import LimitFields, { type Limits } from '@/components/LimitFields.vue'
import { useOrg } from '@/composables/useOrg'
import { Card, Section } from '@/ui'

interface Named {
  id: string
  name: string
}
interface LimitsResponse {
  org: Limits
  teams: Record<string, Limits>
  members: Record<string, Limits>
}

const unlimited: Limits = { vcpu: null, ramMb: null, diskGb: null, vmCount: null, snapshotCount: null }

const { t } = useI18n()
const { org, can } = useOrg()

const orgLimits = ref<Limits>(unlimited)
const teams = ref<(Named & { limits: Limits; valid: boolean })[]>([])
const members = ref<(Named & { limits: Limits; valid: boolean })[]>([])
const busy = ref('')
const error = ref('')

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  const base = `/api/orgs/${id}`
  try {
    const [ls, ts, ms] = await Promise.all([
      api<LimitsResponse>(`${base}/limits`),
      api<Named[]>(`${base}/teams`).catch(() => []),
      api<{ id: string; name: string; email: string }[]>(`${base}/members`).catch(() => []),
    ])
    // Org switched while loading.
    if (id !== org.value?.id) return
    orgLimits.value = ls.org
    teams.value = ts.map((x) => ({ id: x.id, name: x.name, limits: ls.teams[x.id] ?? { ...unlimited }, valid: true }))
    members.value = ms.map((x) => ({ id: x.id, name: x.name || x.email, limits: ls.members[x.id] ?? { ...unlimited }, valid: true }))
  } catch (e) {
    if (id === org.value?.id) error.value = e instanceof Error ? e.message : String(e)
  }
}

async function save(scope: string, limits: Limits) {
  busy.value = scope
  error.value = ''
  try {
    await api(`/api/orgs/${org.value?.id}/limits/${scope}`, { method: 'PUT', body: JSON.stringify(limits) })
    await refresh()
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
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
      <Card><LimitFields :model-value="orgLimits" disabled /></Card>
    </Section>

    <template v-for="group in [{ key: 'teams', prefix: 'team', rows: teams }, { key: 'members', prefix: 'user', rows: members }]" :key="group.key">
      <Section v-if="group.rows.length" :label="t(`limits.${group.key}`)">
        <Card v-for="row in group.rows" :key="row.id">
          <strong>{{ row.name }}</strong>
          <LimitFields v-model="row.limits" v-model:valid="row.valid" :disabled="!can('limit:set') || !!busy" />
          <p v-if="can('limit:set')">
            <Button variant="accent" :disabled="!!busy || !row.valid" @click="save(`${group.prefix}:${row.id}`, row.limits)">
              {{ t('org.save') }}
            </Button>
          </p>
        </Card>
      </Section>
    </template>
  </template>
</template>
