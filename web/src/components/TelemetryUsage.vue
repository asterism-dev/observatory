<script setup lang="ts">
import { SegmentedControl } from '@asterism-dev/lumen'
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import TimeSeriesChart from '@/components/TimeSeriesChart.vue'
import { droppedTotal, presets, rangeFor, type Preset, type Unit, type Usage, type UsageRange } from '@/composables/telemetry'
import { Section } from '@/ui'

const props = defineProps<{ url: string; noData: string; memMax?: number }>()
const emit = defineEmits<{ range: [r: UsageRange]; hover: [t: number | null] }>()

const { t } = useI18n()
const preset = ref<Preset>('6h')
const zoomed = ref<{ from: Date; to: Date } | null>(null)
const usage = ref<Usage | null>(null)
const shown = ref<UsageRange | null>(null)
const error = ref('')

const options = presets.map((p) => ({ value: p, label: p.replace(/(\d+)/, '$1 ') }))
const charts = computed(() => {
  const defs: { key: keyof Usage['charts']; unit: Unit; max?: number }[] = [
    { key: 'cpu', unit: '%', max: 100 },
    { key: 'mem', unit: 'By', max: props.memMax },
    { key: 'disk', unit: '%', max: 100 },
    { key: 'net', unit: 'By/s' },
  ]
  return defs.map((d) => ({ ...d, title: t(`telemetry.charts.${d.key}`), series: usage.value?.charts[d.key] ?? [] }))
})

const range = (): UsageRange => (zoomed.value ? { ...zoomed.value, now: false } : { ...rangeFor(preset.value, new Date()), now: true })

async function load(r: UsageRange) {
  const url = props.url
  const q = new URLSearchParams({ from: r.from.toISOString(), to: r.to.toISOString() })
  try {
    const res = await api<Usage>(`${url}?${q}`)
    if (url !== props.url) return
    usage.value = res
    shown.value = r
    error.value = ''
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

function pick(p: Preset | '') {
  if (p) preset.value = p
  zoomed.value = null
}

watch(
  [preset, zoomed, () => props.url],
  () => {
    const r = range()
    emit('range', r)
    load(r)
  },
  { immediate: true },
)

const refresh = () => document.visibilityState === 'visible' && !zoomed.value && load(range())
const timer = window.setInterval(refresh, 10000)
document.addEventListener('visibilitychange', refresh)
onBeforeUnmount(() => {
  window.clearInterval(timer)
  document.removeEventListener('visibilitychange', refresh)
})
</script>

<template>
  <Section :label="t('telemetry.usage')">
    <template #actions>
      <SegmentedControl :model-value="zoomed ? '' : preset" :options="options" size="sm" @update:model-value="pick" />
    </template>
    <p v-if="error" role="alert">{{ error }}</p>
    <p v-else-if="usage && !usage.t.length" class="muted">{{ noData }}</p>
    <p v-else class="muted small">{{ t('telemetry.zoomHint') }}</p>
    <div class="charts">
      <TimeSeriesChart
        v-for="c in charts"
        :key="c.key"
        :title="c.title"
        :unit="c.unit"
        :max="c.max"
        :t="usage?.t ?? []"
        :series="c.series"
        :from="(shown?.from.getTime() ?? 0) / 1000"
        :to="(shown?.to.getTime() ?? 0) / 1000"
        :sync-key="url"
        @zoom="(from, to) => (zoomed = { from: new Date(from * 1000), to: new Date(to * 1000) })"
        @reset="zoomed = null"
        @hover="(at) => emit('hover', at)"
      />
    </div>
    <p v-if="droppedTotal(usage?.dropped)" role="status" class="dropped">{{ t('telemetry.dropped', usage!.dropped!) }}</p>
  </Section>
</template>

<style scoped>
.charts {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(min(340px, 100%), 1fr));
  gap: 12px;
}
.small {
  font-size: 0.75rem;
}
p {
  margin: 0;
}
.dropped {
  color: var(--warning);
}
</style>
