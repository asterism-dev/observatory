<script setup lang="ts">
import { Button, Dialog, Textarea, TextInput, type TableColumn } from '@asterism-dev/lumen'
import { Plus, User } from '@lucide/vue'
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'

import { timestampDate, type Timestamp } from '@bufbuild/protobuf/wkt'

import { api } from '@/api'
import DefaultImageSection from '@/components/DefaultImageSection.vue'
import { errorMessage, iamClient, isNotFound, isUnauthenticated } from '@/composables/iamClient'
import { useMe } from '@/composables/useMe'
import { Card, DataTable, KeyValue, KeyValueList, Page, ResourceList, ResourceRow, Section, Tag } from '@/ui'

type Session = Awaited<ReturnType<typeof iamClient.listSessions>>['sessions'][number]
type SshKey = Awaited<ReturnType<typeof iamClient.listSSHKeys>>['keys'][number]

const { t, d } = useI18n()
const { me, activeOrg, expire } = useMe()

const sessions = ref<Session[]>([])
const issuer = ref('')
const pending = ref<Session | null>(null)
const busy = ref(false)
const error = ref('')

const columns = computed<TableColumn[]>(() => [
  { key: 'kind', label: t('profile.kind') },
  { key: 'userAgent', label: t('profile.userAgent') },
  { key: 'ip', label: t('profile.ip') },
  { key: 'lastUsedAt', label: t('profile.lastUsed') },
  { key: 'expiresAt', label: t('profile.expires') },
  { key: 'actions', label: '' },
])

const keys = ref<SshKey[]>([])
const keyPending = ref<SshKey | null>(null)
const adding = ref(false)
const keyName = ref('')
const keyValue = ref('')
const keyError = ref('')

const fmt = (v?: Timestamp) => (v ? d(timestampDate(v), { dateStyle: 'short', timeStyle: 'short' }) : '–')

function fail(e: unknown) {
  if (isUnauthenticated(e)) return expire()
  error.value = errorMessage(e)
}

async function loadSessions() {
  try {
    sessions.value = (await iamClient.listSessions({})).sessions
  } catch (e) {
    fail(e)
  }
}

async function loadKeys() {
  try {
    keys.value = (await iamClient.listSSHKeys({})).keys
  } catch (e) {
    fail(e)
  }
}

function openAdd() {
  keyName.value = ''
  keyValue.value = ''
  keyError.value = ''
  adding.value = true
}

async function addKey() {
  busy.value = true
  keyError.value = ''
  try {
    await iamClient.createSSHKey({ name: keyName.value.trim(), publicKey: keyValue.value.trim() })
    adding.value = false
    await loadKeys()
  } catch (e) {
    if (isUnauthenticated(e)) return expire()
    keyError.value = errorMessage(e)
  } finally {
    busy.value = false
  }
}

async function removeKey() {
  if (!keyPending.value) return
  busy.value = true
  try {
    await iamClient.deleteSSHKey({ key: keyPending.value.id })
  } catch (e) {
    if (!isNotFound(e)) fail(e)
  } finally {
    keyPending.value = null
    busy.value = false
  }
  await loadKeys()
}

async function revoke() {
  if (!pending.value) return
  const wasCurrent = pending.value.current
  busy.value = true
  try {
    await iamClient.revokeSession({ session: pending.value.id })
    pending.value = null
    if (wasCurrent) return expire()
    await loadSessions()
  } catch (e) {
    pending.value = null
    // 404: already gone, just refresh.
    if (isNotFound(e)) await loadSessions()
    else fail(e)
  } finally {
    busy.value = false
  }
}

onMounted(async () => {
  loadSessions()
  loadKeys()
  try {
    issuer.value = (await api<{ issuer: string }>('/api/auth/config')).issuer
  } catch {
    // The note is informational; the link is simply omitted.
  }
})
</script>

<template>
  <Page :icon="User" :title="t('nav.profile')" :subtitle="t('profile.subtitle')">
    <Section :label="t('profile.account')">
      <Card>
        <KeyValueList>
          <KeyValue :label="t('members.name')">{{ me?.user.name || '–' }}</KeyValue>
          <KeyValue :label="t('members.email')">
            {{ me?.user.email }}
            <Tag v-if="me?.user.platformAdmin" strong>{{ t('profile.admin') }}</Tag>
          </KeyValue>
        </KeyValueList>
        <p class="muted">
          {{ t('profile.idpNote') }}
          <a v-if="issuer" :href="issuer" target="_blank" rel="noopener">{{ t('profile.idpLink') }}</a>
        </p>
      </Card>
    </Section>

    <DefaultImageSection v-if="activeOrg" :org-id="activeOrg.id" level="user" editable :description="t('images.userDefaultHint', { org: activeOrg.name })" />

    <Section :label="t('profile.sessions')">
      <p v-if="error" role="alert">{{ error }}</p>
      <DataTable :columns="columns" :rows="sessions" row-key="id">
        <template #cell-kind="{ row }">
          <Tag>{{ row.kind }}</Tag>
          <Tag v-if="row.current" tone="success">{{ t('profile.current') }}</Tag>
        </template>
        <template #cell-lastUsedAt="{ row }">{{ fmt(row.lastUsedAt) }}</template>
        <template #cell-expiresAt="{ row }">{{ fmt(row.expiresAt) }}</template>
        <template #cell-actions="{ row }">
          <Button variant="danger" size="sm" @click="pending = row">{{ t('profile.revoke') }}</Button>
        </template>
      </DataTable>
    </Section>

    <Section :label="t('profile.sshKeys')">
      <template #actions>
        <Button variant="accent" size="sm" :icon="Plus" @click="openAdd">{{ t('profile.addKey') }}</Button>
      </template>
      <ResourceList :count="keys.length" :empty="t('profile.noKeys')">
        <ResourceRow v-for="k in keys" :key="k.id" :title="k.name" :meta="k.fingerprint">
          {{ t('profile.added') }} {{ fmt(k.createdAt) }}
          <template #actions>
            <Button variant="danger" size="sm" @click="keyPending = k">{{ t('profile.deleteKey') }}</Button>
          </template>
        </ResourceRow>
      </ResourceList>
    </Section>

    <Dialog
      :open="!!pending"
      :title="t('profile.revokeTitle')"
      :description="pending?.current ? t('profile.revokeCurrent') : t('profile.revokeBody')"
      @update:open="(o) => !o && !busy && (pending = null)"
    >
      <template #actions>
        <Button :disabled="busy" @click="pending = null">{{ t('profile.cancel') }}</Button>
        <Button variant="danger" :disabled="busy" @click="revoke">{{ t('profile.revoke') }}</Button>
      </template>
    </Dialog>

    <Dialog :open="adding" :title="t('profile.addKey')" @update:open="(o) => !busy && (adding = o)">
      <TextInput v-model="keyName" :aria-label="t('profile.keyName')" :placeholder="t('profile.keyName')" />
      <Textarea v-model="keyValue" spellcheck="false" :aria-label="t('profile.publicKey')" :placeholder="t('profile.publicKey')" />
      <p v-if="keyError" role="alert">{{ keyError }}</p>
      <template #actions>
        <Button :disabled="busy" @click="adding = false">{{ t('profile.cancel') }}</Button>
        <Button variant="accent" :disabled="busy || !keyName.trim() || !keyValue.trim()" @click="addKey">{{ t('profile.addKey') }}</Button>
      </template>
    </Dialog>

    <Dialog
      :open="!!keyPending"
      :title="t('profile.deleteKeyTitle')"
      :description="t('profile.deleteKeyBody')"
      @update:open="(o) => !o && !busy && (keyPending = null)"
    >
      <template #actions>
        <Button :disabled="busy" @click="keyPending = null">{{ t('profile.cancel') }}</Button>
        <Button variant="danger" :disabled="busy" @click="removeKey">{{ t('profile.deleteKey') }}</Button>
      </template>
    </Dialog>
  </Page>
</template>
