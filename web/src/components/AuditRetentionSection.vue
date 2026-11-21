<script setup lang="ts">
import { Button, TextInput } from '@asterism-dev/lumen'
import { Code, ConnectError } from '@connectrpc/connect'
import { onMounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'

import { auditClient } from '@/composables/auditClient'
import { validRetention } from '@/composables/useAudit'
import { useMe } from '@/composables/useMe'
import { Card, Section } from '@/ui'

const { t } = useI18n()
const { expire } = useMe()
const saved = ref(0)
const days = ref('')
const busy = ref(false)
const error = ref('')

async function run(call: () => Promise<{ retentionDays: number }>) {
  busy.value = true
  try {
    saved.value = (await call()).retentionDays
    days.value = String(saved.value)
    error.value = ''
  } catch (e) {
    if (e instanceof ConnectError && e.code === Code.Unauthenticated) return expire()
    error.value = e instanceof ConnectError ? e.rawMessage : e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}

onMounted(() => run(() => auditClient.getPlatformSettings({})))
</script>

<template>
  <Section :label="t('audit.retention')">
    <Card>
      <p>{{ t('audit.retentionHint') }}</p>
      <form class="row" @submit.prevent="run(() => auditClient.updatePlatformSettings({ retentionDays: +days }))">
        <TextInput v-model="days" inputmode="numeric" :aria-label="t('audit.retention')" :disabled="busy" />
        <Button type="submit" variant="accent" :disabled="busy || !validRetention(days) || +days === saved">{{ t('org.save') }}</Button>
      </form>
      <p v-if="error" role="alert">{{ error }}</p>
    </Card>
  </Section>
</template>

<style scoped>
.row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
</style>
