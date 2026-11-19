<script setup lang="ts">
import { Button, Checkbox, Dialog, Select, TextInput, type TableColumn } from '@asterism-dev/lumen'
import { computed, ref, watch, watchEffect } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRoute, useRouter } from 'vue-router'

import { api } from '@/api'
import DefaultImageSection from '@/components/DefaultImageSection.vue'
import { useCrumbs } from '@/composables/useCrumbs'
import { useMe } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'
import { Card, DataTable, Section, Tag } from '@/ui'

interface TeamMember {
  id: string
  email: string
  name: string
  admin: boolean
}
interface Team {
  id: string
  name: string
  members: TeamMember[]
  canManageMembers: boolean
  canUpdate: boolean
  canDelete: boolean
  canLeave: boolean
}
interface OrgMember {
  id: string
  email: string
  name: string
}

const { t } = useI18n()
const route = useRoute()
const router = useRouter()
const { me } = useMe()
const { org, reload } = useOrg()

const team = ref<Team | null>(null)
const { setCrumbs } = useCrumbs()
watchEffect(() => {
  if (team.value) setCrumbs(route.path, [{ label: team.value.name, to: route.path }])
})
const orgMembers = ref<OrgMember[]>([])
const name = ref('')
const newUser = ref<string | null>(null)
const newAdmin = ref(false)
const confirmDelete = ref(false)
const busy = ref(false)
const error = ref('')

const teamId = computed(() => route.params.teamId as string)
const base = computed(() => `/api/orgs/${org.value?.id}/teams/${teamId.value}`)
const candidates = computed(() =>
  orgMembers.value
    .filter((m) => !team.value?.members.some((tm) => tm.id === m.id))
    .map((m) => ({ value: m.id, label: m.name || m.email })),
)
const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('members.name') },
  { key: 'email', label: t('members.email') },
  { key: 'admin', label: t('teams.admin') },
  { key: 'actions', label: '' },
])
const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

async function refresh() {
  if (!org.value) return
  const key = base.value
  try {
    const res = await api<Team>(key)
    // Navigated elsewhere while loading.
    if (key !== base.value) return
    team.value = res
    name.value = res.name
    // Without member:read the picker stays empty.
    orgMembers.value = res.canManageMembers
      ? await api<OrgMember[]>(`/api/orgs/${org.value.id}/members`).catch(() => [])
      : []
  } catch (e) {
    if (key === base.value) error.value = message(e)
  }
}

async function run(fn: () => Promise<unknown>) {
  busy.value = true
  error.value = ''
  try {
    await fn()
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = false
  }
}

const afterChange = async () => {
  await reload()
  await refresh()
}

const rename = () =>
  run(async () => {
    await api(base.value, { method: 'PATCH', body: JSON.stringify({ name: name.value.trim() }) })
    await afterChange()
  })

const setMember = (id: string, admin: boolean) =>
  run(async () => {
    await api(`${base.value}/members/${id}`, { method: 'PUT', body: JSON.stringify({ admin }) })
    newUser.value = null
    newAdmin.value = false
    await afterChange()
  })

const removeMember = (id: string) =>
  run(async () => {
    await api(`${base.value}/members/${id}`, { method: 'DELETE' })
    await afterChange()
  })

const deleteTeam = () =>
  run(async () => {
    await api(base.value, { method: 'DELETE' })
    confirmDelete.value = false
    await reload()
    await router.push(`/orgs/${org.value?.id}/teams`)
  })

watch(
  [() => org.value?.id, teamId],
  () => {
    error.value = ''
    team.value = null
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <p v-if="error" role="alert">{{ error }}</p>
    <template v-if="team">
      <Section v-if="team.canUpdate" :label="t('nav.general')">
        <Card>
          <form class="row" @submit.prevent="rename">
            <TextInput v-model="name" :aria-label="t('teams.name')" />
            <Button type="submit" variant="accent" :disabled="busy || !name.trim() || name.trim() === team.name">
              {{ t('org.save') }}
            </Button>
          </form>
        </Card>
      </Section>

      <Section :label="t('teams.members')">
        <Card v-if="team.canManageMembers">
          <form class="row" @submit.prevent="newUser && setMember(newUser, newAdmin)">
            <Select v-model="newUser" :options="candidates" :placeholder="t('teams.addMember')" :aria-label="t('teams.addMember')" />
            <Checkbox v-model="newAdmin">{{ t('teams.admin') }}</Checkbox>
            <Button type="submit" variant="accent" :disabled="busy || !newUser">{{ t('teams.add') }}</Button>
          </form>
        </Card>
        <DataTable :columns="columns" :rows="team.members" row-key="id">
          <template #cell-admin="{ row }"><Tag v-if="row.admin">{{ t('teams.admin') }}</Tag></template>
          <template #cell-actions="{ row }">
            <span class="row">
              <Button v-if="team?.canManageMembers" size="sm" :disabled="busy" @click="setMember(row.id, !row.admin)">
                {{ row.admin ? t('teams.demote') : t('teams.promote') }}
              </Button>
              <Button
                v-if="row.id === me?.user.id ? team?.canLeave : team?.canManageMembers"
                variant="danger"
                size="sm"
                :disabled="busy"
                @click="removeMember(row.id)"
              >
                {{ row.id === me?.user.id ? t('teams.leave') : t('members.remove') }}
              </Button>
            </span>
          </template>
          <template #empty>{{ t('teams.noMembers') }}</template>
        </DataTable>
      </Section>

      <DefaultImageSection :org-id="org.id" :level="`team/${team.id}`" :editable="team.canUpdate" :description="t('images.teamDefaultHint')" />

      <Section v-if="team.canDelete" :label="t('teams.delete')">
        <Card tone="danger">
          <span class="muted">{{ t('teams.deleteBody', { name: team.name }) }}</span>
          <p><Button variant="danger" @click="confirmDelete = true">{{ t('teams.delete') }}</Button></p>
        </Card>
      </Section>

      <Dialog
        :open="confirmDelete"
        :title="t('teams.deleteTitle')"
        :description="t('teams.deleteBody', { name: team.name })"
        @update:open="(o) => !o && !busy && (confirmDelete = false)"
      >
        <p v-if="error" role="alert">{{ error }}</p>
        <template #actions>
          <Button :disabled="busy" @click="confirmDelete = false">{{ t('profile.cancel') }}</Button>
          <Button variant="danger" :disabled="busy" @click="deleteTeam">{{ t('teams.delete') }}</Button>
        </template>
      </Dialog>
    </template>
  </template>
</template>

<style scoped>
.row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
</style>
