<script setup lang="ts">
import { Switch } from '@asterism-dev/lumen'
import { Code, ConnectError } from '@connectrpc/connect'
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'

import { auditClient } from '@/composables/auditClient'
import { dataEventRows, type DataEventRow } from '@/composables/useAudit'
import { useMe } from '@/composables/useMe'
import { Card, Section, ValueField } from '@/ui'

const props = defineProps<{ org: string; editable: boolean }>()
const { t } = useI18n()
const { expire } = useMe()

const rows = ref<DataEventRow[]>([])
const busy = ref(false)
const error = ref('')

async function run(call: () => Promise<{ dataEvents: Record<string, boolean> }>) {
  busy.value = true
  try {
    rows.value = dataEventRows((await call()).dataEvents)
    error.value = ''
  } catch (e) {
    if (e instanceof ConnectError && e.code === Code.Unauthenticated) return expire()
    error.value = e instanceof ConnectError ? e.rawMessage : e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}

const set = (service: string, on: boolean) => run(() => auditClient.updateSettings({ org: props.org, dataEvents: { [service]: on } }))

onMounted(() => run(() => auditClient.getSettings({ org: props.org })))
</script>

<template>
  <Section :label="t('audit.settings')" :description="t('audit.dataEvents')">
    <Card>
      <ValueField v-for="row in rows" :key="row.service" :label="row.service" :value="t('audit.dataEventsHint')">
        <template #actions>
          <Switch :model-value="row.enabled" :disabled="busy || !editable" :aria-label="`${t('audit.dataEvents')}: ${row.service}`" @update:model-value="(v) => set(row.service, v)" />
        </template>
      </ValueField>
      <p v-if="error" role="alert">{{ error }}</p>
    </Card>
  </Section>
</template>
