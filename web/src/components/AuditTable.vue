<script setup lang="ts">
import { Button, TextInput, type TableColumn } from '@asterism-dev/lumen'
import type { MessageInitShape } from '@bufbuild/protobuf'
import { timestampFromDate } from '@bufbuild/protobuf/wkt'
import { Code, ConnectError, createClient } from '@connectrpc/connect'
import { createConnectTransport } from '@connectrpc/connect-web'
import { computed, onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'

import { toAuditRow, type AuditRow } from '@/composables/useAudit'
import { useMe } from '@/composables/useMe'
import { AuditService, type EventFilterSchema } from '@/gen/observatory/audit/v1/audit_pb'
import { DataTable, FilterBar, Tag } from '@/ui'

// Without org the table shows the platform-wide trail.
const props = defineProps<{ org?: string }>()
const { t, d } = useI18n()
const { expire } = useMe()

const client = createClient(
  AuditService,
  createConnectTransport({
    baseUrl: '/api/audit/v1',
    fetch: (input, init) => fetch(input, { ...init, credentials: 'include' }),
    // Connect unary calls are POSTs, which the edge refuses without the CSRF header.
    interceptors: [
      (next) => (req) => {
        req.header.set('X-Observatory', '1')
        return next(req)
      },
    ],
  }),
)

const entries = ref<AuditRow[]>([])
const next = ref('')
const error = ref('')
const actor = ref('')
const action = ref('')
const from = ref('')
const to = ref('')

const columns = computed<TableColumn[]>(() => [
  { key: 'at', label: t('audit.time') },
  { key: 'actorId', label: t('audit.actor') },
  { key: 'org', label: t('audit.org') },
  { key: 'action', label: t('audit.action') },
  { key: 'resource', label: t('audit.resource') },
  { key: 'result', label: t('audit.result') },
])

const tones = { allowed: 'success', denied: 'warning', failed: 'danger', '': undefined } as const

let applied: MessageInitShape<typeof EventFilterSchema> = {}

async function load(more: boolean) {
  try {
    const filter = more
      ? applied
      : {
          actorId: actor.value,
          type: action.value,
          from: from.value ? timestampFromDate(new Date(from.value)) : undefined,
          to: to.value ? timestampFromDate(new Date(to.value)) : undefined,
        }
    const req = { filter, pageToken: more ? next.value : '' }
    const res = props.org ? await client.listEvents({ ...req, org: props.org }) : await client.listAllEvents(req)
    applied = filter
    const rows = res.events.map(toAuditRow)
    entries.value = more ? [...entries.value, ...rows] : rows
    next.value = res.nextPageToken
    error.value = ''
  } catch (e) {
    if (e instanceof ConnectError && e.code === Code.Unauthenticated) return expire()
    error.value = e instanceof ConnectError ? e.rawMessage : e instanceof Error ? e.message : String(e)
  }
}

onMounted(() => load(false))
</script>

<template>
  <form @submit.prevent="load(false)">
    <FilterBar>
      <TextInput v-model="actor" :placeholder="t('audit.actor')" :aria-label="t('audit.actor')" />
      <TextInput v-model="action" :placeholder="t('audit.action')" :aria-label="t('audit.action')" />
      <label>{{ t('audit.from') }} <input v-model="from" type="datetime-local" /></label>
      <label>{{ t('audit.to') }} <input v-model="to" type="datetime-local" /></label>
      <template #end><Button type="submit" variant="accent">{{ t('audit.filter') }}</Button></template>
    </FilterBar>
  </form>
  <p v-if="error" role="alert">{{ error }}</p>
  <DataTable :columns="columns" :rows="entries" row-key="id">
    <template #cell-at="{ row }">{{ d(row.at, { dateStyle: 'short', timeStyle: 'medium' }) }}</template>
    <template #cell-actorId="{ row }"><span class="mono">{{ row.actorKind }}:{{ row.actorId }}</span></template>
    <template #cell-org="{ row }"><span class="mono">{{ row.org }}</span></template>
    <template #cell-action="{ row }"><span class="mono">{{ row.action }}</span></template>
    <template #cell-result="{ row }"><Tag :tone="tones[row.result as AuditRow['result']]">{{ row.result }}</Tag></template>
    <template #empty>{{ t('audit.empty') }}</template>
  </DataTable>
  <div v-if="next"><Button @click="load(true)">{{ t('audit.loadMore') }}</Button></div>
</template>
