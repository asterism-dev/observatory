<script setup lang="ts">
import { Button, type TableColumn } from '@asterism-dev/lumen'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api, ApiError } from '@/api'
import { formatValue } from '@/composables/telemetry'
import { DataTable, Section } from '@/ui'

interface Process {
  pid: number
  ppid: number
  user: string
  command: string
  state: string
  cpuPercent: number
  rssBytes: number
}

const props = defineProps<{ orgId: string; vmId: string; running: boolean }>()

const { t } = useI18n()
const processes = ref<Process[]>([])
const paused = ref(false)
const notRunning = ref(false)
const error = ref('')
let generation = 0

const columns = computed<TableColumn[]>(() => [
  { key: 'pid', label: t('processes.pid'), align: 'right' },
  { key: 'command', label: t('processes.command') },
  { key: 'cpuPercent', label: t('processes.cpu'), align: 'right' },
  { key: 'rssBytes', label: t('processes.memory'), align: 'right' },
  { key: 'user', label: t('processes.user') },
])

async function load() {
  const gen = ++generation
  try {
    const res = await api<{ processes: Process[] }>(`/api/orgs/${props.orgId}/vms/${props.vmId}/processes`)
    if (gen !== generation) return
    processes.value = res.processes
    notRunning.value = false
    error.value = ''
  } catch (e) {
    if (gen !== generation) return
    notRunning.value = e instanceof ApiError && e.status === 409
    error.value = notRunning.value ? '' : e instanceof Error ? e.message : String(e)
  }
}

const refresh = () => props.running && !paused.value && document.visibilityState === 'visible' && load()
const timer = window.setInterval(refresh, 5000)
document.addEventListener('visibilitychange', refresh)
onBeforeUnmount(() => {
  window.clearInterval(timer)
  document.removeEventListener('visibilitychange', refresh)
  generation++
})

watch(
  () => [props.orgId, props.vmId],
  () => {
    processes.value = []
    error.value = ''
  },
)
watch([() => props.orgId, () => props.vmId, () => props.running, paused], refresh, { immediate: true })
</script>

<template>
  <Section :label="t('processes.title')">
    <template #actions>
      <Button v-if="running" variant="ghost" size="sm" class="live mono" :class="{ paused }" @click="paused = !paused">
        {{ paused ? `❚❚ ${t('telemetry.paused')}` : `● ${t('telemetry.live')}` }}
      </Button>
    </template>
    <p v-if="!running || notRunning" class="muted">{{ t('processes.notRunning') }}</p>
    <template v-else>
      <p v-if="error" role="alert">{{ error }}</p>
      <div class="procs">
        <DataTable :columns="columns" :rows="processes as unknown as Record<string, unknown>[]" row-key="pid">
          <template #cell-command="{ row }"><span class="mono cmd" :title="String(row.command)">{{ row.command }}</span></template>
          <template #cell-cpuPercent="{ row }"><span class="mono">{{ (row.cpuPercent as number).toFixed(1) }}</span></template>
          <template #cell-rssBytes="{ row }"><span class="mono">{{ formatValue(row.rssBytes as number, 'By') }}</span></template>
          <template #cell-pid="{ row }"><span class="mono">{{ row.pid }}</span></template>
          <template #empty>{{ t('processes.empty') }}</template>
        </DataTable>
      </div>
      <p class="muted small">{{ t('processes.cpuHint') }}</p>
    </template>
  </Section>
</template>

<style scoped>
.live {
  color: var(--success);
  font-size: 0.7rem;
  letter-spacing: 0.08em;
  text-transform: uppercase;
}
.live.paused {
  color: var(--muted);
}
.procs {
  max-height: 360px;
  overflow: auto;
}
.cmd {
  display: block;
  max-width: 48ch;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.small {
  font-size: 0.75rem;
}
p {
  margin: 0;
}
</style>
