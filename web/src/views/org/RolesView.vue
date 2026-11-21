<script setup lang="ts">
import { Button, Dialog, Select, type TableColumn } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'

import { errorMessage as message, iamClient } from '@/composables/iamClient'
import { useOrg } from '@/composables/useOrg'
import type { Role as IamRole, RoleBinding as Binding } from '@/gen/observatory/iam/v1/iam_pb'
import { Card, DataTable, Section, Tag } from '@/ui'

type Role = IamRole & { rid: string }

const { t } = useI18n()
const router = useRouter()
const { org, can, reload } = useOrg()

const roles = ref<Role[]>([])
const bindings = ref<Binding[]>([])
const subjectNames = ref<Record<string, string>>({})
const subject = ref<string | null>(null)
const roleId = ref<string | null>(null)
const pendingDelete = ref<Role | null>(null)
const busy = ref(false)
const error = ref('')

const custom = computed(() => roles.value.filter((r) => !r.builtin))
const subjectOptions = computed(() => Object.entries(subjectNames.value).map(([value, label]) => ({ value, label })))
const roleOptions = computed(() => custom.value.map((r) => ({ value: r.id, label: r.name })))
const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('members.name') },
  { key: 'actions', label: '' },
])
const bindingColumns = computed<TableColumn[]>(() => [
  { key: 'subject', label: t('roles.subject') },
  { key: 'roleId', label: t('members.role') },
  { key: 'actions', label: '' },
])

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const rs = (await iamClient.listRoles({ org: id })).roles
    const bs = can('iam.roleBindings.manage') ? (await iamClient.listRoleBindings({ org: id })).bindings : []
    // Without iam.members.list or iam.teams.list the subject picker has fewer entries.
    const [ms, ts] = can('iam.roleBindings.manage')
      ? await Promise.all([
          iamClient.listMembers({ org: id }).then((r) => r.members, () => []),
          iamClient.listTeams({ org: id }).then((r) => r.teams, () => []),
        ])
      : [[], []]
    // Org switched while loading.
    if (id !== org.value?.id) return
    roles.value = rs.map((r) => ({ ...r, rid: r.key || r.id }))
    bindings.value = bs
    subjectNames.value = Object.fromEntries([
      ...ms.map((m) => [`user:${m.id}`, m.name || m.email]),
      ...ts.map((x) => [`team:${x.id}`, t('roles.teamLabel', { name: x.name })]),
    ])
  } catch (e) {
    if (id === org.value?.id) error.value = message(e)
  }
}

async function run(fn: () => Promise<unknown>) {
  busy.value = true
  error.value = ''
  try {
    await fn()
    await reload()
    await refresh()
  } catch (e) {
    error.value = message(e)
  } finally {
    busy.value = false
  }
}

const bind = () =>
  run(async () => {
    await iamClient.createRoleBinding({ org: org.value?.id, subject: subject.value ?? '', roleId: roleId.value ?? '' })
    subject.value = roleId.value = null
  })

const unbind = (b: Binding) => run(() => iamClient.deleteRoleBinding({ org: org.value?.id, binding: b.id }))

function remove() {
  const r = pendingDelete.value
  if (!r) return
  return run(async () => {
    await iamClient.deleteRole({ org: org.value?.id, role: r.id })
    pendingDelete.value = null
  })
}

watch(
  () => org.value?.id,
  () => {
    error.value = ''
    roles.value = bindings.value = []
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <p v-if="error" role="alert">{{ error }}</p>
    <Section :label="t('nav.roles')">
      <template v-if="can('iam.roles.manage')" #actions>
        <Button variant="accent" @click="router.push(`/orgs/${org!.id}/roles/new`)">{{ t('roles.create') }}</Button>
      </template>
      <DataTable :columns="columns" :rows="roles" row-key="rid">
        <template #cell-name="{ row }">
          <span class="row">{{ row.name }} <Tag v-if="row.builtin">{{ t('roles.builtin') }}</Tag></span>
        </template>
        <template #cell-actions="{ row }">
          <span class="row">
            <Button size="sm" @click="router.push(`/orgs/${org!.id}/roles/${row.rid}`)">
              {{ row.builtin || !can('iam.roles.manage') ? t('roles.view') : t('roles.edit') }}
            </Button>
            <Button v-if="!row.builtin && can('iam.roles.manage')" variant="danger" size="sm" @click="pendingDelete = row">
              {{ t('roles.delete') }}
            </Button>
          </span>
        </template>
      </DataTable>
    </Section>

    <Section v-if="can('iam.roleBindings.manage')" :label="t('roles.bindings')">
      <Card>
        <form class="row" @submit.prevent="bind">
          <Select v-model="subject" :options="subjectOptions" :placeholder="t('roles.subject')" :aria-label="t('roles.subject')" />
          <Select v-model="roleId" :options="roleOptions" :placeholder="t('members.role')" :aria-label="t('members.role')" />
          <Button type="submit" variant="accent" :disabled="busy || !subject || !roleId">{{ t('roles.bind') }}</Button>
        </form>
      </Card>
      <DataTable :columns="bindingColumns" :rows="bindings" row-key="id">
        <template #cell-subject="{ row }">{{ subjectNames[row.subject] ?? row.subject }}</template>
        <template #cell-roleId="{ row }">{{ custom.find((r) => r.id === row.roleId)?.name ?? row.roleId }}</template>
        <template #cell-actions="{ row }">
          <Button variant="danger" size="sm" :disabled="busy" @click="unbind(row)">{{ t('roles.unbind') }}</Button>
        </template>
        <template #empty>{{ t('roles.noBindings') }}</template>
      </DataTable>
    </Section>

    <Dialog
      :open="!!pendingDelete"
      :title="t('roles.deleteTitle')"
      :description="t('roles.deleteBody', { name: pendingDelete?.name })"
      @update:open="(o) => !o && !busy && (pendingDelete = null)"
    >
      <p v-if="error" role="alert">{{ error }}</p>
      <template #actions>
        <Button :disabled="busy" @click="pendingDelete = null">{{ t('profile.cancel') }}</Button>
        <Button variant="danger" :disabled="busy" @click="remove">{{ t('roles.delete') }}</Button>
      </template>
    </Dialog>
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
