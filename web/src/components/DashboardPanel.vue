<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import TimeSeriesChart from '@/components/TimeSeriesChart.vue'
import { chartUnits, legendLabels, stepFor, toSeries, type InstantResult, type Panel, type RangeResult } from '@/composables/dashboards'
import { formatValue, type Series } from '@/composables/telemetry'
import { Card } from '@/ui'

// tick: bumped by the page to re-run the query for the same range.
const props = defineProps<{ orgId: string; panel: Panel; from: Date; to: Date; syncKey: string; tick?: number }>()
const emit = defineEmits<{ zoom: [from: number, to: number]; reset: [] }>()

const { t } = useI18n()
const t0 = ref<number[]>([])
const series = ref<Series[] | null>(null)
const stat = ref<{ value: string; label: string; more: number } | null>(null)
const error = ref('')
const loaded = ref(false)
let seq = 0

const unit = computed(() => chartUnits[props.panel.unit])
const empty = computed(() => loaded.value && !error.value && (props.panel.type === 'stat' ? stat.value === null : !series.value?.length))

async function load() {
  const n = ++seq
  const { query, type } = props.panel
  const url = `/api/orgs/${props.orgId}/dashboards/query`
  try {
    if (type === 'stat') {
      const res = await api<InstantResult>(url, { method: 'POST', body: JSON.stringify({ query, time: props.to.toISOString() }) })
      if (n !== seq) return
      // The first series only: a panel that wants a total says so with sum().
      const first = res.series[0]
      stat.value =
        first && first.value !== null
          ? { value: formatValue(first.value, unit.value), label: res.series.length > 1 ? (legendLabels([first.labels])[0] ?? '') : '', more: res.series.length - 1 }
          : null
    } else {
      const body = { query, from: props.from.toISOString(), to: props.to.toISOString(), step: stepFor(props.from, props.to) }
      const res = await api<RangeResult>(url, { method: 'POST', body: JSON.stringify(body) })
      if (n !== seq) return
      t0.value = res.t
      series.value = toSeries(res)
    }
    error.value = ''
  } catch (e) {
    if (n !== seq) return
    error.value = e instanceof Error ? e.message : String(e)
  }
  loaded.value = true
}

watch(() => [props.panel.query, props.panel.type, props.panel.unit, props.from, props.to, props.tick], load, { immediate: true })
</script>

<template>
  <Card v-if="error || panel.type === 'stat' || !series?.length" class="panel">
    <span class="title">{{ panel.title }}</span>
    <p v-if="error" role="alert" class="error mono">{{ error }}</p>
    <p v-else-if="empty" class="muted">{{ t('dashboards.noData') }}</p>
    <div v-else-if="stat && panel.type === 'stat'" class="stat">
      <span class="value">{{ stat.value }}</span>
      <span v-if="stat.label" class="muted mono small">{{ stat.label }} · {{ t('dashboards.moreSeries', { n: stat.more }) }}</span>
    </div>
  </Card>
  <TimeSeriesChart
    v-else-if="series"
    :key="unit"
    :title="panel.title"
    :unit="unit"
    :t="t0"
    :series="series"
    :from="from.getTime() / 1000"
    :to="to.getTime() / 1000"
    :sync-key="syncKey"
    @zoom="(a, b) => emit('zoom', a, b)"
    @reset="emit('reset')"
  />
</template>

<style scoped>
.panel {
  min-height: 120px;
}
.title {
  font-size: 0.75rem;
  letter-spacing: 0.06em;
  text-transform: uppercase;
}
.stat {
  display: grid;
  gap: 4px;
  padding-top: 8px;
}
.value {
  font-size: 2rem;
  font-weight: 600;
  font-variant-numeric: tabular-nums;
}
.small {
  font-size: 0.7rem;
}
.error {
  margin: 0;
  color: var(--danger);
  font-size: 0.75rem;
  white-space: pre-wrap;
  overflow-wrap: anywhere;
}
p {
  margin: 0;
}
</style>
