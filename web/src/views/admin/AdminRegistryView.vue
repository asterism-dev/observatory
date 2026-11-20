<script setup lang="ts">
import { Button } from '@asterism-dev/lumen'
import { onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'

import { api, ApiError } from '@/api'
import { Card, Section, Tag, ValueField } from '@/ui'

interface GCStatus {
  state: 'open' | 'draining' | 'closed' | 'collecting'
  schedule: string
  next?: string
  last?: { startedAt: string; endedAt: string; summary?: string; error?: string }
}

const { t, d } = useI18n()
const status = ref<GCStatus | null>(null)
const unmanaged = ref(false)
const busy = ref(false)
const error = ref('')
let timer: ReturnType<typeof setInterval> | undefined

async function load() {
  try {
    status.value = await api<GCStatus>('/api/admin/registry-gc')
    error.value = ''
  } catch (e) {
    if (e instanceof ApiError && e.status === 404) unmanaged.value = true
    else error.value = e instanceof Error ? e.message : String(e)
  }
}

async function run() {
  busy.value = true
  error.value = ''
  try {
    status.value = await api<GCStatus>('/api/admin/registry-gc', { method: 'POST' })
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  } finally {
    busy.value = false
  }
}

const when = (s: string) => d(new Date(s), { dateStyle: 'short', timeStyle: 'short' })

onMounted(() => {
  load()
  timer = setInterval(() => !unmanaged.value && load(), 10_000)
})
onUnmounted(() => clearInterval(timer))
</script>

<template>
  <p v-if="error" role="alert">{{ error }}</p>
  <p v-if="unmanaged">{{ t('registryGC.unmanaged') }}</p>
  <Section v-if="status" :label="t('registryGC.title')">
    <Card>
      <p>{{ t('registryGC.explain') }}</p>
      <ValueField :label="t('registryGC.state')" :value="t(`registryGC.states.${status.state}`)" />
      <ValueField :label="t('registryGC.schedule')" :value="status.schedule ? t('registryGC.daily', { time: status.schedule }) : t('registryGC.manual')" />
      <ValueField v-if="status.next" :label="t('registryGC.next')" :value="when(status.next)" />
      <ValueField v-if="status.last" :label="t('registryGC.last')" :value="when(status.last.endedAt)">
        <template #actions>
          <Tag v-if="status.last.error" tone="danger">{{ t('registryGC.failed') }}</Tag>
          <Tag v-else tone="success">{{ t('registryGC.ok') }}</Tag>
        </template>
      </ValueField>
      <ValueField v-if="status.last?.error" :label="t('registryGC.error')" :value="status.last.error" mono />
      <ValueField v-if="status.last?.summary" :label="t('registryGC.summary')" :value="status.last.summary" mono />
      <p>
        <Button variant="accent" :disabled="busy || status.state !== 'open'" @click="run">{{ t('registryGC.run') }}</Button>
      </p>
    </Card>
  </Section>
</template>
