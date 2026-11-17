<script setup lang="ts">
import { Badge, Button, Dialog, Select, Table, type TableColumn } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'

import { api } from '@/api'
import { useOrg } from '@/composables/useOrg'

interface Role {
  id?: string
  key?: string
  rid: string
  name: string
  builtin?: boolean
}
interface Binding {
  id: string
  subject: string
  roleId: string
}

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
const roleOptions = computed(() => custom.value.map((r) => ({ value: r.id!, label: r.name })))
const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('members.name') },
  { key: 'actions', label: '' },
])
const bindingColumns = computed<TableColumn[]>(() => [
  { key: 'subject', label: t('roles.subject') },
  { key: 'roleId', label: t('members.role') },
  { key: 'actions', label: '' },
])
const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  const base = `/api/orgs/${id}`
  try {
    const rs = await api<Omit<Role, 'rid'>[]>(`${base}/roles`)
    const bs = can('role:bind') ? await api<Binding[]>(`${base}/bindings`) : []
    // Without member:read or team:read the subject picker has fewer entries.
    const [ms, ts] = can('role:bind')
      ? await Promise.all([
          api<{ id: string; name: string; email: string }[]>(`${base}/members`).catch(() => []),
          api<{ id: string; name: string }[]>(`${base}/teams`).catch(() => []),
        ])
      : [[], []]
    // Org switched while loading.
    if (id !== org.value?.id) return
    roles.value = rs.map((r) => ({ ...r, rid: r.key ?? r.id! }))
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
    await api(`/api/orgs/${org.value?.id}/bindings`, {
      method: 'POST',
      body: JSON.stringify({ subject: subject.value, roleId: roleId.value }),
    })
    subject.value = roleId.value = null
  })

const unbind = (b: Binding) => run(() => api(`/api/orgs/${org.value?.id}/bindings/${b.id}`, { method: 'DELETE' }))

function remove() {
  const r = pendingDelete.value
  if (!r) return
  return run(async () => {
    await api(`/api/orgs/${org.value?.id}/roles/${r.id}`, { method: 'DELETE' })
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
    <Button v-if="can('role:write')" variant="accent" @click="router.push(`/orgs/${org.id}/roles/new`)">
      {{ t('roles.create') }}
    </Button>
    <Table :columns="columns" :rows="roles" row-key="rid">
      <template #cell-name="{ row }">
        {{ row.name }} <Badge v-if="row.builtin">{{ t('roles.builtin') }}</Badge>
      </template>
      <template #cell-actions="{ row }">
        <Button size="sm" @click="router.push(`/orgs/${org!.id}/roles/${row.rid}`)">
          {{ row.builtin || !can('role:write') ? t('roles.view') : t('roles.edit') }}
        </Button>
        <Button v-if="!row.builtin && can('role:write')" variant="danger" size="sm" @click="pendingDelete = row">
          {{ t('roles.delete') }}
        </Button>
      </template>
    </Table>

    <template v-if="can('role:bind')">
      <h2>{{ t('roles.bindings') }}</h2>
      <form class="row" @submit.prevent="bind">
        <Select v-model="subject" :options="subjectOptions" :placeholder="t('roles.subject')" :aria-label="t('roles.subject')" />
        <Select v-model="roleId" :options="roleOptions" :placeholder="t('members.role')" :aria-label="t('members.role')" />
        <Button type="submit" variant="accent" :disabled="busy || !subject || !roleId">{{ t('roles.bind') }}</Button>
      </form>
      <Table :columns="bindingColumns" :rows="bindings" row-key="id">
        <template #cell-subject="{ row }">{{ subjectNames[row.subject] ?? row.subject }}</template>
        <template #cell-roleId="{ row }">{{ custom.find((r) => r.id === row.roleId)?.name ?? row.roleId }}</template>
        <template #cell-actions="{ row }">
          <Button variant="danger" size="sm" :disabled="busy" @click="unbind(row)">{{ t('roles.unbind') }}</Button>
        </template>
        <template #empty>{{ t('roles.noBindings') }}</template>
      </Table>
    </template>

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
  gap: 8px;
  margin: 12px 0;
}
</style>
