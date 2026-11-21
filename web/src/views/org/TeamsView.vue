<script setup lang="ts">
import { Button, TextInput } from '@asterism-dev/lumen'
import { ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { errorMessage as message, iamClient } from '@/composables/iamClient'
import { useOrg } from '@/composables/useOrg'
import type { TeamInfo } from '@/gen/observatory/iam/v1/iam_pb'
import { Card, ResourceList, ResourceRow, Section } from '@/ui'

type Team = TeamInfo

const { t } = useI18n()
const { org, can, reload } = useOrg()

const teams = ref<Team[]>([])
const name = ref('')
const busy = ref(false)
const error = ref('')

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const res = (await iamClient.listTeams({ org: id })).teams
    // Org switched while loading.
    if (id === org.value?.id) teams.value = res
  } catch (e) {
    if (id === org.value?.id) error.value = message(e)
  }
}

async function create() {
  busy.value = true
  error.value = ''
  try {
    await iamClient.createTeam({ org: org.value?.id, name: name.value.trim() })
    name.value = ''
    await reload()
    await refresh()
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = false
  }
}

watch(
  () => org.value?.id,
  () => {
    error.value = ''
    teams.value = []
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <p v-if="error" role="alert">{{ error }}</p>
    <Section :label="t('nav.teams')">
      <Card v-if="can('iam.teams.create')">
        <form class="row" @submit.prevent="create">
          <TextInput v-model="name" :placeholder="t('teams.name')" :aria-label="t('teams.name')" />
          <Button type="submit" variant="accent" :disabled="busy || !name.trim()">{{ t('teams.create') }}</Button>
        </form>
      </Card>
      <ResourceList :count="teams.length" :empty="t('teams.empty')">
        <ResourceRow v-for="team in teams" :key="team.id" :title="team.name" :to="`/orgs/${org.id}/teams/${team.id}`">
          {{ t('teams.memberCount', team.memberCount) }}
        </ResourceRow>
      </ResourceList>
    </Section>
  </template>
</template>

<style scoped>
.row {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}
</style>
