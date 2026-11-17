<script setup lang="ts">
import { Badge, Button, Dialog, Select, Table, TextInput, type TableColumn } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'

import { api } from '@/api'
import { useMe } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'

interface Member {
  id: string
  email: string
  name: string
  role: string
  joinedAt: string
}
interface Invite {
  id: string
  role: string
  expiresAt: string
}

const { t, d } = useI18n()
const router = useRouter()
const { me, load: loadMe } = useMe()
const { org, can, reload } = useOrg()

const members = ref<Member[]>([])
const invites = ref<Invite[]>([])
const pending = ref<Member | null>(null)
const inviteRole = ref('member')
const link = ref('')
const linkBox = ref<HTMLElement | null>(null)
const copied = ref(false)
const busy = ref(false)
const error = ref('')

const canInvite = computed(() => can('member:invite') && !org.value?.personal)
const roleOptions = computed(() =>
  (can('member:set-role') ? ['member', 'admin', 'owner'] : ['member']).map((r) => ({ value: r, label: t(`role.${r}`) })),
)
const isSelf = (m: Member) => m.id === me.value?.user.id

const columns = computed<TableColumn[]>(() => [
  { key: 'name', label: t('members.name') },
  { key: 'email', label: t('members.email') },
  { key: 'role', label: t('members.role') },
  { key: 'joinedAt', label: t('members.joined') },
  { key: 'actions', label: '' },
])
const inviteColumns = computed<TableColumn[]>(() => [
  { key: 'role', label: t('members.role') },
  { key: 'expiresAt', label: t('members.expires') },
  { key: 'actions', label: '' },
])

const fmt = (v: string) => d(new Date(v), { dateStyle: 'short', timeStyle: 'short' })
const message = (e: unknown) => (e instanceof Error ? e.message : String(e))

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  const base = `/api/orgs/${id}`
  try {
    const ms = await api<Member[]>(`${base}/members`)
    const invs = canInvite.value ? await api<Invite[]>(`${base}/invites`) : []
    // Org switched while loading.
    if (id !== org.value?.id) return
    members.value = ms
    invites.value = invs
  } catch (e) {
    if (id === org.value?.id) error.value = message(e)
  }
}

async function refreshAfterChange(self: boolean) {
  try {
    if (self) await loadMe()
  } catch (e) {
    error.value = message(e)
  }
  await reload()
  await refresh()
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

const memberUrl = (m: Member) => `/api/orgs/${org.value?.id}/members/${m.id}`

const setRole = (m: Member, role: string) =>
  run(async () => {
    await api(memberUrl(m), { method: 'PATCH', body: JSON.stringify({ role }) })
    await refreshAfterChange(isSelf(m))
  })

function remove() {
  const m = pending.value
  if (!m) return
  return run(async () => {
    await api(memberUrl(m), { method: 'DELETE' })
    pending.value = null
    if (isSelf(m)) {
      await loadMe()
      await router.push('/')
    } else await refreshAfterChange(false)
  })
}

const createInvite = () =>
  run(async () => {
    const res = await api<{ url: string }>(`/api/orgs/${org.value?.id}/invites`, {
      method: 'POST',
      body: JSON.stringify({ role: inviteRole.value }),
    })
    link.value = res.url
    copied.value = false
    await refresh()
  })

const revokeInvite = (i: Invite) =>
  run(async () => {
    await api(`/api/orgs/${org.value?.id}/invites/${i.id}`, { method: 'DELETE' })
    await refresh()
  })

async function copyLink() {
  try {
    await navigator.clipboard.writeText(link.value)
    copied.value = true
  } catch {
    // Clipboard blocked (e.g. insecure context): select the text so the user can copy it.
    linkBox.value?.querySelector('input')?.select()
  }
}

watch(
  () => org.value?.id,
  () => {
    link.value = error.value = ''
    members.value = invites.value = []
    refresh()
  },
  { immediate: true },
)
</script>

<template>
  <template v-if="org">
    <h1>{{ t('nav.members') }}</h1>
    <p v-if="error" role="alert">{{ error }}</p>
    <Table :columns="columns" :rows="members" row-key="id">
      <template #cell-role="{ row }">
        <Select
          v-if="can('member:set-role')"
          :model-value="row.role"
          :options="roleOptions"
          :disabled="busy"
          :aria-label="t('members.role')"
          @update:model-value="(r) => r !== row.role && setRole(row, String(r))"
        />
        <Badge v-else>{{ t(`role.${row.role}`) }}</Badge>
      </template>
      <template #cell-joinedAt="{ row }">{{ fmt(row.joinedAt) }}</template>
      <template #cell-actions="{ row }">
        <Button
          v-if="isSelf(row) ? !org.personal : can('member:remove')"
          variant="danger"
          size="sm"
          @click="pending = row"
        >
          {{ isSelf(row) ? t('members.leave') : t('members.remove') }}
        </Button>
      </template>
    </Table>

    <template v-if="canInvite">
      <h2>{{ t('members.invitations') }}</h2>
      <Select v-model="inviteRole" :options="roleOptions" :aria-label="t('members.role')" />
      <Button variant="accent" :disabled="busy" @click="createInvite">{{ t('members.invite') }}</Button>
      <div v-if="link" ref="linkBox">
        <p>{{ t('members.linkOnce') }}</p>
        <TextInput :model-value="link" :aria-label="t('members.link')" readonly @focus="($event.target as HTMLInputElement).select()" />
        <Button @click="copyLink">{{ copied ? t('members.copied') : t('members.copy') }}</Button>
      </div>
      <Table :columns="inviteColumns" :rows="invites" row-key="id">
        <template #cell-role="{ row }"><Badge>{{ t(`role.${row.role}`) }}</Badge></template>
        <template #cell-expiresAt="{ row }">{{ fmt(row.expiresAt) }}</template>
        <template #cell-actions="{ row }">
          <Button variant="danger" size="sm" :disabled="busy" @click="revokeInvite(row)">{{ t('profile.revoke') }}</Button>
        </template>
      </Table>
    </template>

    <Dialog
      :open="!!pending"
      :title="pending && isSelf(pending) ? t('members.leaveTitle') : t('members.removeTitle')"
      :description="pending && isSelf(pending) ? t('members.leaveBody') : t('members.removeBody', { name: pending?.name || pending?.email })"
      @update:open="(o) => !o && !busy && (pending = null)"
    >
      <p v-if="error" role="alert">{{ error }}</p>
      <template #actions>
        <Button :disabled="busy" @click="pending = null">{{ t('profile.cancel') }}</Button>
        <Button variant="danger" :disabled="busy" @click="remove">
          {{ pending && isSelf(pending) ? t('members.leave') : t('members.remove') }}
        </Button>
      </template>
    </Dialog>
  </template>
</template>
