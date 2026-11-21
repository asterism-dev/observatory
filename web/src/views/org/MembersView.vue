<script setup lang="ts">
import { timestampDate, type Timestamp } from '@bufbuild/protobuf/wkt'
import { Button, Dialog, Select, TextInput, type TableColumn } from '@asterism-dev/lumen'
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { useRouter } from 'vue-router'

import { errorMessage as message, iamClient } from '@/composables/iamClient'
import { useMe } from '@/composables/useMe'
import { useOrg } from '@/composables/useOrg'
import type { Invite as IamInvite, Member as IamMember } from '@/gen/observatory/iam/v1/iam_pb'
import { Card, DataTable, Section, Tag } from '@/ui'

type Member = IamMember
type Invite = IamInvite

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

const canInvite = computed(() => can('iam.invites.create') && !org.value?.personal)
const roleOptions = computed(() =>
  (can('iam.members.setRole') ? ['member', 'admin', 'owner'] : ['member']).map((r) => ({ value: r, label: t(`role.${r}`) })),
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

const fmt = (v?: Timestamp) => (v ? d(timestampDate(v), { dateStyle: 'short', timeStyle: 'short' }) : '–')

async function refresh() {
  if (!org.value) return
  const id = org.value.id
  try {
    const ms = (await iamClient.listMembers({ org: id })).members
    const invs = canInvite.value ? (await iamClient.listInvites({ org: id })).invites : []
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

const setRole = (m: Member, role: string) =>
  run(async () => {
    await iamClient.updateMember({ org: org.value?.id, user: m.id, role })
    await refreshAfterChange(isSelf(m))
  })

function remove() {
  const m = pending.value
  if (!m) return
  return run(async () => {
    if (isSelf(m)) await iamClient.leaveOrg({ orgId: org.value?.id })
    else await iamClient.deleteMember({ org: org.value?.id, user: m.id })
    pending.value = null
    if (isSelf(m)) {
      await loadMe()
      await router.push('/')
    } else await refreshAfterChange(false)
  })
}

const createInvite = () =>
  run(async () => {
    const res = await iamClient.createInvite({ org: org.value?.id, role: inviteRole.value })
    link.value = res.url
    copied.value = false
    await refresh()
  })

const revokeInvite = (i: Invite) =>
  run(async () => {
    await iamClient.deleteInvite({ org: org.value?.id, invite: i.id })
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
    <p v-if="error" role="alert">{{ error }}</p>
    <Section :label="t('nav.members')">
      <DataTable :columns="columns" :rows="members" row-key="id">
        <template #cell-role="{ row }">
          <Select
            v-if="can('iam.members.setRole')"
            :model-value="row.role"
            :options="roleOptions"
            :disabled="busy"
            :aria-label="t('members.role')"
            @update:model-value="(r) => r !== row.role && setRole(row, String(r))"
          />
          <Tag v-else>{{ t(`role.${row.role}`) }}</Tag>
        </template>
        <template #cell-joinedAt="{ row }">{{ fmt(row.joinedAt) }}</template>
        <template #cell-actions="{ row }">
          <Button
            v-if="isSelf(row) ? !org.personal : can('iam.members.delete')"
            variant="danger"
            size="sm"
            @click="pending = row"
          >
            {{ isSelf(row) ? t('members.leave') : t('members.remove') }}
          </Button>
        </template>
      </DataTable>
    </Section>

    <Section v-if="canInvite" :label="t('members.invitations')">
      <Card>
        <div class="row">
          <Select v-model="inviteRole" :options="roleOptions" :aria-label="t('members.role')" />
          <Button variant="accent" :disabled="busy" @click="createInvite">{{ t('members.invite') }}</Button>
        </div>
        <div v-if="link" ref="linkBox" class="link">
          <span class="muted">{{ t('members.linkOnce') }}</span>
          <div class="row">
            <TextInput :model-value="link" :aria-label="t('members.link')" readonly @focus="($event.target as HTMLInputElement).select()" />
            <Button @click="copyLink">{{ copied ? t('members.copied') : t('members.copy') }}</Button>
          </div>
        </div>
      </Card>
      <DataTable :columns="inviteColumns" :rows="invites" row-key="id">
        <template #cell-role="{ row }"><Tag>{{ t(`role.${row.role}`) }}</Tag></template>
        <template #cell-expiresAt="{ row }">{{ fmt(row.expiresAt) }}</template>
        <template #cell-actions="{ row }">
          <Button variant="danger" size="sm" :disabled="busy" @click="revokeInvite(row)">{{ t('profile.revoke') }}</Button>
        </template>
      </DataTable>
    </Section>

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

<style scoped>
.row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
.link {
  display: grid;
  gap: 6px;
}
</style>
