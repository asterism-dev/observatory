<script setup lang="ts">
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'

import { api } from '@/api'
import { gb, liveUsage, type VmInfo } from '@/composables/nodes'
import type { Series, Usage } from '@/composables/telemetry'
import { Card, Section } from '@/ui'

const props = defineProps<{ orgId: string; vm: VmInfo }>()
const { t } = useI18n()
const usage = ref<Usage | null>(null)
const error = ref('')

const W = 300
const H = 100
const windowMs = 10 * 60e3
const now = ref(Date.now())
const last = computed(() => props.vm.usage)
const live = computed(() => liveUsage(props.vm, now.value).cpu !== null)

// Sums the series per timestamp, as cpu comes split by state.
function line(series: Series[] | undefined, max: number) {
  const ts = usage.value?.t ?? []
  if (!series?.length || !max) return ''
  const from = now.value - windowMs
  const points: string[] = []
  ts.forEach((at, i) => {
    const vals = series.map((s) => s.values[i]).filter((v): v is number => v != null)
    if (!vals.length) return
    const v = vals.reduce((a, b) => a + b, 0)
    points.push(`${(((at * 1000 - from) / windowMs) * W).toFixed(1)},${(H - (Math.min(v, max) / max) * (H - 2) - 1).toFixed(1)}`)
  })
  return points.length > 1 ? points.join(' ') : ''
}

const cards = computed(() => {
  const l = last.value
  const p = liveUsage(props.vm, now.value)
  const c = usage.value?.charts
  return [
    { key: 'cpu', label: t('usage.cpu'), detail: l ? `${((l.cpuPercent / 100) * props.vm.cpus).toFixed(1)} / ${props.vm.cpus} vCPU` : '', percent: p.cpu, points: line(c?.cpu, 100) },
    { key: 'mem', label: t('usage.mem'), detail: l ? `${gb(l.memUsedMB)} / ${gb(l.memTotalMB)} GB` : '', percent: p.mem, points: line(c?.mem?.filter((s) => s.name === 'used'), (l?.memTotalMB ?? 0) * 2 ** 20) },
    { key: 'dsk', label: t('usage.dsk'), detail: l ? `${gb(l.diskUsedMB)} / ${gb(l.diskTotalMB)} GB` : '', percent: p.dsk, points: line(c?.disk?.filter((s) => s.name === '/'), 100) },
  ]
})

async function load() {
  const id = props.vm.id
  const to = new Date()
  const q = new URLSearchParams({ from: new Date(to.getTime() - windowMs).toISOString(), to: to.toISOString() })
  try {
    const res = await api<Usage>(`/api/orgs/${props.orgId}/vms/${id}/usage?${q}`)
    if (id !== props.vm.id) return
    usage.value = res
    now.value = to.getTime()
    error.value = ''
  } catch (e) {
    error.value = e instanceof Error ? e.message : String(e)
  }
}

const refresh = () => document.visibilityState === 'visible' && load()
const timer = window.setInterval(refresh, 10000)
document.addEventListener('visibilitychange', refresh)
onBeforeUnmount(() => {
  window.clearInterval(timer)
  document.removeEventListener('visibilitychange', refresh)
})

watch(
  () => props.vm.id,
  () => {
    usage.value = null
    load()
  },
  { immediate: true },
)
</script>

<template>
  <Section :label="t('usage.window', { n: 10 })">
    <template #actions><span v-if="live" class="live mono">{{ t('usage.live') }}</span></template>
    <p v-if="error" role="alert">{{ error }}</p>
    <p v-else-if="!last" class="muted">{{ t('usage.none') }}</p>
    <p v-else-if="!live" class="muted">{{ t('usage.stale') }}</p>
    <template v-if="last">
      <div class="cards">
        <Card v-for="c in cards" :key="c.key">
          <div class="card-head">
            <div>
              <div class="mono small">{{ c.label }}</div>
              <div class="mono small muted">{{ c.detail }}</div>
            </div>
            <strong>{{ c.percent === null ? '–' : `${Math.round(c.percent)}%` }}</strong>
          </div>
          <svg :viewBox="`0 0 ${W} ${H}`" preserveAspectRatio="none" role="img" :aria-label="c.label">
            <line v-for="y in [25, 50, 75]" :key="y" x1="0" :x2="W" :y1="y" :y2="y" class="grid" />
            <polyline v-if="c.points" :points="c.points" fill="none" class="plot" vector-effect="non-scaling-stroke" />
          </svg>
          <div class="axis mono small muted">
            <span>{{ t('usage.ago', { n: 10 }) }}</span>
            <span>{{ t('usage.now') }}</span>
          </div>
        </Card>
      </div>
      <p class="mono small muted">{{ t('usage.diskHint') }}</p>
    </template>
  </Section>
</template>

<style scoped>
.live {
  font-size: 0.7rem;
  letter-spacing: 0.08em;
  text-transform: uppercase;
  color: var(--success);
}
.cards {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
  gap: 16px;
}
.card-head {
  display: flex;
  justify-content: space-between;
  align-items: flex-start;
  gap: 8px;
}
svg {
  display: block;
  width: 100%;
  height: 100px;
}
.grid {
  stroke: var(--border);
  stroke-width: 0.5;
}
.plot {
  stroke: var(--success);
  stroke-width: 1.5;
}
.axis {
  display: flex;
  justify-content: space-between;
}
.small {
  font-size: 0.7rem;
}
p {
  margin: 0;
}
</style>
